package ingestion

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// BatchWriter buffers incoming samples and flushes them to the TSDB in batches.
type BatchWriter struct {
	db            *storage.TSDB
	batchSize     int
	flushInterval time.Duration

	mu      sync.Mutex
	buffer  []storage.IngestSample
	ticker  *time.Ticker
	done    chan struct{}

	// Metrics
	totalIngested atomic.Int64
	totalBatches  atomic.Int64
	totalErrors   atomic.Int64
	lastFlushTime atomic.Int64
}

// NewBatchWriter creates a new batch writer with the given parameters.
func NewBatchWriter(db *storage.TSDB, batchSize int, flushInterval time.Duration) *BatchWriter {
	bw := &BatchWriter{
		db:            db,
		batchSize:     batchSize,
		flushInterval: flushInterval,
		buffer:        make([]storage.IngestSample, 0, batchSize),
		done:          make(chan struct{}),
	}

	bw.ticker = time.NewTicker(flushInterval)
	go bw.flushLoop()

	return bw
}

// Add adds a sample to the batch buffer. If the buffer is full, it flushes immediately.
func (bw *BatchWriter) Add(name string, labels map[string]string, ts int64, value float64) {
	bw.mu.Lock()
	bw.buffer = append(bw.buffer, storage.IngestSample{
		Name:      name,
		Labels:    labels,
		Timestamp: ts,
		Value:     value,
	})
	shouldFlush := len(bw.buffer) >= bw.batchSize
	bw.mu.Unlock()

	if shouldFlush {
		bw.Flush()
	}
}

// AddBatch adds multiple samples to the buffer.
func (bw *BatchWriter) AddBatch(samples []storage.IngestSample) {
	bw.mu.Lock()
	bw.buffer = append(bw.buffer, samples...)
	shouldFlush := len(bw.buffer) >= bw.batchSize
	bw.mu.Unlock()

	if shouldFlush {
		bw.Flush()
	}
}

// Flush writes all buffered samples to the TSDB.
func (bw *BatchWriter) Flush() {
	bw.mu.Lock()
	if len(bw.buffer) == 0 {
		bw.mu.Unlock()
		return
	}
	batch := bw.buffer
	bw.buffer = make([]storage.IngestSample, 0, bw.batchSize)
	bw.mu.Unlock()

	if err := bw.db.IngestBatch(batch); err != nil {
		bw.totalErrors.Add(1)
		return
	}
	bw.totalIngested.Add(int64(len(batch)))
	bw.totalBatches.Add(1)
	bw.lastFlushTime.Store(time.Now().UnixMilli())
}

// Close flushes remaining data and stops the batch writer.
func (bw *BatchWriter) Close() {
	close(bw.done)
	bw.ticker.Stop()
	bw.Flush()
}

// Stats returns ingestion statistics.
func (bw *BatchWriter) Stats() BatchStats {
	return BatchStats{
		TotalIngested: bw.totalIngested.Load(),
		TotalBatches:  bw.totalBatches.Load(),
		TotalErrors:   bw.totalErrors.Load(),
	}
}

// BatchStats holds batch writer metrics.
type BatchStats struct {
	TotalIngested int64
	TotalBatches  int64
	TotalErrors   int64
}

func (bw *BatchWriter) flushLoop() {
	for {
		select {
		case <-bw.done:
			return
		case <-bw.ticker.C:
			bw.Flush()
		}
	}
}
