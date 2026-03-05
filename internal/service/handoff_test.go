package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/service"
)

// oneSampleSeries builds a single-series, single-sample write batch for hint-store tests.
func oneSampleSeries(name string, ts int64) []service.TimeSeries {
	return []service.TimeSeries{{
		Name:    name,
		Samples: []service.Sample{{TimestampMs: ts, Value: float64(ts)}},
	}}
}

// TestHintStore_BoundedDropsOldest proves the per-target sample cap bounds the buffer:
// past the cap the oldest hints are dropped and counted, so a long outage cannot grow
// the hint store without bound.
func TestHintStore_BoundedDropsOldest(t *testing.T) {
	store, err := service.NewHintStore(t.TempDir(), 5) // cap: 5 samples per target
	if err != nil {
		t.Fatal(err)
	}
	target := "10.0.0.1:8080"
	for i := 0; i < 8; i++ { // 8 single-sample hints against a cap of 5
		if err := store.Add(target, oneSampleSeries("m", int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.Pending(target); got != 5 {
		t.Fatalf("pending=%d, want 5 (capped)", got)
	}
	if got := store.PendingSamples(); got != 5 {
		t.Fatalf("PendingSamples=%d, want 5", got)
	}
	if got := store.Dropped(); got != 3 {
		t.Fatalf("dropped=%d, want 3 (oldest over cap)", got)
	}

	// The retained hints are the newest ones (3..7), in order.
	var seen []int64
	store.Drain(target, func(h service.Hint) bool {
		seen = append(seen, h.Series[0].Samples[0].TimestampMs)
		return true
	})
	want := []int64{3, 4, 5, 6, 7}
	if len(seen) != len(want) {
		t.Fatalf("retained %d hints %v, want %v", len(seen), seen, want)
	}
	for i, ts := range want {
		if seen[i] != ts {
			t.Fatalf("retained[%d]=%d, want %d (drop-oldest keeps the newest in order)", i, seen[i], ts)
		}
	}
}

// TestHintStore_DurableReload proves buffered hints survive a process restart: a fresh
// store opened over the same directory rebuilds the per-target backlog so replay resumes.
func TestHintStore_DurableReload(t *testing.T) {
	dir := t.TempDir()
	store, err := service.NewHintStore(dir, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	a, b := "10.0.0.1:8080", "10.0.0.2:8080"
	if err := store.Add(a, oneSampleSeries("m", 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(a, oneSampleSeries("m", 2)); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(b, oneSampleSeries("m", 3)); err != nil {
		t.Fatal(err)
	}

	reopened, err := service.NewHintStore(dir, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Pending(a); got != 2 {
		t.Fatalf("target a pending=%d after reload, want 2", got)
	}
	if got := reopened.Pending(b); got != 1 {
		t.Fatalf("target b pending=%d after reload, want 1", got)
	}
	if got := reopened.PendingRecords(); got != 3 {
		t.Fatalf("records=%d after reload, want 3", got)
	}
}

// TestHintStore_DrainFIFOAndStopOnFailure proves Drain replays in FIFO order, deletes a
// hint only on a successful send, and stops at the first failure so the rest are retained
// in order for the next pass.
func TestHintStore_DrainFIFOAndStopOnFailure(t *testing.T) {
	store, err := service.NewHintStore(t.TempDir(), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	target := "n:1"
	for i := 0; i < 3; i++ {
		if err := store.Add(target, oneSampleSeries("m", int64(i))); err != nil {
			t.Fatal(err)
		}
	}

	var order []int64
	n, full := store.Drain(target, func(h service.Hint) bool {
		order = append(order, h.Series[0].Samples[0].TimestampMs)
		return true
	})
	if n != 3 || !full {
		t.Fatalf("clean drain returned (%d,%v), want (3,true)", n, full)
	}
	if store.Pending(target) != 0 {
		t.Fatalf("pending should be 0 after a full drain, got %d", store.Pending(target))
	}
	if store.Replayed() != 3 {
		t.Fatalf("replayed counter=%d, want 3", store.Replayed())
	}
	for i, ts := range order {
		if ts != int64(i) {
			t.Fatalf("FIFO violated: order[%d]=%d", i, ts)
		}
	}

	// A failing send stops the pass and preserves every hint for a later retry.
	if err := store.Add(target, oneSampleSeries("m", 100)); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(target, oneSampleSeries("m", 101)); err != nil {
		t.Fatal(err)
	}
	n, full = store.Drain(target, func(h service.Hint) bool { return false })
	if n != 0 || full {
		t.Fatalf("failing drain returned (%d,%v), want (0,false)", n, full)
	}
	if store.Pending(target) != 2 {
		t.Fatalf("failed hints must be retained, pending=%d want 2", store.Pending(target))
	}
}

// TestHintedHandoff_ConvergesInteriorGap is the end-to-end proof: a replica down during
// a write misses it (quorum holds on the survivors and a hint accumulates); on return it
// catches up via backfill replay and fully converges — including an interior gap that
// read-repair provably cannot fill. See ADR-029.
func TestHintedHandoff_ConvergesInteriorGap(t *testing.T) {
	nodes := startReplCluster(t, 3) // N=3 == cluster: every series replicates to all three
	sc := service.NewReplicatedStorageClient(replAddrs(nodes), replOpts(3, 2, 2))
	store, err := service.NewHintStore(t.TempDir(), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	sc.SetHintStore(store)
	sc.RefreshHealth()
	ctx := context.Background()
	now := time.Now().UnixMilli()

	name := "disk_io"
	labels := map[string]string{"host": "db-1"}
	d := nodes[2] // the replica we take down

	// 1. Baseline write while all replicas are up: D holds @now.
	if _, err := sc.Write(ctx, writeOneSeries(name, labels, []service.Sample{{TimestampMs: now, Value: 1}})); err != nil {
		t.Fatalf("baseline write: %v", err)
	}
	if got := len(pointsOnNode(t, d.db, name)); got != 1 {
		t.Fatalf("D should hold the baseline point, has %d", got)
	}

	// 2. D goes down. The down-window write reaches quorum on the survivors and buffers
	//    a hint for D, which stays stale.
	d.down()
	sc.RefreshHealth()
	downWindow := []service.Sample{
		{TimestampMs: now + 1000, Value: 2},
		{TimestampMs: now + 2000, Value: 3},
		{TimestampMs: now + 3000, Value: 4},
	}
	if _, err := sc.Write(ctx, writeOneSeries(name, labels, downWindow)); err != nil {
		t.Fatalf("write during outage should hold quorum: %v", err)
	}
	if store.Pending(d.addr()) == 0 {
		t.Fatal("expected hints buffered for the down replica")
	}
	if got := len(pointsOnNode(t, d.db, name)); got != 1 {
		t.Fatalf("down replica must still be stale (1 point), has %d", got)
	}

	// 3. D returns with a backlog → it must go through the catching-up (Joining) state,
	//    not straight to Active.
	d.up()
	sc.RefreshHealth()
	if st := sc.NodeState(d.addr()); st != "joining" {
		t.Fatalf("a returning replica with a backlog must be Joining, got %q", st)
	}

	// 4. Make the gap interior: a newer sample lands on D directly, so the buffered
	//    down-window points are now older than D's last. Read-repair writes through the
	//    in-order path, so re-applying an interior point there is rejected — prove it.
	if err := d.db.Ingest(name, labels, now+10000, 9); err != nil {
		t.Fatal(err)
	}
	before := d.db.OutOfOrderTotal()
	_ = d.db.Ingest(name, labels, now+1000, 2) // exactly what read-repair would attempt
	if d.db.OutOfOrderTotal() == before {
		t.Fatal("interior in-order write should be rejected as out-of-order (read-repair cannot fill it)")
	}

	// 5. Replay drains the hints through the out-of-order-tolerant backfill path and
	//    promotes the caught-up node to Active.
	sc.ReplayHintsOnce(ctx)
	if st := sc.NodeState(d.addr()); st != "active" {
		t.Fatalf("a caught-up replica must be promoted to Active, got %q", st)
	}
	if got := store.Pending(d.addr()); got != 0 {
		t.Fatalf("hints should be drained after replay, pending=%d", got)
	}

	// 6. D fully converged, interior gap and all, in sorted order.
	got := pointsOnNode(t, d.db, name)
	wantTS := []int64{now, now + 1000, now + 2000, now + 3000, now + 10000}
	if len(got) != len(wantTS) {
		t.Fatalf("D did not fully converge: got %d points %+v, want %d", len(got), got, len(wantTS))
	}
	for i, ts := range wantTS {
		if got[i].Timestamp != ts {
			t.Fatalf("converged point %d ts=%d, want %d (sorted, incl. interior fill)", i, got[i].Timestamp, ts)
		}
	}

	// And the cluster read returns the full union now that every replica is live.
	ss, err := sc.Query(ctx, nameMatcher(name), 0, 1<<62)
	if err != nil {
		t.Fatalf("cluster read after convergence: %v", err)
	}
	if len(ss) != 1 || len(ss[0].Points) != len(wantTS) {
		t.Fatalf("cluster read after convergence: got %d series / %d points, want 1 / %d",
			len(ss), seriesPointLen(ss), len(wantTS))
	}
}

// TestHintedHandoff_DisabledPreservesADR022 proves that with no hint store the client
// behaves exactly as ADR-022: a write past a down replica buffers nothing, and a
// returning replica is marked Active immediately (no catch-up state).
func TestHintedHandoff_DisabledPreservesADR022(t *testing.T) {
	nodes := startReplCluster(t, 3)
	sc := service.NewReplicatedStorageClient(replAddrs(nodes), replOpts(3, 2, 2))
	sc.RefreshHealth()
	now := time.Now().UnixMilli()

	if sc.HintStore() != nil {
		t.Fatal("hint store should be nil when hinted handoff is disabled")
	}

	nodes[2].down()
	sc.RefreshHealth()
	if _, err := sc.Write(context.Background(), writeOneSeries("m", map[string]string{"h": "a"}, ramp(now, 3))); err != nil {
		t.Fatalf("write at quorum with one down: %v", err)
	}

	nodes[2].up()
	sc.RefreshHealth()
	if st := sc.NodeState(nodes[2].addr()); st != "active" {
		t.Fatalf("without handoff a returning replica should be Active immediately, got %q", st)
	}
	// ReplayHintsOnce is a no-op with no store; it must not panic.
	sc.ReplayHintsOnce(context.Background())
}
