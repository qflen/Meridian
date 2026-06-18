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
}

// WritePool bounds in-flight writes to a Writer with a bounded queue and a fixed
// worker pool (ADR-023). Submit blocks while the queue is full — the backpressure —
// and sheds past the block deadline (returning ErrShed) instead of letting
// unbounded concurrent writes pile up when the downstream stalls (a slow quorum
// write to replicas). The workers call Writer.Write unchanged, so replication /
// quorum semantics are preserved; only the submission rate is bounded.
type WritePool struct {
	w     Writer
	queue *backpressure.Queue[*writeJob]
	block time.Duration
	wg    sync.WaitGroup
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
	p := &WritePool{
		w:     w,
		queue: backpressure.New[*writeJob](opts.Capacity, opts.HighWatermark),
		block: opts.BlockDeadline,
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
