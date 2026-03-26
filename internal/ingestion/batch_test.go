package ingestion

import (
	"sync"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

func setupTestDB(t *testing.T) *storage.TSDB {
	t.Helper()
	dir := t.TempDir()
	opts := storage.DefaultTSDBOptions()
	opts.WALDir = dir + "/wal"
	opts.BlockDir = dir + "/blocks"
	opts.FlushInterval = 1 * time.Hour

	db, err := storage.Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestBatchWriterFlushOnSize(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	bw := NewBatchWriter(db, 10, 1*time.Hour) // large interval, small batch
	defer bw.Close()

	for i := 0; i < 25; i++ {
		bw.Add("metric", map[string]string{"host": "a"}, int64(i)*1000, float64(i))
	}

	// Force flush remaining
	bw.Flush()

	stats := bw.Stats()
	if stats.TotalIngested != 25 {
		t.Fatalf("total ingested: %d", stats.TotalIngested)
	}
	if stats.TotalBatches < 2 {
		t.Fatalf("expected at least 2 batches (10+10+5), got %d", stats.TotalBatches)
	}
}

func TestBatchWriterFlushOnTimeout(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	bw := NewBatchWriter(db, 1000, 50*time.Millisecond) // small interval
	defer bw.Close()

	bw.Add("metric", nil, 1000, 1.0)
	bw.Add("metric", nil, 2000, 2.0)

	// Wait for timer-based flush
	time.Sleep(200 * time.Millisecond)

	stats := bw.Stats()
	if stats.TotalIngested < 2 {
		t.Fatalf("expected 2 ingested after timeout, got %d", stats.TotalIngested)
	}
}

func TestBatchWriterConcurrentWriters(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	bw := NewBatchWriter(db, 50, 100*time.Millisecond)
	defer bw.Close()

	nWriters := 8
	samplesPerWriter := 100
	var wg sync.WaitGroup

	for w := 0; w < nWriters; w++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			for i := 0; i < samplesPerWriter; i++ {
				bw.Add("metric", map[string]string{"writer": "w"}, int64(i)*1000, float64(writerID))
			}
		}(w)
	}
	wg.Wait()
	bw.Flush()

	stats := bw.Stats()
	expected := int64(nWriters * samplesPerWriter)
	if stats.TotalIngested != expected {
		t.Fatalf("total ingested: got %d, want %d", stats.TotalIngested, expected)
	}
}

func TestBatchWriterAddBatch(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	bw := NewBatchWriter(db, 100, 1*time.Hour)
	defer bw.Close()

	samples := make([]storage.IngestSample, 50)
	for i := range samples {
		samples[i] = storage.IngestSample{
			Name:      "metric",
			Labels:    map[string]string{"host": "a"},
			Timestamp: int64(i) * 1000,
			Value:     float64(i),
		}
	}
	bw.AddBatch(samples)
	bw.Flush()

	stats := bw.Stats()
	if stats.TotalIngested != 50 {
		t.Fatalf("total ingested: %d", stats.TotalIngested)
	}
}

func TestValidateMetricName(t *testing.T) {
	valid := []string{"cpu_usage", "http_requests_total", "go:gc_duration", "_private"}
	for _, name := range valid {
		if err := ValidateMetricName(name); err != nil {
			t.Fatalf("should be valid: %s: %v", name, err)
		}
	}

	invalid := []string{"", "123abc", "with-dash", "with space"}
	for _, name := range invalid {
		if err := ValidateMetricName(name); err == nil {
			t.Fatalf("should be invalid: %q", name)
		}
	}
}
