package storage

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"
)

// testHash mirrors cluster.HashKey (SHA-256 prefix) so the digest's series
// classification is exercised with the same hash writes are routed by, without the
// storage package depending on cluster.
func testHash(key string) uint64 {
	h := sha256.Sum256([]byte(key))
	return binary.BigEndian.Uint64(h[:8])
}

// allRanges is the whole ring (Lo == Hi ⇒ every position).
var allRanges = []HashRange{{Lo: 0, Hi: 0}}

func backfillAll(t *testing.T, db *TSDB, samples []IngestSample) {
	t.Helper()
	if _, err := db.Backfill(samples); err != nil {
		t.Fatalf("backfill: %v", err)
	}
}

func leavesByStart(d MerkleDigest) map[int64]WindowDigest {
	m := make(map[int64]WindowDigest, len(d.Leaves))
	for _, l := range d.Leaves {
		m[l.Start] = l
	}
	return m
}

func TestFloorWindow(t *testing.T) {
	cases := []struct{ ts, window, want int64 }{
		{0, 100, 0},
		{50, 100, 0},
		{100, 100, 100},
		{199, 100, 100},
		{-1, 100, -100},
		{-100, 100, -100},
		{-101, 100, -200},
	}
	for _, c := range cases {
		if got := floorWindow(c.ts, c.window); got != c.want {
			t.Errorf("floorWindow(%d,%d)=%d, want %d", c.ts, c.window, got, c.want)
		}
	}
}

// TestRangeDigestIdenticalDataEqualRoot is the converged case anti-entropy must
// recognise: two nodes holding byte-identical data produce the same Merkle root, so the
// comparison stops with no transfer.
func TestRangeDigestIdenticalDataEqualRoot(t *testing.T) {
	dbA := openTestDB(t, t.TempDir())
	defer dbA.Close()
	dbB := openTestDB(t, t.TempDir())
	defer dbB.Close()

	samples := []IngestSample{
		{Name: "cpu", Labels: map[string]string{"h": "a"}, Timestamp: 10, Value: 1},
		{Name: "cpu", Labels: map[string]string{"h": "a"}, Timestamp: 110, Value: 2},
		{Name: "cpu", Labels: map[string]string{"h": "a"}, Timestamp: 210, Value: 3},
		{Name: "mem", Labels: map[string]string{"h": "b"}, Timestamp: 120, Value: 7},
	}
	backfillAll(t, dbA, samples)
	backfillAll(t, dbB, samples)

	da, err := dbA.RangeDigest(allRanges, 0, 1<<62, 100, testHash)
	if err != nil {
		t.Fatal(err)
	}
	db, err := dbB.RangeDigest(allRanges, 0, 1<<62, 100, testHash)
	if err != nil {
		t.Fatal(err)
	}
	if da.Root != db.Root {
		t.Fatalf("identical data must share a root: %s vs %s", da.Root, db.Root)
	}
	if len(da.Leaves) == 0 {
		t.Fatal("expected non-empty leaves")
	}
}

// TestRangeDigestDivergenceLocalised proves a single differing window changes only that
// leaf (and the root) while every other window's leaf stays equal — so anti-entropy
// transfers only the divergent window, not everything.
func TestRangeDigestDivergenceLocalised(t *testing.T) {
	dbA := openTestDB(t, t.TempDir())
	defer dbA.Close()
	dbB := openTestDB(t, t.TempDir())
	defer dbB.Close()

	base := []IngestSample{
		{Name: "cpu", Labels: map[string]string{"h": "a"}, Timestamp: 10, Value: 1},   // window 0
		{Name: "cpu", Labels: map[string]string{"h": "a"}, Timestamp: 110, Value: 2},  // window 100
		{Name: "cpu", Labels: map[string]string{"h": "a"}, Timestamp: 210, Value: 3},  // window 200
	}
	backfillAll(t, dbA, base)
	backfillAll(t, dbB, base)
	// B gains one extra point in window 100 only.
	backfillAll(t, dbB, []IngestSample{{Name: "cpu", Labels: map[string]string{"h": "a"}, Timestamp: 150, Value: 9}})

	da, _ := dbA.RangeDigest(allRanges, 0, 1<<62, 100, testHash)
	db, _ := dbB.RangeDigest(allRanges, 0, 1<<62, 100, testHash)

	if da.Root == db.Root {
		t.Fatal("roots must differ after divergence")
	}
	la, lb := leavesByStart(da), leavesByStart(db)
	for _, w := range []int64{0, 200} {
		if la[w].Hash != lb[w].Hash {
			t.Errorf("window %d should be identical, got %s vs %s", w, la[w].Hash, lb[w].Hash)
		}
	}
	if la[100].Hash == lb[100].Hash {
		t.Error("window 100 should differ")
	}
	if lb[100].Count != 2 || la[100].Count != 1 {
		t.Errorf("counts: A window100=%d (want 1), B window100=%d (want 2)", la[100].Count, lb[100].Count)
	}
}

// TestRangeDigestHashRangeFilter proves the digest and export honour the hash range: a
// series whose key hashes outside the range contributes to neither. This is what scopes
// a co-replica comparison to the data the two nodes actually share.
func TestRangeDigestHashRangeFilter(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer db.Close()

	backfillAll(t, db, []IngestSample{
		{Name: "m1", Timestamp: 10, Value: 1},
		{Name: "m2", Timestamp: 20, Value: 2},
	})

	h1 := testHash("m1")
	// (h1-1, h1] is the single position h1 — contains m1, excludes m2 (different hash).
	only1 := []HashRange{{Lo: h1 - 1, Hi: h1}}

	exported, err := db.RangeExport(only1, 0, 1<<62, testHash)
	if err != nil {
		t.Fatal(err)
	}
	if len(exported) != 1 || exported[0].Name != "m1" {
		t.Fatalf("range should export only m1, got %+v", exported)
	}

	// The single-series digest must match a digest taken over the whole ring but with
	// only m1 present — i.e. m2 truly contributes nothing.
	dFiltered, _ := db.RangeDigest(only1, 0, 1<<62, 100, testHash)

	dbOnly1 := openTestDB(t, t.TempDir())
	defer dbOnly1.Close()
	backfillAll(t, dbOnly1, []IngestSample{{Name: "m1", Timestamp: 10, Value: 1}})
	dWhole, _ := dbOnly1.RangeDigest(allRanges, 0, 1<<62, 100, testHash)

	if dFiltered.Root != dWhole.Root {
		t.Fatalf("filtered digest (%s) must equal a digest of m1 alone (%s)", dFiltered.Root, dWhole.Root)
	}
}

// TestRangeExportRoundTripsThroughBackfill proves a transfer is faithful: exporting a
// node's range and backfilling it into an empty node reproduces the source digest
// exactly, so the two converge.
func TestRangeExportRoundTripsThroughBackfill(t *testing.T) {
	src := openTestDB(t, t.TempDir())
	defer src.Close()
	dst := openTestDB(t, t.TempDir())
	defer dst.Close()

	backfillAll(t, src, []IngestSample{
		{Name: "cpu", Labels: map[string]string{"dc": "e"}, Timestamp: 10, Value: 1},
		{Name: "cpu", Labels: map[string]string{"dc": "e"}, Timestamp: 250, Value: 5},
		{Name: "rps", Labels: map[string]string{"svc": "api"}, Timestamp: 90, Value: 42},
	})

	exported, err := src.RangeExport(allRanges, 0, 1<<62, testHash)
	if err != nil {
		t.Fatal(err)
	}
	var samples []IngestSample
	for _, rs := range exported {
		labels := make(map[string]string)
		for k, v := range rs.Labels {
			if k != "__name__" {
				labels[k] = v
			}
		}
		for _, p := range rs.Points {
			samples = append(samples, IngestSample{Name: rs.Name, Labels: labels, Timestamp: p.Timestamp, Value: p.Value})
		}
	}
	backfillAll(t, dst, samples)

	ds, _ := src.RangeDigest(allRanges, 0, 1<<62, 100, testHash)
	dd, _ := dst.RangeDigest(allRanges, 0, 1<<62, 100, testHash)
	if ds.Root != dd.Root {
		t.Fatalf("destination digest %s != source %s after export+backfill", dd.Root, ds.Root)
	}
}

// TestRangeDigestEmpty proves an empty selection (no ranges, or a node with no data)
// yields the stable empty-root sentinel, so two empty sides compare equal.
func TestRangeDigestEmpty(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer db.Close()
	backfillAll(t, db, []IngestSample{{Name: "m1", Timestamp: 10, Value: 1}})

	noRanges, err := db.RangeDigest(nil, 0, 1<<62, 100, testHash)
	if err != nil {
		t.Fatal(err)
	}
	if len(noRanges.Leaves) != 0 {
		t.Fatalf("no ranges should select nothing, got %d leaves", len(noRanges.Leaves))
	}

	empty := openTestDB(t, t.TempDir())
	defer empty.Close()
	emptyDigest, _ := empty.RangeDigest(allRanges, 0, 1<<62, 100, testHash)
	if emptyDigest.Root != noRanges.Root {
		t.Fatalf("empty selections must share the sentinel root: %s vs %s", emptyDigest.Root, noRanges.Root)
	}

	if _, err := db.RangeDigest(allRanges, 0, 1<<62, 0, testHash); err == nil {
		t.Fatal("window <= 0 must error")
	}
}
