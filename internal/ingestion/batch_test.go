package ingestion

import (
	"sync"
	"sync/atomic"
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

// waitFor polls cond until it holds or the deadline elapses. The drain is
// asynchronous, so ingestion counts settle shortly after the producer returns.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}

// gateSink is a batchSink whose IngestBatch blocks until released, used to stall
// the drain and force the queue to fill. release is idempotent so a failing test
// can never deadlock in Close waiting on a stalled drain.
type gateSink struct {
	open     chan struct{}
	once     sync.Once
	ingested int64
}

func newGateSink() *gateSink { return &gateSink{open: make(chan struct{})} }

func (g *gateSink) IngestBatch(b []storage.IngestSample) error {
	<-g.open
	atomic.AddInt64(&g.ingested, int64(len(b)))
	return nil
}

func (g *gateSink) release()        { g.once.Do(func() { close(g.open) }) }
func (g *gateSink) Ingested() int64 { return atomic.LoadInt64(&g.ingested) }

func TestBatchWriterFlushOnSize(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	bw := NewBatchWriter(db, 10, 1*time.Hour) // large interval, small batch
	defer bw.Close()

	for i := 0; i < 25; i++ {
		bw.Add("metric", map[string]string{"host": "a"}, int64(i)*1000, float64(i))
	}
	bw.Flush() // force the trailing partial batch

	waitFor(t, time.Second, func() bool { return bw.Stats().TotalIngested == 25 })
	if stats := bw.Stats(); stats.TotalBatches < 2 {
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

	// The flush tick should drain the partial batch without an explicit Flush.
	waitFor(t, time.Second, func() bool { return bw.Stats().TotalIngested >= 2 })
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

	expected := int64(nWriters * samplesPerWriter)
	// The default queue is large, so nothing is shed; all offered samples drain.
	waitFor(t, 2*time.Second, func() bool { return bw.Stats().TotalIngested == expected })
	if d := bw.Stats().Queue.DroppedSamples; d != 0 {
		t.Fatalf("unexpected shedding under normal load: dropped %d", d)
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

	waitFor(t, time.Second, func() bool { return bw.Stats().TotalIngested == 50 })
}

// TestBatchWriterBackpressureAndShedding is the core proof for the monolith path:
// a stalled drain fills the bounded queue, the producer is backpressured and then
// shed past the deadline, the drop counters are exact, the queue never exceeds
// capacity, and once the drain recovers throughput resumes and shedding stops.
func TestBatchWriterBackpressureAndShedding(t *testing.T) {
	sink := newGateSink()
	opts := QueueOptions{Capacity: 5, HighWatermark: 4, BlockDeadline: 30 * time.Millisecond}
	bw := newBatchWriterSink(sink, 1, time.Hour, opts) // batchSize 1 ⇒ each Add cuts a batch
	defer func() {
		sink.release()
		bw.Close()
	}()

	// Drain stalls on its first IngestBatch; the queue holds at most capacity more.
	const offered = 20
	var shed int64
	for i := 0; i < offered; i++ {
		shed += bw.Add("m", nil, int64(i+1)*1000, float64(i)).Shed
	}

	if shed == 0 {
		t.Fatal("expected shedding while the drain was blocked")
	}
	q := bw.QueueStats()
	if q.DroppedSamples != shed {
		t.Fatalf("queue DroppedSamples=%d != producer-observed shed=%d", q.DroppedSamples, shed)
	}
	if q.Depth > q.Capacity || q.MaxDepth > q.Capacity {
		t.Fatalf("queue exceeded capacity %d (depth %d, max %d) — memory not bounded", q.Capacity, q.Depth, q.MaxDepth)
	}
	if q.BackpressureEvents == 0 {
		t.Fatal("expected the producer to block (backpressure) while the queue was full")
	}

	// Recovery: release the drain; every accepted sample must ingest, shedding stops.
	accepted := int64(offered) - shed
	sink.release()
	waitFor(t, 2*time.Second, func() bool { return sink.Ingested() == accepted })

	droppedBefore := bw.QueueStats().DroppedSamples
	for i := offered; i < offered+5; i++ {
		bw.Add("m", nil, int64(i+1)*1000, float64(i))
	}
	waitFor(t, 2*time.Second, func() bool { return sink.Ingested() == accepted+5 })
	if d := bw.QueueStats().DroppedSamples; d != droppedBefore {
		t.Fatalf("shedding continued after recovery: dropped %d -> %d", droppedBefore, d)
	}
}

// TestBatchWriterBoundedUnderFlood: many producers hammer the writer while the
// drain is stalled; the queue depth must stay within capacity and every offered
// sample is accounted for as accepted or dropped. Run with -race.
func TestBatchWriterBoundedUnderFlood(t *testing.T) {
	sink := newGateSink()
	opts := QueueOptions{Capacity: 100, HighWatermark: 80, BlockDeadline: 2 * time.Millisecond}
	bw := newBatchWriterSink(sink, 1, time.Hour, opts)
	defer func() {
		sink.release()
		bw.Close()
	}()

	const (
		producers = 8
		perProd   = 500
	)
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProd; i++ {
				bw.Add("m", nil, int64(p*1_000_000+i)*1000, 1)
			}
		}(p)
	}
	wg.Wait()

	q := bw.QueueStats()
	if q.MaxDepth > q.Capacity {
		t.Fatalf("max depth %d exceeded capacity %d under flood — memory not bounded", q.MaxDepth, q.Capacity)
	}
	if q.DroppedSamples == 0 {
		t.Fatal("expected shedding under a flood with a stalled drain")
	}
	offered := int64(producers * perProd)
	if q.Enqueued+q.DroppedSamples != offered {
		t.Fatalf("conservation broken: enqueued %d + dropped %d != offered %d", q.Enqueued, q.DroppedSamples, offered)
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
