package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// replayWAL reopens a WAL in the given directory and replays every segment,
// returning the recovered entries. Replay is independent of commit mode, so the
// default (synchronous) WAL is used to read back what a group-commit WAL wrote.
func replayWAL(t *testing.T, dir string) *testWALHandler {
	t.Helper()
	r, err := OpenWAL(dir)
	if err != nil {
		t.Fatalf("reopen WAL: %v", err)
	}
	defer r.Close()
	h := &testWALHandler{}
	if err := r.Replay(h); err != nil {
		t.Fatalf("replay: %v", err)
	}
	return h
}

// TestWALGroupCommitWriteReplay is the group-commit analogue of TestWALWriteAndReplay:
// a series + samples written through the committer must round-trip byte-for-byte, so
// the on-disk format is independent of commit mode.
func TestWALGroupCommitWriteReplay(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWALWithOptions(dir, WALOptions{GroupCommit: true})
	if err != nil {
		t.Fatal(err)
	}

	if err := w.LogSeries(1, "cpu_usage", map[string]string{"host": "web-01", "region": "us-east"}); err != nil {
		t.Fatal(err)
	}
	samples := []Sample{
		{SeriesID: 1, Timestamp: 1000, Value: 0.75},
		{SeriesID: 1, Timestamp: 2000, Value: 0.80},
	}
	if err := w.LogSamples(samples); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	h := replayWAL(t, dir)
	if len(h.series) != 1 || h.series[0].name != "cpu_usage" {
		t.Fatalf("series round-trip failed: %+v", h.series)
	}
	if h.series[0].labels["region"] != "us-east" {
		t.Fatalf("label round-trip failed: %+v", h.series[0].labels)
	}
	if len(h.samples) != 2 || h.samples[0].Value != 0.75 || h.samples[1].Timestamp != 2000 {
		t.Fatalf("sample round-trip failed: %+v", h.samples)
	}
}

// TestWALGroupCommitConcurrentDurableInOrder runs many concurrent writers, each
// appending a strictly increasing sequence to its own series. After a Close + replay
// every frame must be durable, and each writer's subsequence must appear in submission
// order — the committer preserves FIFO order within a writer even while coalescing
// across writers. Run under -race, this also exercises the committer/submitter
// handoff for data races.
func TestWALGroupCommitConcurrentDurableInOrder(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWALWithOptions(dir, WALOptions{GroupCommit: true})
	if err != nil {
		t.Fatal(err)
	}

	const writers = 16
	const perWriter = 200

	var wg sync.WaitGroup
	for id := 0; id < writers; id++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 1; j <= perWriter; j++ {
				// One frame per call; LogSamples returns only once its frame is fsynced.
				if err := w.LogSamples([]Sample{{SeriesID: uint64(id), Timestamp: int64(j), Value: float64(j)}}); err != nil {
					t.Errorf("writer %d frame %d: %v", id, j, err)
					return
				}
			}
		}(id)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	h := replayWAL(t, dir)
	if len(h.samples) != writers*perWriter {
		t.Fatalf("durability: got %d samples, want %d", len(h.samples), writers*perWriter)
	}
	// In-order per writer: replay reads frames in on-disk order, which is commit
	// order, so each series' timestamps must be exactly 1..perWriter ascending.
	perSeries := make(map[uint64][]int64)
	for _, s := range h.samples {
		perSeries[s.SeriesID] = append(perSeries[s.SeriesID], s.Timestamp)
	}
	if len(perSeries) != writers {
		t.Fatalf("expected %d series, got %d", writers, len(perSeries))
	}
	for id := 0; id < writers; id++ {
		got := perSeries[uint64(id)]
		if len(got) != perWriter {
			t.Fatalf("series %d: got %d samples, want %d", id, len(got), perWriter)
		}
		for j, ts := range got {
			if ts != int64(j+1) {
				t.Fatalf("series %d out of order at index %d: ts=%d, want %d", id, j, ts, j+1)
			}
		}
	}

	fsyncs := w.fsyncs.Load()
	t.Logf("group commit: %d frames committed in %d fsyncs (%.1f frames/fsync)",
		writers*perWriter, fsyncs, float64(writers*perWriter)/float64(fsyncs))
}

// TestWALGroupCommitOneFsyncManyFrames proves the core property — one fsync covers
// many frames. With a linger window, a fleet of writers that submit concurrently are
// coalesced into a single batch, so the commit fsync count is far below the frame
// count. Linger makes this deterministic rather than scheduling-dependent.
func TestWALGroupCommitOneFsyncManyFrames(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWALWithOptions(dir, WALOptions{GroupCommit: true, Linger: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	const n = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all writers together so they land in one linger window
			if err := w.LogSamples([]Sample{{SeriesID: uint64(i), Timestamp: int64(i), Value: float64(i)}}); err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fsyncs := w.fsyncs.Load()
	if fsyncs == 0 {
		t.Fatal("no fsyncs recorded")
	}
	if fsyncs >= uint64(n) {
		t.Fatalf("no coalescing: %d fsyncs for %d frames (expected far fewer)", fsyncs, n)
	}
	// With a 50ms linger and a single concurrent burst, this is realistically 1–2.
	if fsyncs > uint64(n)/4 {
		t.Fatalf("weak coalescing: %d fsyncs for %d frames", fsyncs, n)
	}
	t.Logf("coalesced %d frames into %d fsync(s)", n, fsyncs)

	// All frames must still be durable.
	h := replayWAL(t, dir)
	if len(h.samples) != n {
		t.Fatalf("durability: got %d samples, want %d", len(h.samples), n)
	}
}

// TestWALGroupCommitBatchSpansRotation forces a single coalesced batch to cross one
// or more segment boundaries (rotation owned by the committer). Every frame in the
// batch — those in the sealed segments and those in the final one — must be durable.
func TestWALGroupCommitBatchSpansRotation(t *testing.T) {
	dir := t.TempDir()
	// Small segment cap so a modest batch spans several segments. Each frame below is
	// 1+4+10*24 = 245 payload → padded to 248 bytes, so ~4 frames fill a 1 KiB segment.
	w, err := OpenWALWithOptions(dir, WALOptions{
		GroupCommit:    true,
		Linger:         50 * time.Millisecond,
		SegmentMaxSize: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	const frames = 20
	const samplesPerFrame = 10
	var wg sync.WaitGroup
	for i := 0; i < frames; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			batch := make([]Sample, samplesPerFrame)
			for j := range batch {
				batch[j] = Sample{SeriesID: uint64(i), Timestamp: int64(j), Value: float64(i*samplesPerFrame + j)}
			}
			if err := w.LogSamples(batch); err != nil {
				t.Errorf("frame %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	segs, _ := filepath.Glob(filepath.Join(dir, "segment-*"))
	if len(segs) < 2 {
		t.Fatalf("expected the batch to span multiple segments, got %d segment file(s)", len(segs))
	}
	t.Logf("batch spanned %d segments", len(segs))

	h := replayWAL(t, dir)
	if len(h.samples) != frames*samplesPerFrame {
		t.Fatalf("durability across rotation: got %d samples, want %d", len(h.samples), frames*samplesPerFrame)
	}
}

// TestWALGroupCommitErrorFailsBatch verifies that a write/fsync failure fails the
// WHOLE coalesced batch: every waiter sees the error. The segment fd is closed out
// from under the committer, so the batch write fails, and a linger window guarantees
// all submitters share one batch.
func TestWALGroupCommitErrorFailsBatch(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWALWithOptions(dir, WALOptions{GroupCommit: true, Linger: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	// Close the underlying fd while the committer is idle, so the next batch write
	// fails. Done under w.mu to mirror the committer's own access discipline.
	w.mu.Lock()
	if err := w.segment.Close(); err != nil {
		w.mu.Unlock()
		t.Fatalf("pre-close segment fd: %v", err)
	}
	w.mu.Unlock()

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = w.LogSamples([]Sample{{SeriesID: uint64(i), Timestamp: int64(i), Value: 1}})
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e == nil {
			t.Fatalf("frame %d: expected the failed batch to surface an error, got nil", i)
		}
	}
	// The WAL is intentionally left with a closed fd; Close will report sync/close
	// errors on it, which is expected here.
	_ = w.Close()
}

// --- TSDB-level integration: group commit under concurrent ingest + flush ---

func openGroupCommitDB(t *testing.T, dir string, linger time.Duration) *TSDB {
	t.Helper()
	opts := DefaultTSDBOptions()
	opts.WALDir = filepath.Join(dir, "wal")
	opts.BlockDir = filepath.Join(dir, "blocks")
	opts.FlushInterval = time.Hour // drive flushes manually
	opts.WALGroupCommit = true
	opts.WALCommitLinger = linger
	db, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

// TestGroupCommitConcurrentIngestFlushNoLoss is the end-to-end durability test: many
// writers ingest concurrently (each its own series) while Flush repeatedly performs
// the head-swap + WAL-rotate cut. Group commit puts a committer goroutine between the
// RLock'd ingest path and the WLock'd flush cut; under -race this proves the
// committer-owned rotation and the flush-owned Rotate never race and lose a sample.
func TestGroupCommitConcurrentIngestFlushNoLoss(t *testing.T) {
	dir := t.TempDir()
	db := openGroupCommitDB(t, dir, 0)
	defer db.Close()

	const writers = 8
	const perWriter = 1000

	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for wkr := 0; wkr < writers; wkr++ {
		wg.Add(1)
		go func(wkr int) {
			defer wg.Done()
			name := fmt.Sprintf("m%d", wkr)
			lbl := map[string]string{"h": "a"}
			for i := 0; i < perWriter; i++ {
				if err := db.Ingest(name, lbl, int64(i+1)*1000, float64(i)); err != nil {
					errCh <- fmt.Errorf("ingest %s/%d: %w", name, i, err)
					return
				}
			}
		}(wkr)
	}

	// Hammer Flush from the main goroutine while ingestion is in flight.
	for i := 0; i < 50; i++ {
		if err := db.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		t.Fatalf("ingest error: %v", err)
	}
	if err := db.Flush(); err != nil {
		t.Fatalf("final flush: %v", err)
	}

	for wkr := 0; wkr < writers; wkr++ {
		name := fmt.Sprintf("m%d", wkr)
		results, err := db.Query(context.Background(),
			[]LabelMatcher{{Name: "__name__", Value: name, Type: MatchEqual}}, 0, int64(perWriter+1)*1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 {
			t.Fatalf("series %s: expected 1 series, got %d", name, len(results))
		}
		if got := len(results[0].Points); got != perWriter {
			t.Fatalf("series %s: sample loss/duplication: got %d points, want %d", name, got, perWriter)
		}
	}
}
