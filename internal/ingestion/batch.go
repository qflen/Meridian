package ingestion

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/meridiandb/meridian/internal/backpressure"
	"github.com/meridiandb/meridian/internal/storage"
)

// defaultBlockDeadline is how long a producer blocks against a full queue before a
// batch is shed, when no explicit deadline is configured.
const defaultBlockDeadline = 250 * time.Millisecond

// batchSink is the drain target for the BatchWriter. *storage.TSDB satisfies it;
// tests inject a slow or blocking sink to exercise backpressure and shedding.
type batchSink interface {
	IngestBatch([]storage.IngestSample) error
}

// QueueOptions bounds the ingest queue. Invalid or zero capacity/high-water values
// fall back to sane defaults derived from batchSize, so a bare NewBatchWriter still
// gets a bounded queue. A BlockDeadline of exactly 0 is honoured as non-blocking
// (shed immediately when full); a negative value is treated as 0.
type QueueOptions struct {
	Capacity      int           // hard cap in samples
	HighWatermark int           // throttle threshold in samples
	BlockDeadline time.Duration // how long enqueue blocks before shedding
	// Admission, when non-nil, layers per-series fair-share / priority-class shedding
	// (ADR-027) in front of the bounded queue: under overload it sheds the lowest
	// priority and most over-budget series first. Nil leaves the queue's uniform
	// block-then-shed as the only policy (the default).
	Admission *backpressure.ShaperConfig
}

func (o QueueOptions) normalize(batchSize int) QueueOptions {
	if o.Capacity < batchSize {
		o.Capacity = 50 * batchSize
	}
	if o.HighWatermark < 1 || o.HighWatermark > o.Capacity {
		o.HighWatermark = o.Capacity * 4 / 5
		if o.HighWatermark < 1 {
			o.HighWatermark = o.Capacity
		}
	}
	if o.BlockDeadline < 0 {
		o.BlockDeadline = 0
	}
	return o
}

// AddResult reports the flow-control outcome of feeding samples to the writer.
type AddResult struct {
	// Shed is the number of samples dropped in this call because the queue was full
	// past the block deadline. meridian_dropped_samples_total is the authoritative
	// cumulative count; this is the best-effort per-call signal used to NACK the
	// producer. (Because batches coalesce samples across calls, the attribution to a
	// single call is approximate; the queue's counter is exact.)
	Shed int64
	// Throttled is set when the queue reached its high-water mark — an early
	// backpressure hint so a cooperative producer eases off before shedding begins.
	Throttled bool
}

// BatchWriter coalesces incoming samples into batches and drains them to the TSDB
// through a bounded queue (ADR-023). Producers (Add/AddBatch) accumulate samples
// and, when a full batch forms, enqueue it with block-then-shed flow control: a
// full queue blocks the producer up to the block deadline (backpressure) and then
// sheds the batch (drop + count) rather than growing without bound. A single drain
// goroutine pulls batches in FIFO order and writes them, so a slow TSDB applies
// backpressure upstream and the queue depth bounds resident memory.
type BatchWriter struct {
	sink          batchSink
	batchSize     int
	flushInterval time.Duration

	queue *backpressure.Queue[[]storage.IngestSample]
	block time.Duration
	// shaper, when set, runs per-series fair-share / priority admission ahead of the
	// queue (ADR-027); nil falls back to the queue's uniform shedding.
	shaper *backpressure.Shaper

	mu     sync.Mutex
	buffer []storage.IngestSample // staging accumulation, always < batchSize between calls
	ticker *time.Ticker
	done   chan struct{} // stops the flush-tick loop
	drain  chan struct{} // closed when the drain goroutine exits

	// Drain-side metrics. The bounded queue owns the flow-control counters
	// (dropped/shed/backpressure/depth); see QueueStats.
	totalIngested atomic.Int64
	totalBatches  atomic.Int64
	totalErrors   atomic.Int64
	lastFlushTime atomic.Int64
}

// NewBatchWriter creates a batch writer with a bounded queue sized from defaults.
func NewBatchWriter(db *storage.TSDB, batchSize int, flushInterval time.Duration) *BatchWriter {
	return newBatchWriterSink(db, batchSize, flushInterval, QueueOptions{BlockDeadline: defaultBlockDeadline})
}

// NewBatchWriterWithQueue creates a batch writer whose bounded queue is sized from
// opts (wired from ingestion config by the serve command).
func NewBatchWriterWithQueue(db *storage.TSDB, batchSize int, flushInterval time.Duration, opts QueueOptions) *BatchWriter {
	return newBatchWriterSink(db, batchSize, flushInterval, opts)
}

func newBatchWriterSink(sink batchSink, batchSize int, flushInterval time.Duration, opts QueueOptions) *BatchWriter {
	if batchSize < 1 {
		batchSize = 1
	}
	opts = opts.normalize(batchSize)
	var shaper *backpressure.Shaper
	if opts.Admission != nil {
		shaper = backpressure.NewShaper(opts.Capacity, *opts.Admission)
	}
	bw := &BatchWriter{
		sink:          sink,
		batchSize:     batchSize,
		flushInterval: flushInterval,
		queue:         backpressure.New[[]storage.IngestSample](opts.Capacity, opts.HighWatermark),
		block:         opts.BlockDeadline,
		shaper:        shaper,
		buffer:        make([]storage.IngestSample, 0, batchSize),
		done:          make(chan struct{}),
		drain:         make(chan struct{}),
	}
	bw.ticker = time.NewTicker(flushInterval)
	go bw.flushLoop()
	go bw.drainLoop()
	return bw
}

// Add buffers one sample and, when a full batch accumulates, enqueues it with
// block-then-shed flow control. The returned AddResult reports any shedding so the
// caller can NACK the producer.
func (bw *BatchWriter) Add(name string, labels map[string]string, ts int64, value float64) AddResult {
	// Admission shedding (ADR-027) runs before the sample is buffered: a low-priority
	// or over-budget series is dropped here rather than crowding the queue.
	if bw.shaper != nil {
		if !bw.shaper.Admit(name, labels, 1, bw.queue.Depth()).Admit {
			bw.queue.RecordShed(1)
			return AddResult{Shed: 1, Throttled: true}
		}
	}
	bw.mu.Lock()
	bw.buffer = append(bw.buffer, storage.IngestSample{Name: name, Labels: labels, Timestamp: ts, Value: value})
	batches := bw.cutFullLocked()
	bw.mu.Unlock()
	return bw.enqueueAll(batches)
}

// AddBatch buffers many samples and enqueues every full batch that forms. When
// admission shedding is enabled, samples are first filtered per-series (ADR-027): the
// shed ones are dropped and counted before any buffering.
func (bw *BatchWriter) AddBatch(samples []storage.IngestSample) AddResult {
	if len(samples) == 0 {
		return AddResult{}
	}
	var preShed int64
	if bw.shaper != nil {
		samples, preShed = bw.admit(samples)
	}
	bw.mu.Lock()
	bw.buffer = append(bw.buffer, samples...)
	batches := bw.cutFullLocked()
	bw.mu.Unlock()
	res := bw.enqueueAll(batches)
	res.Shed += preShed
	if preShed > 0 {
		res.Throttled = true
	}
	return res
}

// admit filters samples through the shaper, returning the kept samples (compacted into
// the input's backing array) and the number shed. Each admission drop is folded into
// the queue's grand-total drop counter so meridian_dropped_samples_total stays
// authoritative; the per-class/series breakdown is recorded inside the shaper.
func (bw *BatchWriter) admit(samples []storage.IngestSample) ([]storage.IngestSample, int64) {
	depth := bw.queue.Depth()
	kept := samples[:0]
	var shed int64
	for _, s := range samples {
		if bw.shaper.Admit(s.Name, s.Labels, 1, depth).Admit {
			kept = append(kept, s)
		} else {
			shed++
		}
	}
	if shed > 0 {
		bw.queue.RecordShed(int(shed))
	}
	return kept, shed
}

// Flush enqueues the current staging buffer immediately, even below batchSize. It
// runs on the flush-interval tick and on Close so partial batches don't linger.
func (bw *BatchWriter) Flush() AddResult {
	bw.mu.Lock()
	if len(bw.buffer) == 0 {
		bw.mu.Unlock()
		return AddResult{}
	}
	batch := bw.buffer
	bw.buffer = make([]storage.IngestSample, 0, bw.batchSize)
	bw.mu.Unlock()
	return bw.enqueueAll([][]storage.IngestSample{batch})
}

// cutFullLocked detaches every full batchSize-sized chunk from the front of the
// staging buffer, leaving a remainder < batchSize in a fresh slice (so the large
// appended backing array is released). Callers hold bw.mu.
func (bw *BatchWriter) cutFullLocked() [][]storage.IngestSample {
	if len(bw.buffer) < bw.batchSize {
		return nil
	}
	var batches [][]storage.IngestSample
	i := 0
	for ; i+bw.batchSize <= len(bw.buffer); i += bw.batchSize {
		b := make([]storage.IngestSample, bw.batchSize)
		copy(b, bw.buffer[i:i+bw.batchSize])
		batches = append(batches, b)
	}
	rem := make([]storage.IngestSample, len(bw.buffer)-i, bw.batchSize)
	copy(rem, bw.buffer[i:])
	bw.buffer = rem
	return batches
}

// enqueueAll offers each batch to the bounded queue and aggregates the outcome.
func (bw *BatchWriter) enqueueAll(batches [][]storage.IngestSample) AddResult {
	var res AddResult
	for _, b := range batches {
		r := bw.queue.Enqueue(b, len(b), bw.block)
		if !r.Accepted {
			res.Shed += int64(len(b))
		}
		if r.Throttled {
			res.Throttled = true
		}
	}
	return res
}

// drainLoop pulls batches in FIFO order and writes them to the sink. It exits once
// the queue is closed and drained, so Close flushes everything still queued.
func (bw *BatchWriter) drainLoop() {
	defer close(bw.drain)
	for {
		batch, ok := bw.queue.Dequeue()
		if !ok {
			return
		}
		if err := bw.sink.IngestBatch(batch); err != nil {
			bw.totalErrors.Add(1)
			continue
		}
		bw.totalIngested.Add(int64(len(batch)))
		bw.totalBatches.Add(1)
		bw.lastFlushTime.Store(time.Now().UnixMilli())
	}
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

// Close stops accepting flush ticks, enqueues the final partial batch, then closes
// the queue and waits for the drain to finish the backlog.
func (bw *BatchWriter) Close() {
	close(bw.done)
	bw.ticker.Stop()
	bw.Flush()
	bw.queue.Close()
	<-bw.drain
}

// Stats returns ingestion statistics, including the bounded-queue flow-control
// snapshot.
func (bw *BatchWriter) Stats() BatchStats {
	return BatchStats{
		TotalIngested: bw.totalIngested.Load(),
		TotalBatches:  bw.totalBatches.Load(),
		TotalErrors:   bw.totalErrors.Load(),
		Queue:         bw.queue.Stats(),
	}
}

// QueueStats returns just the bounded-queue flow-control snapshot (depth, capacity,
// drops, shed/backpressure events), used by the /metrics and /api/v1/stats handlers.
func (bw *BatchWriter) QueueStats() backpressure.Stats {
	return bw.queue.Stats()
}

// AdmissionStats returns the per-series fair-share / priority-class admission snapshot
// (ADR-027), or the zero value when admission shedding is disabled.
func (bw *BatchWriter) AdmissionStats() backpressure.ShaperStats {
	return bw.shaper.Stats() // nil-safe: a disabled (nil) shaper reports the zero value
}

// BatchStats holds batch writer metrics.
type BatchStats struct {
	TotalIngested int64
	TotalBatches  int64
	TotalErrors   int64
	Queue         backpressure.Stats
}
