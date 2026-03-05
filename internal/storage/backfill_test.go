package storage

import (
	"context"
	"testing"
	"time"
)

// queryPoints reads a metric's points from a TSDB over an all-time range.
func queryPoints(t *testing.T, db *TSDB, name string) []Point {
	t.Helper()
	ss, err := db.Query(context.Background(),
		[]LabelMatcher{{Name: "__name__", Value: name, Type: MatchEqual}}, 0, 1<<62)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, s := range ss {
		if s.Name == name {
			return s.Points
		}
	}
	return nil
}

// TestBackfillFillsInteriorGapRejectedByInOrderIngest is the core hinted-handoff
// invariant (ADR-029): a sample older than a series' last is rejected by the in-order
// ingest path (the path read-repair writes through) but accepted by Backfill, which
// inserts it in sorted position. This is exactly the interior gap read-repair cannot fix.
func TestBackfillFillsInteriorGapRejectedByInOrderIngest(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)
	defer db.Close()

	lbl := map[string]string{"h": "a"}
	if err := db.Ingest("m", lbl, 100, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.Ingest("m", lbl, 300, 3); err != nil {
		t.Fatal(err)
	}

	// Read-repair re-applies a missing point through Ingest. With the series' last at
	// 300, the interior point 200 is rejected as out-of-order — read-repair is stuck.
	if err := db.Ingest("m", lbl, 200, 2); err != nil {
		t.Fatal(err)
	}
	if got := db.OutOfOrderTotal(); got != 1 {
		t.Fatalf("interior in-order ingest should be rejected as out-of-order: got count %d, want 1", got)
	}
	if pts := queryPoints(t, db, "m"); len(pts) != 2 {
		t.Fatalf("interior point must not have been ingested: got %d points, want 2", len(pts))
	}

	// Backfill fills the same interior gap.
	applied, err := db.Backfill([]IngestSample{{Name: "m", Labels: lbl, Timestamp: 200, Value: 2}})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if applied != 1 {
		t.Fatalf("backfill applied %d, want 1", applied)
	}
	// Backfilling an out-of-order point must NOT touch the out-of-order metric.
	if got := db.OutOfOrderTotal(); got != 1 {
		t.Fatalf("backfill must not count toward out-of-order: got %d, want 1", got)
	}

	pts := queryPoints(t, db, "m")
	want := []Point{{100, 1}, {200, 2}, {300, 3}}
	if len(pts) != len(want) {
		t.Fatalf("got %d points, want %d: %+v", len(pts), len(want), pts)
	}
	for i, p := range pts {
		if p.Timestamp != want[i].Timestamp || p.Value != want[i].Value {
			t.Fatalf("point %d = %+v, want %+v (series must stay sorted)", i, p, want[i])
		}
	}
}

// TestBackfillGapFillIsIdempotent proves Backfill fills only gaps: a timestamp already
// present is left untouched (never overwritten, not double-counted), so replaying the
// same hint twice converges to the same state.
func TestBackfillGapFillIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)
	defer db.Close()

	lbl := map[string]string{"h": "a"}
	if err := db.Ingest("m", lbl, 100, 1); err != nil {
		t.Fatal(err)
	}

	// First backfill of the interior point 50 inserts it.
	if applied, err := db.Backfill([]IngestSample{{Name: "m", Labels: lbl, Timestamp: 50, Value: 5}}); err != nil || applied != 1 {
		t.Fatalf("first backfill applied=%d err=%v, want 1,nil", applied, err)
	}
	// A duplicate timestamp (even with a conflicting value) is a no-op gap-fill.
	if applied, err := db.Backfill([]IngestSample{{Name: "m", Labels: lbl, Timestamp: 50, Value: 999}}); err != nil || applied != 0 {
		t.Fatalf("duplicate backfill applied=%d err=%v, want 0,nil", applied, err)
	}
	pts := queryPoints(t, db, "m")
	want := []Point{{50, 5}, {100, 1}}
	if len(pts) != len(want) {
		t.Fatalf("got %d points, want %d: %+v", len(pts), len(want), pts)
	}
	for i, p := range pts {
		if p.Timestamp != want[i].Timestamp || p.Value != want[i].Value {
			t.Fatalf("point %d = %+v, want %+v (existing point must not be overwritten)", i, p, want[i])
		}
	}
}

// TestBackfillDurableAcrossReopen proves backfilled samples survive a process restart:
// they are logged under the WAL backfill frame and replay through the out-of-order-
// tolerant path, so the recovered head equals the pre-crash head even where backfill
// filled an interior gap and even for a series the node only ever saw via backfill.
func TestBackfillDurableAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultTSDBOptions()
	opts.WALDir = dir + "/wal"
	opts.BlockDir = dir + "/blocks"
	opts.FlushInterval = time.Hour // keep data in the WAL, force replay on reopen

	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}

	lbl := map[string]string{"h": "a"}
	if err := db.Ingest("live", lbl, 100, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.Ingest("live", lbl, 300, 3); err != nil {
		t.Fatal(err)
	}
	// Interior backfill on the live series + a series only ever seen via backfill.
	if _, err := db.Backfill([]IngestSample{
		{Name: "live", Labels: lbl, Timestamp: 200, Value: 2},
		{Name: "born_backfilled", Labels: lbl, Timestamp: 500, Value: 9},
	}); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: WAL replay must reconstruct both series, including the interior fill.
	db2, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	if pts := queryPoints(t, db2, "live"); len(pts) != 3 ||
		pts[0].Timestamp != 100 || pts[1].Timestamp != 200 || pts[2].Timestamp != 300 {
		t.Fatalf("live series not recovered with interior fill: %+v", pts)
	}
	if pts := queryPoints(t, db2, "born_backfilled"); len(pts) != 1 || pts[0].Timestamp != 500 || pts[0].Value != 9 {
		t.Fatalf("backfill-only series not recovered: %+v", pts)
	}
}

// TestWALBackfillFrameRoutesToHandleBackfill proves the WAL replays live sample frames
// and backfill frames through distinct handler methods, so the in-order policy of
// ADR-015 stays scoped to live frames.
func TestWALBackfillFrameRoutesToHandleBackfill(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.LogSeries(1, "m", map[string]string{"h": "a"}); err != nil {
		t.Fatal(err)
	}
	if err := w.LogSamples([]Sample{{SeriesID: 1, Timestamp: 100, Value: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := w.LogBackfillSamples([]Sample{{SeriesID: 1, Timestamp: 50, Value: 5}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w2, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	h := &testWALHandler{}
	if err := w2.Replay(h); err != nil {
		t.Fatal(err)
	}

	if len(h.samples) != 1 || h.samples[0].Timestamp != 100 {
		t.Fatalf("live sample frame should route to HandleSamples, got %+v", h.samples)
	}
	if len(h.backfill) != 1 || h.backfill[0].Timestamp != 50 {
		t.Fatalf("backfill frame should route to HandleBackfill, got %+v", h.backfill)
	}
}
