package service_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/service"
)

// writeSeriesSet writes `count` distinct single-series metrics (metric_0..metric_{count-1}),
// each with `pts` ramped samples, through the client's quorum write path.
func writeSeriesSet(t *testing.T, sc *service.StorageClient, count, pts int) []string {
	t.Helper()
	now := time.Now().UnixMilli()
	labels := map[string]string{"host": "h"}
	names := make([]string, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("metric_%d", i)
		names[i] = name
		if _, err := sc.Write(context.Background(), writeOneSeries(name, labels, ramp(now, pts))); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return names
}

// assertExactPlacement is the headline rebalance invariant: every series is physically present
// on EXACTLY its current ring replicas — no missing copy (data loss) and no extra copy
// (un-GC'd over-replication) — and every present copy is complete. `physical` is the full set
// of nodes whose disks to inspect (including any joined/removed node).
func assertExactPlacement(t *testing.T, sc *service.StorageClient, physical []*replNode, names []string, wantPts int) {
	t.Helper()
	labels := map[string]string{"host": "h"}
	for _, name := range names {
		want := map[string]bool{}
		for _, a := range sc.Replicas(name, labels) {
			want[a] = true
		}
		if len(want) != sc.ReplicationFactor() {
			t.Fatalf("%s: expected %d replicas, ring gave %d", name, sc.ReplicationFactor(), len(want))
		}
		for _, n := range physical {
			pts := pointsOnNode(t, n.db, name)
			onReplica := want[n.addr()]
			switch {
			case len(pts) > 0 && !onReplica:
				t.Errorf("%s: stale copy left on non-owner %s (%d points) — GC missed it", name, n.id, len(pts))
			case len(pts) == 0 && onReplica:
				t.Errorf("%s: missing on owner %s — data lost in migration", name, n.id)
			case onReplica && len(pts) != wantPts:
				t.Errorf("%s: owner %s has %d points, want %d (incomplete migration)", name, n.id, len(pts), wantPts)
			}
		}
	}
}

// ownedBy counts how many of the named series have addr among their ring replicas.
func ownedBy(sc *service.StorageClient, addr string, names []string) int {
	labels := map[string]string{"host": "h"}
	n := 0
	for _, name := range names {
		for _, a := range sc.Replicas(name, labels) {
			if a == addr {
				n++
				break
			}
		}
	}
	return n
}

// TestRebalance_AddNodeMigratesAndGCs proves the join path end to end: adding a node migrates
// the ranges it now owns to it, the displaced owners GC their now-un-owned copies, and no data
// is lost — the final placement is exactly the ring's replica sets. (ADR-031)
func TestRebalance_AddNodeMigratesAndGCs(t *testing.T) {
	all := startReplCluster(t, 4)
	initial, joiner := all[:3], all[3]
	sc := service.NewReplicatedStorageClient(replAddrs(initial), replOpts(3, 2, 2))
	sc.RefreshHealth()

	names := writeSeriesSet(t, sc, 40, 4)
	// Before the join the joiner owns nothing and holds nothing.
	if ownedBy(sc, joiner.addr(), names) != 0 {
		t.Fatal("joiner should own nothing before joining")
	}

	stats := sc.JoinNode(context.Background(), joiner.addr(), service.RebalanceOptions{})

	// Every series now sits on exactly its ring replicas, across all four nodes.
	assertExactPlacement(t, sc, all, names, 4)

	joinerOwns := ownedBy(sc, joiner.addr(), names)
	if joinerOwns == 0 {
		t.Skip("joiner happened to own no series for these random ports; placement invariant still verified")
	}
	if stats.Migrations == 0 {
		t.Errorf("joiner owns %d series but no migrations were recorded", joinerOwns)
	}
	if stats.SeriesDropped == 0 {
		t.Errorf("joiner took over %d series but nothing was GC'd from the displaced owners", joinerOwns)
	}
	// A read after rebalancing returns every series, complete.
	for _, name := range names {
		ss, err := sc.Query(context.Background(), nameMatcher(name), 0, 1<<62)
		if err != nil {
			t.Fatalf("post-join read %s: %v", name, err)
		}
		if len(ss) != 1 || len(ss[0].Points) != 4 {
			t.Fatalf("%s: post-join read incomplete: %d series", name, len(ss))
		}
	}
}

// TestRebalance_IncompleteJoinIsSafe proves the join is atomic with respect to promotion: if
// the joiner cannot receive its data (it is unreachable), it is NOT promoted into routing, the
// displaced owners are NOT GC'd, and no data is lost — the move simply waits for a retry.
// (ADR-031)
func TestRebalance_IncompleteJoinIsSafe(t *testing.T) {
	all := startReplCluster(t, 4)
	initial, joiner := all[:3], all[3]
	sc := service.NewReplicatedStorageClient(replAddrs(initial), replOpts(3, 2, 2))
	sc.RefreshHealth()
	names := writeSeriesSet(t, sc, 30, 4)

	joiner.down() // the joiner cannot receive any migrated data
	stats := sc.JoinNode(context.Background(), joiner.addr(), service.RebalanceOptions{})

	// The joiner stays Joining (out of routing) — never promoted with missing data.
	if st := sc.NodeState(joiner.addr()); st != "joining" {
		t.Fatalf("an unreachable joiner must stay joining, got %q", st)
	}
	// Nothing migrated, nothing GC'd.
	if stats.Migrations != 0 || stats.SeriesDropped != 0 {
		t.Errorf("a failed join must not migrate or GC, got %+v", stats)
	}
	if stats.Skipped == 0 {
		t.Error("a failed join should record skipped changes")
	}
	// Data is intact on its original owners and the joiner holds nothing.
	assertExactPlacement(t, sc, all, names, 4)
	for _, name := range names {
		if got := len(pointsOnNode(t, joiner.db, name)); got != 0 {
			t.Errorf("%s: unreachable joiner unexpectedly holds %d points", name, got)
		}
		ss, err := sc.Query(context.Background(), nameMatcher(name), 0, 1<<62)
		if err != nil || len(ss) != 1 || len(ss[0].Points) != 4 {
			t.Fatalf("%s: read incomplete after a failed join (err=%v)", name, err)
		}
	}
}

// TestRebalance_ReadsCompleteDuringJoin proves a node join never opens a read gap: a reader
// hammering the cluster throughout the migration always sees every sample. (ADR-031)
func TestRebalance_ReadsCompleteDuringJoin(t *testing.T) {
	all := startReplCluster(t, 4)
	initial, joiner := all[:3], all[3]
	sc := service.NewReplicatedStorageClient(replAddrs(initial), replOpts(3, 2, 2))
	sc.RefreshHealth()
	names := writeSeriesSet(t, sc, 24, 5)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var readErr atomic_error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, name := range names {
				ss, err := sc.Query(context.Background(), nameMatcher(name), 0, 1<<62)
				if err != nil {
					readErr.set(fmt.Errorf("read %s during join: %w", name, err))
					return
				}
				if len(ss) != 1 || len(ss[0].Points) != 5 {
					readErr.set(fmt.Errorf("read %s during join returned %d series / incomplete points", name, len(ss)))
					return
				}
			}
		}
	}()

	sc.JoinNode(context.Background(), joiner.addr(), service.RebalanceOptions{})
	close(stop)
	wg.Wait()

	if err := readErr.get(); err != nil {
		t.Fatalf("read was not complete throughout the join: %v", err)
	}
	assertExactPlacement(t, sc, all, names, 5)
}

// TestRebalance_RemoveNodeReHomes proves the leave path: a removed node's ranges re-home to the
// survivors, the leaver sheds its data, and nothing is lost — with three survivors at RF=3,
// every series ends up on all three. (ADR-031)
func TestRebalance_RemoveNodeReHomes(t *testing.T) {
	all := startReplCluster(t, 4)
	sc := service.NewReplicatedStorageClient(replAddrs(all), replOpts(3, 2, 2))
	sc.RefreshHealth()
	names := writeSeriesSet(t, sc, 40, 4)
	leaver := all[3]
	survivors := all[:3]

	if ownedBy(sc, leaver.addr(), names) == 0 {
		t.Skip("leaver happened to own no series for these random ports")
	}

	stats := sc.LeaveNode(context.Background(), leaver.addr(), service.RebalanceOptions{})

	// Three nodes remain at RF=3, so every series must sit on all three survivors, complete.
	for _, name := range names {
		for _, n := range survivors {
			if got := len(pointsOnNode(t, n.db, name)); got != 4 {
				t.Errorf("%s: survivor %s has %d points after leave, want 4 (re-home lost data)", name, n.id, got)
			}
		}
		// The leaver has shed everything it once held.
		if got := len(pointsOnNode(t, leaver.db, name)); got != 0 {
			t.Errorf("%s: leaver %s still holds %d points — not GC'd", name, leaver.id, got)
		}
	}
	if stats.Migrations == 0 {
		t.Error("leaver owned series but no migration to survivors was recorded")
	}
	// Reads still complete against the shrunk cluster.
	for _, name := range names {
		ss, err := sc.Query(context.Background(), nameMatcher(name), 0, 1<<62)
		if err != nil || len(ss) != 1 || len(ss[0].Points) != 4 {
			t.Fatalf("%s: post-leave read incomplete (err=%v)", name, err)
		}
	}
}

// TestRebalance_GCsDegradedOverReplication proves the over-replication path: writes taken while
// an owner is down land on a fallback; when the owner returns and is caught up, the fallback's
// extra copy is GC'd, leaving exactly the ring's replicas — without dropping the last copy.
// (ADR-031)
func TestRebalance_GCsDegradedOverReplication(t *testing.T) {
	all := startReplCluster(t, 4)
	sc := service.NewReplicatedStorageClient(replAddrs(all), replOpts(3, 2, 2))
	sc.RefreshHealth()

	// Find a series and an owner of it we can take down to force a degraded write onto a
	// fallback (a node that is NOT a natural owner).
	labels := map[string]string{"host": "h"}
	var name string
	var downNode *replNode
	for i := 0; i < 200; i++ {
		cand := fmt.Sprintf("series_%d", i)
		owners := sc.Replicas(cand, labels)
		if len(owners) == 3 {
			name = cand
			for _, n := range all {
				if n.addr() == owners[0] {
					downNode = n
					break
				}
			}
			break
		}
	}
	if name == "" || downNode == nil {
		t.Fatal("could not find a candidate series/owner")
	}

	// Take an owner down and write: the write replicates onto a live fallback instead.
	downNode.down()
	sc.RefreshHealth()
	now := time.Now().UnixMilli()
	if _, err := sc.Write(context.Background(), writeOneSeries(name, labels, ramp(now, 6))); err != nil {
		t.Fatalf("degraded write: %v", err)
	}

	// Bring the owner back and let a read repair it, so the natural owners all hold the series.
	downNode.up()
	sc.RefreshHealth()
	if _, err := sc.Query(context.Background(), nameMatcher(name), 0, 1<<62); err != nil {
		t.Fatalf("read: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return len(pointsOnNode(t, downNode.db, name)) == 6 })

	// Count holders before GC: the three natural owners plus the fallback ⇒ over-replicated.
	holdersBefore := 0
	for _, n := range all {
		if len(pointsOnNode(t, n.db, name)) > 0 {
			holdersBefore++
		}
	}
	if holdersBefore <= 3 {
		t.Skipf("no over-replication materialised for these random ports (holders=%d)", holdersBefore)
	}

	stats := sc.GCReturnedNode(context.Background(), downNode.addr(), service.RebalanceOptions{})
	if stats.SeriesDropped == 0 {
		t.Error("expected the fallback's over-replicated copy to be GC'd")
	}

	// Exactly the ring's three replicas hold the series now — the fallback dropped its copy,
	// and no owner lost it.
	want := map[string]bool{}
	for _, a := range sc.Replicas(name, labels) {
		want[a] = true
	}
	for _, n := range all {
		pts := len(pointsOnNode(t, n.db, name))
		if want[n.addr()] && pts != 6 {
			t.Errorf("owner %s should retain the series (6 points), has %d", n.id, pts)
		}
		if !want[n.addr()] && pts != 0 {
			t.Errorf("non-owner %s should have GC'd the series, still has %d points", n.id, pts)
		}
	}
}

// atomic_error is a tiny goroutine-safe first-error holder for the concurrent-read test.
type atomic_error struct {
	mu  sync.Mutex
	err error
}

func (a *atomic_error) set(err error) {
	a.mu.Lock()
	if a.err == nil {
		a.err = err
	}
	a.mu.Unlock()
}

func (a *atomic_error) get() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}
