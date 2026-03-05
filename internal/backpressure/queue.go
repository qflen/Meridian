// Package backpressure provides a cost-bounded blocking queue with block-then-
// shed enqueue — the shared flow-control primitive on every Meridian ingest
// path (the monolith batch writer, the ingestor's submission pool, and the
// storage accept queue).
//
// The model is flow control by a bounded queue (see ADR-023). A producer offers
// work to a Queue that holds at most Capacity units of "cost" — for ingest the
// cost is a sample count, so the queue's depth bounds resident memory. When the
// queue is full an Enqueue blocks up to a deadline waiting for the drain to free
// room; that blocking IS the backpressure. If the deadline elapses the item is
// shed: dropped, counted, and reported so the caller can signal overload to the
// producer (HTTP 429, TCP NACK) rather than growing without bound. By Little's
// Law the queue depth tracks arrival_rate × service_time, so bounding the depth
// bounds both memory and tail latency, and shedding past the cap is a counted,
// observable event rather than a silent OOM.
package backpressure

import (
	"sync"
	"time"
)

// Queue is a FIFO of items weighted by an additive cost, bounded so the queued
// cost never exceeds Capacity. It is safe for many concurrent producers and one
// or more drain goroutines. The zero value is not usable; construct with New.
type Queue[T any] struct {
	mu       sync.Mutex
	notEmpty sync.Cond // signalled when an item is pushed or the queue closes
	notFull  sync.Cond // signalled when an item is popped or the deadline fires

	buf   []entry[T] // ring buffer; len grows as needed, never shrinks
	head  int
	count int // number of queued items

	cost      int // sum of queued item costs (== queue depth)
	capacity  int
	highWater int
	closed    bool

	// Cumulative counters, all in cost units except *Events. Guarded by mu.
	enqueued    int64 // cost accepted into the queue
	dequeued    int64 // cost removed by the drain
	dropped     int64 // cost shed (never entered the queue)
	shedEvents  int64 // Enqueue calls that shed
	pressEvents int64 // Enqueue calls that had to block (hard backpressure)
	maxDepth    int   // high-watermark of cost ever resident
}

type entry[T any] struct {
	val  T
	cost int
}

// New creates a queue holding at most capacity units of cost. highWater is the
// depth at or above which Enqueue flags the producer to throttle (an early
// backpressure hint, before outright shedding); it is clamped to [1, capacity].
// A capacity below 1 is raised to 1 so the queue is always usable.
func New[T any](capacity, highWater int) *Queue[T] {
	if capacity < 1 {
		capacity = 1
	}
	if highWater < 1 || highWater > capacity {
		highWater = capacity
	}
	q := &Queue[T]{capacity: capacity, highWater: highWater}
	q.notEmpty.L = &q.mu
	q.notFull.L = &q.mu
	return q
}

// Result reports the outcome of an Enqueue.
type Result struct {
	// Accepted is true when the item was queued, false when it was shed.
	Accepted bool
	// Throttled is true when the queue is at or above its high-water mark, or the
	// item was shed: the producer should slow down. A cooperative producer treats
	// this as an early signal before shedding begins.
	Throttled bool
	// Blocked is how long Enqueue waited for room. A non-zero value means hard
	// backpressure was applied because the queue was full.
	Blocked time.Duration
}

// Enqueue appends val, weighted by cost (clamped to >= 1), with block-then-shed
// flow control. With room available the item is queued immediately. When the
// queue is full Enqueue blocks up to block waiting for the drain to free room;
// if block elapses first the item is shed (dropped, counted) and returned with
// Accepted=false so the caller can NACK the producer. An item whose cost exceeds
// the whole capacity can never fit and is shed at once rather than blocking
// forever; a non-positive block makes Enqueue non-blocking (try-or-shed).
func (q *Queue[T]) Enqueue(val T, cost int, block time.Duration) Result {
	if cost < 1 {
		cost = 1
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		q.shed(cost)
		return Result{Throttled: true}
	}
	// Fast path: room is available right now.
	if q.cost+cost <= q.capacity {
		q.push(val, cost)
		return Result{Accepted: true, Throttled: q.cost >= q.highWater}
	}
	// Cannot ever fit, or the caller opted out of blocking: shed immediately.
	if cost > q.capacity || block <= 0 {
		q.shed(cost)
		return Result{Throttled: true}
	}

	// Full: block up to the deadline. sync.Cond has no timed Wait, so a timer
	// broadcasts notFull at the deadline to wake the waiter. The timer's callback
	// takes the same mutex, so it cannot fire between the deadline check below and
	// Wait()'s atomic unlock-and-suspend — there is no missed wakeup.
	start := time.Now()
	q.pressEvents++
	timer := time.AfterFunc(block, func() {
		q.mu.Lock()
		q.notFull.Broadcast()
		q.mu.Unlock()
	})
	for q.cost+cost > q.capacity && !q.closed {
		if time.Since(start) >= block {
			timer.Stop()
			q.shed(cost)
			return Result{Throttled: true, Blocked: time.Since(start)}
		}
		q.notFull.Wait()
	}
	timer.Stop()
	waited := time.Since(start)
	if q.closed {
		q.shed(cost)
		return Result{Throttled: true, Blocked: waited}
	}
	q.push(val, cost)
	return Result{Accepted: true, Throttled: q.cost >= q.highWater, Blocked: waited}
}

// Dequeue removes and returns the next item in FIFO order, blocking until one is
// available. ok is false only once the queue is closed and fully drained, which
// is the signal for a drain worker to exit.
func (q *Queue[T]) Dequeue() (val T, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.count == 0 && !q.closed {
		q.notEmpty.Wait()
	}
	if q.count == 0 {
		var zero T
		return zero, false
	}
	e := q.buf[q.head]
	q.buf[q.head] = entry[T]{} // drop the reference so the value can be collected
	q.head = (q.head + 1) % len(q.buf)
	q.count--
	q.cost -= e.cost
	q.dequeued += int64(e.cost)
	q.notFull.Broadcast()
	return e.val, true
}

// Close marks the queue closed. Already-queued items stay dequeuable (a drain
// worker finishes them before Dequeue reports ok=false); further Enqueues shed.
// All blocked producers and drains are woken.
func (q *Queue[T]) Close() {
	q.mu.Lock()
	q.closed = true
	q.notEmpty.Broadcast()
	q.notFull.Broadcast()
	q.mu.Unlock()
}

// push adds an item; callers hold mu and have verified room.
func (q *Queue[T]) push(val T, cost int) {
	if q.count == len(q.buf) {
		q.grow()
	}
	q.buf[(q.head+q.count)%len(q.buf)] = entry[T]{val: val, cost: cost}
	q.count++
	q.cost += cost
	if q.cost > q.maxDepth {
		q.maxDepth = q.cost
	}
	q.enqueued += int64(cost)
	q.notEmpty.Signal()
}

// shed records a dropped item; callers hold mu.
func (q *Queue[T]) shed(cost int) {
	q.dropped += int64(cost)
	q.shedEvents++
}

// RecordShed folds a shed of cost samples that was decided *before* the queue — by an
// upstream admission Shaper (ADR-027) — into the queue's grand-total drop counter, so
// meridian_dropped_samples_total stays the authoritative total across both the uniform
// queue shed and priority/fair-share admission. It accounts samples only (it does not
// touch the buffer or the per-Enqueue shedEvents tally, which counts enqueue attempts);
// the dimensional breakdown by class/series lives in the Shaper.
func (q *Queue[T]) RecordShed(cost int) {
	if cost < 1 {
		cost = 1
	}
	q.mu.Lock()
	q.dropped += int64(cost)
	q.mu.Unlock()
}

// grow doubles the ring buffer, re-linearising items from head; callers hold mu.
func (q *Queue[T]) grow() {
	n := len(q.buf)
	if n == 0 {
		q.buf = make([]entry[T], 4)
		return
	}
	nb := make([]entry[T], n*2)
	for i := 0; i < q.count; i++ {
		nb[i] = q.buf[(q.head+i)%n]
	}
	q.buf = nb
	q.head = 0
}

// Depth returns the cost currently resident in the queue.
func (q *Queue[T]) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cost
}

// Capacity returns the maximum resident cost. It is fixed at construction.
func (q *Queue[T]) Capacity() int { return q.capacity }

// Stats is a consistent snapshot of the queue's gauges and cumulative counters.
type Stats struct {
	Depth         int // resident cost (gauge)
	Capacity      int // max resident cost (gauge)
	HighWatermark int // throttle threshold (gauge)
	MaxDepth      int // highest resident cost ever observed

	Enqueued           int64 // cumulative cost accepted
	Dequeued           int64 // cumulative cost drained
	DroppedSamples     int64 // cumulative cost shed
	ShedEvents         int64 // cumulative shed Enqueue calls
	BackpressureEvents int64 // cumulative blocking Enqueue calls
}

// Stats returns a consistent snapshot taken under the queue lock.
func (q *Queue[T]) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{
		Depth:              q.cost,
		Capacity:           q.capacity,
		HighWatermark:      q.highWater,
		MaxDepth:           q.maxDepth,
		Enqueued:           q.enqueued,
		Dequeued:           q.dequeued,
		DroppedSamples:     q.dropped,
		ShedEvents:         q.shedEvents,
		BackpressureEvents: q.pressEvents,
	}
}
