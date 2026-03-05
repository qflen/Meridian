package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/meridiandb/meridian/internal/backpressure"
)

// ErrShed is returned by WritePool.Submit when a request is dropped because the
// bounded queue was full past the block deadline. Callers translate it into an
// HTTP 429 (with Retry-After) or a TCP NACK.
var ErrShed = errors.New("ingest overloaded: request shed")

// Writer is the downstream a WritePool drains to. *StorageClient satisfies it (the
// quorum write to replicas); a storage node adapts its local TSDB ingest to it.
type Writer interface {
	Write(ctx context.Context, req WriteRequest) (*WriteResponse, error)
}

// PoolOptions configures a WritePool.
type PoolOptions struct {
	Capacity      int           // bounded queue size in samples (hard memory cap)
	HighWatermark int           // throttle threshold in samples
	BlockDeadline time.Duration // how long Submit blocks before shedding
	Workers       int           // concurrent in-flight writes
	// Admission, when non-nil, layers per-series fair-share / priority-class shedding
	// (ADR-027) ahead of the bounded queue: under overload a request's low-priority or
	// over-budget series are shed before the rest is enqueued. Nil leaves the queue's
	// uniform block-then-shed as the only policy (the default).
	Admission *backpressure.ShaperConfig
}

// WritePool bounds in-flight writes to a Writer with a bounded queue and a fixed
// worker pool (ADR-023). Submit blocks while the queue is full — the backpressure —
// and sheds past the block deadline (returning ErrShed) instead of letting
// unbounded concurrent writes pile up when the downstream stalls (a slow quorum
// write to replicas). The workers call Writer.Write unchanged, so replication /
// quorum semantics are preserved; only the submission rate is bounded.
type WritePool struct {
	w      Writer
	queue  *backpressure.Queue[*writeJob]
	block  time.Duration
	shaper *backpressure.Shaper // per-series/priority admission (ADR-027); nil => uniform shedding
	wg     sync.WaitGroup
}

type writeJob struct {
	ctx    context.Context
	req    WriteRequest
	result chan writeOutcome
}

type writeOutcome struct {
	resp *WriteResponse
	err  error
}

// NewWritePool starts a pool of workers draining a bounded queue to w. Invalid
// option values are clamped to safe defaults so the pool is always usable.
func NewWritePool(w Writer, opts PoolOptions) *WritePool {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.Capacity < 1 {
		opts.Capacity = 1
	}
	if opts.HighWatermark < 1 || opts.HighWatermark > opts.Capacity {
		opts.HighWatermark = opts.Capacity * 4 / 5
		if opts.HighWatermark < 1 {
			opts.HighWatermark = opts.Capacity
		}
	}
	if opts.BlockDeadline < 0 {
		opts.BlockDeadline = 0
	}
	var shaper *backpressure.Shaper
	if opts.Admission != nil {
		shaper = backpressure.NewShaper(opts.Capacity, *opts.Admission)
	}
	p := &WritePool{
		w:      w,
		queue:  backpressure.New[*writeJob](opts.Capacity, opts.HighWatermark),
		block:  opts.BlockDeadline,
		shaper: shaper,
	}
	for i := 0; i < opts.Workers; i++ {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

// Submit hands req to a worker and waits for the downstream result. If the queue
// is saturated past the block deadline the request is shed and ErrShed is returned
// (the caller replies 429 / NACK). ctx cancellation aborts the wait. The returned
// Result carries the queue's throttle/blocked signal for callers that want to hint
// the producer even on an accepted write.
func (p *WritePool) Submit(ctx context.Context, req WriteRequest) (*WriteResponse, backpressure.Result, error) {
	// Admission shedding (ADR-027): under overload drop a request's low-priority or
	// over-budget series before enqueuing the rest, so a hot/low-value series cannot
	// crowd out well-behaved or high-priority ones. The dropped cost folds into the
	// queue's grand-total counter; if every series is shed the whole request is shed.
	if p.shaper != nil {
		if req = p.admit(req); len(req.TimeSeries) == 0 {
			return nil, backpressure.Result{Throttled: true}, ErrShed
		}
	}
	cost := sampleCount(req)
	job := &writeJob{ctx: ctx, req: req, result: make(chan writeOutcome, 1)}
	r := p.queue.Enqueue(job, cost, p.block)
	if !r.Accepted {
		return nil, r, ErrShed
	}
	select {
	case out := <-job.result:
		return out.resp, r, out.err
	case <-ctx.Done():
		return nil, r, ctx.Err()
	}
}

// admit filters req's series through the shaper, returning a request of only the
// admitted series. A series is evaluated by its own labels and sample count (its queue
// cost), so fairness and priority are per-series even within a multi-series request.
// Shed cost is folded into the queue's grand-total drop counter; the per-class/series
// breakdown is recorded inside the shaper. The admitted prefix is copied lazily, so a
// request with nothing shed is returned untouched without allocating.
func (p *WritePool) admit(req WriteRequest) WriteRequest {
	depth := p.queue.Depth()
	var kept []TimeSeries
	var shedCost int
	for i, ts := range req.TimeSeries {
		cost := len(ts.Samples)
		if cost < 1 {
			cost = 1
		}
		if p.shaper.Admit(ts.Name, labelsToMap(ts.Labels), cost, depth).Admit {
			if kept != nil {
				kept = append(kept, ts)
			}
			continue
		}
		if kept == nil { // first drop: copy the admitted prefix so the input is untouched
			kept = append(make([]TimeSeries, 0, len(req.TimeSeries)), req.TimeSeries[:i]...)
		}
		shedCost += cost
	}
	if kept != nil {
		req.TimeSeries = kept
		p.queue.RecordShed(shedCost)
	}
	return req
}

// labelsToMap renders a label slice as the map the shaper classifies and hashes on.
// Nil for an unlabelled series (matched on metric name alone).
func labelsToMap(labels []Label) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	m := make(map[string]string, len(labels))
	for _, l := range labels {
		m[l.Name] = l.Value
	}
	return m
}

func (p *WritePool) worker() {
	defer p.wg.Done()
	for {
		job, ok := p.queue.Dequeue()
		if !ok {
			return
		}
		resp, err := p.w.Write(job.ctx, job.req)
		// result is buffered (cap 1), so a worker never blocks even if the Submit
		// caller already gave up (ctx cancelled).
		job.result <- writeOutcome{resp: resp, err: err}
	}
}

// Close stops the pool. Already-queued jobs are still processed (their Submit
// callers receive results) before the workers exit.
func (p *WritePool) Close() {
	p.queue.Close()
	p.wg.Wait()
}

// Stats returns the bounded-queue flow-control snapshot for /metrics and /stats.
func (p *WritePool) Stats() backpressure.Stats { return p.queue.Stats() }

// AdmissionStats returns the per-series fair-share / priority-class admission snapshot
// (ADR-027), or the zero value when admission shedding is disabled.
func (p *WritePool) AdmissionStats() backpressure.ShaperStats {
	return p.shaper.Stats() // nil-safe: a disabled (nil) shaper reports the zero value
}

// sampleCount totals the samples in a write request (the queue cost). It is at
// least 1 so an empty request still occupies a queue slot.
func sampleCount(req WriteRequest) int {
	n := 0
	for _, ts := range req.TimeSeries {
		n += len(ts.Samples)
	}
	if n < 1 {
		n = 1
	}
	return n
}
