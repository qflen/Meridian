package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestDB(t *testing.T, dir string) *TSDB {
	t.Helper()
	opts := DefaultTSDBOptions()
	opts.WALDir = filepath.Join(dir, "wal")
	opts.BlockDir = filepath.Join(dir, "blocks")
	opts.FlushInterval = time.Hour // disable the background flush loop
	db, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

// TestFlushConcurrentIngestNoLoss ingests continuously in one goroutine while Flush
// runs repeatedly in another, and asserts every sample survives — exercising the
// head-swap/WAL-rotate cut. Before the fix, samples ingested between the block
// snapshot and head.Reset() were lost from both the head and the WAL.
func TestFlushConcurrentIngestNoLoss(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)
	defer db.Close()

	const n = 5000
	lbl := map[string]string{"h": "a"}

	done := make(chan error, 1)
	go func() {
		for i := 0; i < n; i++ {
			// Strictly increasing timestamps: every sample is accepted.
			if err := db.Ingest("m", lbl, int64(i+1)*1000, float64(i)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	// Hammer Flush while ingestion is in flight.
	for i := 0; i < 25; i++ {
		if err := db.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// Seal whatever remains in the head.
	if err := db.Flush(); err != nil {
		t.Fatalf("final flush: %v", err)
	}

	results, err := db.Query(context.Background(),
		[]LabelMatcher{{Name: "__name__", Value: "m", Type: MatchEqual}}, 0, int64(n+1)*1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 series, got %d", len(results))
	}
	pts := results[0].Points
	if len(pts) != n {
		t.Fatalf("sample loss/duplication: got %d points, want %d", len(pts), n)
	}
	for i, p := range pts {
		want := int64(i+1) * 1000
		if p.Timestamp != want {
			t.Fatalf("point %d: ts=%d, want %d", i, p.Timestamp, want)
		}
	}
}

// TestOutOfOrderRejectedAndCounted verifies the reject-out-of-order policy: older
// samples are dropped and counted, an exact duplicate of the last sample is a no-op,
// a conflicting value at the same timestamp is rejected, and a flushed block records
// non-inverted time bounds.
func TestOutOfOrderRejectedAndCounted(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)
	defer db.Close()

	lbl := map[string]string{"h": "a"}
	mustIngest := func(ts int64, v float64) {
		if err := db.Ingest("m", lbl, ts, v); err != nil {
			t.Fatalf("ingest ts=%d: %v", ts, err)
		}
	}

	mustIngest(100, 1) // accepted
	mustIngest(50, 2)  // out of order
	mustIngest(200, 3) // accepted
	mustIngest(10, 4)  // out of order
	mustIngest(150, 5) // out of order (< 200)

	if got := db.OutOfOrderTotal(); got != 3 {
		t.Fatalf("out-of-order count: got %d, want 3", got)
	}

	// Exact duplicate of the last sample → deduplicated, not counted.
	mustIngest(200, 3)
	if got := db.OutOfOrderTotal(); got != 3 {
		t.Fatalf("duplicate must not count as out-of-order: got %d, want 3", got)
	}
	// Conflicting value at the last timestamp → rejected and counted.
	mustIngest(200, 99)
	if got := db.OutOfOrderTotal(); got != 4 {
		t.Fatalf("conflicting same-ts value should count: got %d, want 4", got)
	}

	// Sub-range query returns only the accepted, correctly-valued points.
	results, err := db.Query(context.Background(),
		[]LabelMatcher{{Name: "__name__", Value: "m", Type: MatchEqual}}, 0, 250)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 series, got %d", len(results))
	}
	pts := results[0].Points
	if len(pts) != 2 {
		t.Fatalf("expected 2 points, got %d: %+v", len(pts), pts)
	}
	if pts[0].Timestamp != 100 || pts[0].Value != 1 {
		t.Fatalf("point 0: %+v, want {100,1}", pts[0])
	}
	if pts[1].Timestamp != 200 || pts[1].Value != 3 {
		t.Fatalf("point 1: %+v, want {200,3} (conflict must not overwrite)", pts[1])
	}

	// A flushed block has non-inverted bounds derived by scan.
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	blocks := db.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	meta := blocks[0].Meta()
	if meta.MinTime != 100 || meta.MaxTime != 200 {
		t.Fatalf("block bounds: [%d,%d], want [100,200]", meta.MinTime, meta.MaxTime)
	}
	if meta.MinTime > meta.MaxTime {
		t.Fatal("inverted block bounds")
	}
}

// TestCrashExactlyOnce_BlockDurableWALPresent simulates a crash AFTER a block is
// durable but BEFORE its covered WAL segments are cleaned up. On reopen the block and
// the source WAL segments are both present; the low-water-mark must make replay skip
// the covered segments so the data is recovered exactly once (no double-count).
func TestCrashExactlyOnce_BlockDurableWALPresent(t *testing.T) {
	dir := t.TempDir()
	walDir := filepath.Join(dir, "wal")
	blockDir := filepath.Join(dir, "blocks")
	if err := os.MkdirAll(blockDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Log series + 50 samples to the WAL, then rotate to seal them — the rotation
	//    point is the low-water-mark a flush would record.
	w, err := OpenWAL(walDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.LogSeries(1, "m", map[string]string{"h": "a"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := w.LogSamples([]Sample{{SeriesID: 1, Timestamp: int64(i+1) * 1000, Value: float64(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	mark, err := w.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. Write a durable block holding the same data, covering <= mark. The WAL
	//    segments <= mark are deliberately left in place (cleanup "didn't run").
	h := NewHeadBlock()
	s, _ := h.GetOrCreateSeries("m", map[string]string{"h": "a"})
	for i := 0; i < 50; i++ {
		h.Ingest(s.ID, int64(i+1)*1000, float64(i))
	}
	if _, err := WriteBlock(blockDir, h, mark); err != nil {
		t.Fatal(err)
	}

	// 3. Reopen: exactly 50 points, never 100.
	opts := DefaultTSDBOptions()
	opts.WALDir = walDir
	opts.BlockDir = blockDir
	opts.FlushInterval = time.Hour
	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	results, err := db.Query(context.Background(),
		[]LabelMatcher{{Name: "__name__", Value: "m", Type: MatchEqual}}, 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 series, got %d", len(results))
	}
	if got := len(results[0].Points); got != 50 {
		t.Fatalf("exactly-once violated: got %d points, want 50", got)
	}
	// TotalSamples sums head + blocks. A double-count (block loaded AND its WAL
	// segments replayed into the head) surfaces here as 100, even though a query
	// would dedupe the identical timestamps and hide it.
	if got := db.Stats().TotalSamples; got != 50 {
		t.Fatalf("exactly-once violated: TotalSamples=%d, want 50 (100 indicates double-count)", got)
	}
}

// TestCrashExactlyOnce_BlockNotDurable simulates a crash DURING the block write
// (before the atomic rename committed it). On reopen there is no committed block, a
// leftover temp dir exists, and the data lives only in the WAL; it must be recovered
// exactly once and the stale temp dir cleaned up.
func TestCrashExactlyOnce_BlockNotDurable(t *testing.T) {
	dir := t.TempDir()
	walDir := filepath.Join(dir, "wal")
	blockDir := filepath.Join(dir, "blocks")
	if err := os.MkdirAll(blockDir, 0o755); err != nil {
		t.Fatal(err)
	}

	w, err := OpenWAL(walDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.LogSeries(1, "m", map[string]string{"h": "a"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := w.LogSamples([]Sample{{SeriesID: 1, Timestamp: int64(i+1) * 1000, Value: float64(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	// The flush cut rotated the WAL but the block never committed.
	if _, err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Leftover temp block dir from the interrupted, never-renamed write.
	staleTmp := filepath.Join(blockDir, ".01HSTALEXXXXXXXXXXXXXXXXXX.tmp")
	if err := os.MkdirAll(filepath.Join(staleTmp, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}

	opts := DefaultTSDBOptions()
	opts.WALDir = walDir
	opts.BlockDir = blockDir
	opts.FlushInterval = time.Hour
	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if len(db.Blocks()) != 0 {
		t.Fatalf("expected no committed blocks, got %d", len(db.Blocks()))
	}
	results, err := db.Query(context.Background(),
		[]LabelMatcher{{Name: "__name__", Value: "m", Type: MatchEqual}}, 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || len(results[0].Points) != 50 {
		t.Fatalf("expected 50 recovered points from WAL, got %d series", len(results))
	}
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Fatalf("stale temp block dir was not cleaned up on open")
	}
}

// TestEpochZeroAndNegativeTimestamps verifies that ts==0 is a real sample (not the
// "unset" sentinel) and that negative timestamps work, including the flush trigger.
func TestEpochZeroAndNegativeTimestamps(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultTSDBOptions()
	opts.WALDir = filepath.Join(dir, "wal")
	opts.BlockDir = filepath.Join(dir, "blocks")
	opts.FlushInterval = time.Hour
	opts.BlockDuration = time.Millisecond // any real span triggers a flush
	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	lbl := map[string]string{"h": "a"}
	if err := db.Ingest("m", lbl, 0, 1.0); err != nil {
		t.Fatal(err)
	}
	if got := db.Head().MinTime(); got != 0 {
		t.Fatalf("MinTime with a real ts=0 sample: got %d, want 0", got)
	}
	if got := db.Head().MaxTime(); got != 0 {
		t.Fatalf("MaxTime: got %d, want 0", got)
	}

	// Query [0,0] returns the epoch-0 point.
	results, err := db.Query(context.Background(),
		[]LabelMatcher{{Name: "__name__", Value: "m", Type: MatchEqual}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || len(results[0].Points) != 1 || results[0].Points[0].Timestamp != 0 {
		t.Fatalf("epoch-0 sample not queryable: %+v", results)
	}

	// A later sample makes the head span exceed BlockDuration; maybeFlush must NOT
	// skip just because MinTime()==0 (the old sentinel bug).
	if err := db.Ingest("m", lbl, 1000, 2.0); err != nil {
		t.Fatal(err)
	}
	db.maybeFlush()
	if len(db.Blocks()) != 1 {
		t.Fatal("maybeFlush mis-skipped a head whose MinTime is 0")
	}

	// Negative timestamps.
	if err := db.Ingest("neg", lbl, -5000, 10.0); err != nil {
		t.Fatal(err)
	}
	if err := db.Ingest("neg", lbl, -1000, 11.0); err != nil {
		t.Fatal(err)
	}
	results, err = db.Query(context.Background(),
		[]LabelMatcher{{Name: "__name__", Value: "neg", Type: MatchEqual}}, -10000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || len(results[0].Points) != 2 {
		t.Fatalf("negative-timestamp samples not recovered: %+v", results)
	}
}

// TestOversizedLabelRejected rejects names/labels that would overflow the uint16
// on-disk length fields, while a long-but-legal label round-trips through WAL replay
// and a flushed block index.
func TestOversizedLabelRejected(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)

	if err := db.Ingest("m", map[string]string{"h": strings.Repeat("x", MaxLabelValueLength+1)}, 1000, 1.0); err == nil {
		t.Fatal("expected error for oversized label value")
	}
	if err := db.Ingest(strings.Repeat("n", MaxMetricNameLength+1), nil, 1000, 1.0); err == nil {
		t.Fatal("expected error for oversized metric name")
	}

	// A 4 KiB label value is legal and must round-trip.
	okVal := strings.Repeat("y", 4096)
	if err := db.Ingest("ok", map[string]string{"h": okVal}, 2000, 2.0); err != nil {
		t.Fatalf("legal long label rejected: %v", err)
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Reopen and confirm the value survived the block index round-trip.
	opts := DefaultTSDBOptions()
	opts.WALDir = filepath.Join(dir, "wal")
	opts.BlockDir = filepath.Join(dir, "blocks")
	opts.FlushInterval = time.Hour
	db2, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	results, err := db2.Query(context.Background(),
		[]LabelMatcher{{Name: "__name__", Value: "ok", Type: MatchEqual}}, 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Labels["h"] != okVal {
		t.Fatalf("long-but-legal label did not round-trip: %+v", results)
	}
}

// TestLabelsMapDefensiveCopy ensures the caller's labels map is copied on series
// creation, so mutating/reusing it afterward cannot corrupt the stored series.
func TestLabelsMapDefensiveCopy(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)
	defer db.Close()

	labels := map[string]string{"host": "web-01"}
	if err := db.Ingest("m", labels, 1000, 1.0); err != nil {
		t.Fatal(err)
	}
	// Mutate the caller's map after ingest, as a pooled/reused map would be.
	labels["host"] = "web-99"
	labels["injected"] = "bad"

	results, err := db.Query(context.Background(),
		[]LabelMatcher{{Name: "host", Value: "web-01", Type: MatchEqual}}, 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("stored labels were mutated through the caller's map: got %d series for host=web-01", len(results))
	}
	if _, ok := results[0].Labels["injected"]; ok {
		t.Fatal("caller's later map insertion leaked into the stored series")
	}
}
