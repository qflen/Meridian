package storage

import (
	"testing"
	"time"
)

func TestTotalSeriesCountsAcrossBlocksDistinctly(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultTSDBOptions()
	opts.WALDir = dir + "/wal"
	opts.BlockDir = dir + "/blocks"
	opts.FlushInterval = time.Hour
	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Two distinct series in the head, then flush so they move to a block.
	for i := 0; i < 10; i++ {
		ts := int64(i) * 1000
		if err := db.Ingest("cpu", map[string]string{"host": "a"}, ts, 1); err != nil {
			t.Fatal(err)
		}
		if err := db.Ingest("cpu", map[string]string{"host": "b"}, ts, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	// Post-flush the head is empty, but both series live in the block.
	if got := db.Stats().TotalSeries; got != 2 {
		t.Fatalf("TotalSeries after flush = %d, want 2 (counted from the block, not the empty head)", got)
	}

	// Ingest a new series C into the head, and re-ingest series A (already in the
	// block) into the head as well.
	for i := 10; i < 20; i++ {
		ts := int64(i) * 1000
		if err := db.Ingest("cpu", map[string]string{"host": "c"}, ts, 1); err != nil {
			t.Fatal(err)
		}
		if err := db.Ingest("cpu", map[string]string{"host": "a"}, ts, 1); err != nil {
			t.Fatal(err)
		}
	}

	// Distinct series are {a, b, c}: A appears in both head and block but is counted
	// once, B only in the block, C only in the head.
	if got := db.Stats().TotalSeries; got != 3 {
		t.Fatalf("TotalSeries across head+block = %d, want 3 distinct (no double-count of A)", got)
	}
}
