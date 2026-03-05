package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/service"
	"github.com/meridiandb/meridian/internal/storage"
)

func tsOf(points []storage.Point) []int64 {
	out := make([]int64, len(points))
	for i, p := range points {
		out[i] = p.Timestamp
	}
	return out
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func bf(t *testing.T, db *storage.TSDB, name string, lbl map[string]string, samples ...storage.IngestSample) {
	t.Helper()
	for i := range samples {
		samples[i].Name = name
		samples[i].Labels = lbl
	}
	if _, err := db.Backfill(samples); err != nil {
		t.Fatalf("backfill: %v", err)
	}
}

// TestAntiEntropy_ConvergesOnlyDivergentWindows is the headline anti-entropy invariant
// (ADR-030): two co-replicas that diverged in exactly one time window converge after one
// round, transferring ONLY the missing points of that window — not the windows they
// already agree on — and a second round over the now-converged cluster transfers nothing.
//
// The divergence is created by writing straight to each node's local TSDB, never through
// the client's read path, so read-repair (read-triggered) cannot be what converges them.
func TestAntiEntropy_ConvergesOnlyDivergentWindows(t *testing.T) {
	nodes := startReplCluster(t, 2)
	a, b := nodes[0], nodes[1]
	sc := service.NewReplicatedStorageClient(replAddrs(nodes), replOpts(2, 1, 2))
	sc.RefreshHealth() // both alive ⇒ both Active in the ring

	lbl := map[string]string{"h": "a"}
	// Windows 0 and 5000 are identical on both nodes — they must NOT transfer.
	for _, db := range []*storage.TSDB{a.db, b.db} {
		bf(t, db, "cpu", lbl,
			storage.IngestSample{Timestamp: 100, Value: 1},  // window 0
			storage.IngestSample{Timestamp: 5100, Value: 9}, // window 5000
		)
	}
	// Window 1000 diverges: A holds ts 1100, B holds ts 1200.
	bf(t, a.db, "cpu", lbl, storage.IngestSample{Timestamp: 1100, Value: 2})
	bf(t, b.db, "cpu", lbl, storage.IngestSample{Timestamp: 1200, Value: 3})

	opts := service.AntiEntropyOptions{Interval: time.Minute, Window: time.Second, GroupsPerRound: 8}
	sc.AntiEntropyRound(context.Background(), opts)

	want := []int64{100, 1100, 1200, 5100}
	if got := tsOf(pointsOnNode(t, a.db, "cpu")); !equalInt64(got, want) {
		t.Fatalf("node A did not converge: got %v, want %v", got, want)
	}
	if got := tsOf(pointsOnNode(t, b.db, "cpu")); !equalInt64(got, want) {
		t.Fatalf("node B did not converge: got %v, want %v", got, want)
	}

	st := sc.AntiEntropyStats()
	if st.DivergentWindows != 1 {
		t.Errorf("divergent windows = %d, want 1 (only window 1000)", st.DivergentWindows)
	}
	// Exactly two samples moved (1200→A, 1100→B), proving only the divergent window's
	// gaps transferred — not all four points.
	if st.SamplesTransferred != 2 {
		t.Errorf("samples transferred = %d, want 2 (only the missing points)", st.SamplesTransferred)
	}
	if st.Repairs != 2 {
		t.Errorf("repairs = %d, want 2 (one push per replica)", st.Repairs)
	}
	if st.BytesTransferred == 0 {
		t.Error("bytes transferred should be non-zero")
	}
	if st.Rounds != 1 {
		t.Errorf("rounds = %d, want 1", st.Rounds)
	}

	// Second round over the converged cluster transfers nothing.
	before := sc.AntiEntropyStats()
	sc.AntiEntropyRound(context.Background(), opts)
	after := sc.AntiEntropyStats()
	if after.Repairs != before.Repairs || after.SamplesTransferred != before.SamplesTransferred {
		t.Fatalf("converged cluster must not transfer: repairs %d→%d, samples %d→%d",
			before.Repairs, after.Repairs, before.SamplesTransferred, after.SamplesTransferred)
	}
	if after.DivergentWindows != before.DivergentWindows {
		t.Fatalf("no window should diverge after convergence: %d→%d", before.DivergentWindows, after.DivergentWindows)
	}
}

// TestAntiEntropy_FillsSeriesMissingOnOneReplica proves a whole series (and the windows
// only one side has) is reconciled: a window present on one replica but absent on the
// other counts as divergent and is filled bidirectionally.
func TestAntiEntropy_FillsSeriesMissingOnOneReplica(t *testing.T) {
	nodes := startReplCluster(t, 2)
	a, b := nodes[0], nodes[1]
	sc := service.NewReplicatedStorageClient(replAddrs(nodes), replOpts(2, 1, 2))
	sc.RefreshHealth()

	shared := map[string]string{"h": "a"}
	for _, db := range []*storage.TSDB{a.db, b.db} {
		bf(t, db, "cpu", shared, storage.IngestSample{Timestamp: 100, Value: 1})
	}
	// Only B holds the "rps" series, in a window neither shares for "cpu".
	bf(t, b.db, "rps", map[string]string{"svc": "api"}, storage.IngestSample{Timestamp: 100, Value: 7})

	opts := service.AntiEntropyOptions{Interval: time.Minute, Window: time.Hour, GroupsPerRound: 8}
	sc.AntiEntropyRound(context.Background(), opts)

	if got := tsOf(pointsOnNode(t, a.db, "rps")); !equalInt64(got, []int64{100}) {
		t.Fatalf("node A should have received the missing series: got %v", got)
	}
	if st := sc.AntiEntropyStats(); st.SamplesTransferred != 1 || st.Repairs != 1 {
		t.Fatalf("expected exactly one sample pushed to A: samples=%d repairs=%d", st.SamplesTransferred, st.Repairs)
	}
}

// TestAntiEntropy_SkipsWhenNoLivePeer proves the sweep does nothing for a group without
// at least two reachable replicas — there is no peer to converge against, so it neither
// fetches digests nor transfers.
func TestAntiEntropy_SkipsWhenNoLivePeer(t *testing.T) {
	nodes := startReplCluster(t, 2)
	a, b := nodes[0], nodes[1]
	sc := service.NewReplicatedStorageClient(replAddrs(nodes), replOpts(2, 1, 2))

	bf(t, a.db, "cpu", map[string]string{"h": "a"}, storage.IngestSample{Timestamp: 100, Value: 1})
	b.down()
	sc.RefreshHealth() // B unreachable ⇒ Dead

	sc.AntiEntropyRound(context.Background(), service.AntiEntropyOptions{Interval: time.Minute, Window: time.Hour, GroupsPerRound: 8})

	st := sc.AntiEntropyStats()
	if st.Rounds != 0 || st.Repairs != 0 {
		t.Fatalf("a group with one live replica must not run: rounds=%d repairs=%d", st.Rounds, st.Repairs)
	}
}
