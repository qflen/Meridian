package storage

import (
	"context"
	"testing"
	"time"
)

func TestTSDBIngestAndQuery(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultTSDBOptions()
	opts.WALDir = dir + "/wal"
	opts.BlockDir = dir + "/blocks"
	opts.FlushInterval = 1 * time.Hour // disable auto-flush

	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Ingest samples
	for i := 0; i < 100; i++ {
		ts := int64(i) * 5000
		db.Ingest("cpu_usage", map[string]string{"host": "web-01"}, ts, float64(45+i%10))
		db.Ingest("cpu_usage", map[string]string{"host": "web-02"}, ts, float64(60+i%5))
		db.Ingest("mem_bytes", map[string]string{"host": "web-01"}, ts, float64(8e9))
	}

	// Query from head
	ctx := context.Background()
	results, err := db.Query(ctx, []LabelMatcher{
		{Name: "__name__", Value: "cpu_usage", Type: MatchEqual},
	}, 0, 500000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 series, got %d", len(results))
	}

	stats := db.Stats()
	if stats.TotalSamples != 300 {
		t.Fatalf("total samples: %d", stats.TotalSamples)
	}
	if stats.TotalSeries != 3 {
		t.Fatalf("total series: %d", stats.TotalSeries)
	}
}

func TestTSDBFlushAndQuery(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultTSDBOptions()
	opts.WALDir = dir + "/wal"
	opts.BlockDir = dir + "/blocks"
	opts.FlushInterval = 1 * time.Hour

	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Ingest some data
	for i := 0; i < 50; i++ {
		db.Ingest("metric", map[string]string{"host": "a"}, int64(i)*1000, float64(i))
	}

	// Flush to persistent block
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	if len(db.Blocks()) != 1 {
		t.Fatalf("expected 1 block, got %d", len(db.Blocks()))
	}

	// Head should be empty
	if db.Head().SampleCount() != 0 {
		t.Fatalf("head samples after flush: %d", db.Head().SampleCount())
	}

	// Ingest more data into the new head
	for i := 50; i < 100; i++ {
		db.Ingest("metric", map[string]string{"host": "a"}, int64(i)*1000, float64(i))
	}

	// Query should merge head + block
	ctx := context.Background()
	results, err := db.Query(ctx, []LabelMatcher{
		{Name: "__name__", Value: "metric", Type: MatchEqual},
	}, 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 series, got %d", len(results))
	}
	if len(results[0].Points) != 100 {
		t.Fatalf("expected 100 points, got %d", len(results[0].Points))
	}
	// Verify points are sorted
	for i := 1; i < len(results[0].Points); i++ {
		if results[0].Points[i].Timestamp <= results[0].Points[i-1].Timestamp {
			t.Fatal("points not sorted")
		}
	}
}

func TestTSDBBatchIngest(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultTSDBOptions()
	opts.WALDir = dir + "/wal"
	opts.BlockDir = dir + "/blocks"
	opts.FlushInterval = 1 * time.Hour

	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	batch := make([]IngestSample, 100)
	for i := range batch {
		batch[i] = IngestSample{
			Name:      "metric",
			Labels:    map[string]string{"host": "a"},
			Timestamp: int64(i) * 1000,
			Value:     float64(i),
		}
	}

	if err := db.IngestBatch(batch); err != nil {
		t.Fatal(err)
	}

	stats := db.Stats()
	if stats.TotalSamples != 100 {
		t.Fatalf("total samples: %d", stats.TotalSamples)
	}
}

func TestTSDBRetention(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultTSDBOptions()
	opts.WALDir = dir + "/wal"
	opts.BlockDir = dir + "/blocks"
	opts.FlushInterval = 1 * time.Hour

	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Ingest and flush to create a block
	for i := 0; i < 10; i++ {
		db.Ingest("metric", nil, int64(i)*1000, float64(i))
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	blocks := db.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}

	// Delete the block (simulating retention)
	if err := db.DeleteBlock(blocks[0].Meta().ULID); err != nil {
		t.Fatal(err)
	}
	if len(db.Blocks()) != 0 {
		t.Fatal("block not deleted")
	}
}

func TestTSDBRestart(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultTSDBOptions()
	opts.WALDir = dir + "/wal"
	opts.BlockDir = dir + "/blocks"
	opts.FlushInterval = 1 * time.Hour

	// First session: ingest and flush
	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		db.Ingest("metric", map[string]string{"host": "a"}, int64(i)*1000, float64(i))
	}
	db.Flush()
	db.Close()

	// Second session: reopen and verify
	db2, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	if len(db2.Blocks()) != 1 {
		t.Fatalf("expected 1 block after restart, got %d", len(db2.Blocks()))
	}

	ctx := context.Background()
	results, err := db2.Query(ctx, []LabelMatcher{
		{Name: "__name__", Value: "metric", Type: MatchEqual},
	}, 0, 50000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 series, got %d", len(results))
	}
	if len(results[0].Points) != 50 {
		t.Fatalf("expected 50 points, got %d", len(results[0].Points))
	}
}

func TestTSDBLabelNamesAndValues(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultTSDBOptions()
	opts.WALDir = dir + "/wal"
	opts.BlockDir = dir + "/blocks"
	opts.FlushInterval = 1 * time.Hour

	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.Ingest("cpu", map[string]string{"host": "web-01", "region": "us"}, 1000, 1.0)
	db.Ingest("cpu", map[string]string{"host": "web-02", "region": "eu"}, 1000, 2.0)

	names := db.LabelNames()
	if len(names) < 3 { // __name__, host, region
		t.Fatalf("expected at least 3 label names, got %d: %v", len(names), names)
	}

	values := db.LabelValues("host")
	if len(values) != 2 {
		t.Fatalf("expected 2 host values, got %d", len(values))
	}
}
