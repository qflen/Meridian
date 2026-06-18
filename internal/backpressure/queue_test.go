package backpressure

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueFIFOOrder(t *testing.T) {
	q := New[int](100, 100)
	for i := 0; i < 5; i++ {
		if r := q.Enqueue(i, 1, 0); !r.Accepted {
			t.Fatalf("enqueue %d shed unexpectedly", i)
		}
	}
	for i := 0; i < 5; i++ {
		v, ok := q.Dequeue()
		if !ok || v != i {
			t.Fatalf("dequeue %d: got (%d,%v)", i, v, ok)
		}
	}
}

// TestQueueShedsPastDeadline proves the core shed path: with the queue full and
// no drain, an Enqueue blocks for ~the deadline and then sheds, incrementing the
// drop counters by exactly the item's cost.
func TestQueueShedsPastDeadline(t *testing.T) {
	q := New[int](10, 10)
	if r := q.Enqueue(1, 10, 0); !r.Accepted { // fill to capacity
		t.Fatal("initial fill shed")
	}

	start := time.Now()
	r := q.Enqueue(2, 4, 50*time.Millisecond)
	elapsed := time.Since(start)

	if r.Accepted {
		t.Fatal("expected the item to be shed when the queue stays full")
	}
	if !r.Throttled {
		t.Fatal("a shed item must report Throttled")
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("Enqueue returned before the block deadline (%v); it did not apply backpressure", elapsed)
	}
	st := q.Stats()
	if st.DroppedSamples != 4 {
		t.Fatalf("DroppedSamples = %d, want 4", st.DroppedSamples)
	}
	if st.ShedEvents != 1 {
		t.Fatalf("ShedEvents = %d, want 1", st.ShedEvents)
	}
	if st.BackpressureEvents != 1 {
		t.Fatalf("BackpressureEvents = %d, want 1", st.BackpressureEvents)
	}
	if st.Depth != 10 {
		t.Fatalf("Depth = %d, want 10 (the shed item must not have entered)", st.Depth)
	}
}

// TestQueueBlocksThenAcceptsOnDrain proves backpressure followed by recovery: a
// full queue blocks the producer, and once the consumer frees room the same
// Enqueue succeeds rather than shedding.
func TestQueueBlocksThenAcceptsOnDrain(t *testing.T) {
	q := New[int](10, 10)
	q.Enqueue(1, 10, 0) // full

	go func() {
		time.Sleep(30 * time.Millisecond)
		q.Dequeue() // free room
	}()

	start := time.Now()
	r := q.Enqueue(2, 5, time.Second)
	if !r.Accepted {
		t.Fatal("expected the item to be accepted once the consumer freed room")
	}
	if r.Blocked < 20*time.Millisecond {
		t.Fatalf("Blocked = %v, expected to have waited for the drain", r.Blocked)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("Enqueue did not wake promptly when room freed")
	}
}

// TestQueueOversizedItemShedImmediately: an item larger than the whole capacity
// can never fit, so it is shed at once instead of blocking for the full deadline.
func TestQueueOversizedItemShedImmediately(t *testing.T) {
	q := New[int](10, 10)
	start := time.Now()
	r := q.Enqueue(1, 25, time.Second)
	if r.Accepted {
		t.Fatal("an item larger than capacity must be shed")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("an unfittable item must shed immediately, not block")
	}
	if q.Stats().DroppedSamples != 25 {
		t.Fatalf("DroppedSamples = %d, want 25", q.Stats().DroppedSamples)
	}
}

func TestQueueThrottleAtHighWater(t *testing.T) {
	q := New[int](10, 5)
	for i := 0; i < 4; i++ {
		if r := q.Enqueue(i, 1, 0); r.Throttled {
			t.Fatalf("enqueue %d throttled below the high-water mark (depth %d, hw 5)", i, i+1)
		}
	}
	if r := q.Enqueue(99, 1, 0); !r.Throttled { // depth becomes 5 == hw
		t.Fatal("enqueue at the high-water mark must report Throttled")
	}
}

// TestQueueCloseDrains: a closed queue still yields its buffered items before
// signalling exhaustion, and later enqueues shed.
func TestQueueCloseDrains(t *testing.T) {
	q := New[int](100, 100)
	for i := 0; i < 3; i++ {
		q.Enqueue(i, 1, 0)
	}
	q.Close()

	if r := q.Enqueue(42, 1, time.Second); r.Accepted {
		t.Fatal("enqueue after Close must shed")
	}
	for i := 0; i < 3; i++ {
		if v, ok := q.Dequeue(); !ok || v != i {
			t.Fatalf("drain after close: got (%d,%v), want (%d,true)", v, ok, i)
		}
	}
	if _, ok := q.Dequeue(); ok {
		t.Fatal("Dequeue on a closed, drained queue must report ok=false")
	}
}

// TestQueueBoundedUnderConcurrency is the central memory-safety proof: many
// producers hammer a small queue while a slow consumer drains it. Depth must
// never exceed capacity, no accepted item may be lost, and every offered item is
// accounted for as either accepted-and-drained or shed. Run with -race.
func TestQueueBoundedUnderConcurrency(t *testing.T) {
	const (
		capacity  = 100
		producers = 8
		perProd   = 2000
	)
	q := New[int](capacity, 75)

	var dequeuedCost int64
	stop := make(chan struct{})
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			v, ok := q.Dequeue()
			if !ok {
				return
			}
			dequeuedCost += int64(v) // the enqueued value is its own cost
			select {
			case <-stop:
				// Keep draining quickly until closed+empty.
			default:
				time.Sleep(50 * time.Microsecond) // slow consumer ⇒ the queue fills
			}
		}
	}()

	var offered, accepted int64
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perProd; i++ {
				cost := 1 + i%5
				atomic.AddInt64(&offered, int64(cost))
				if q.Enqueue(cost, cost, 2*time.Millisecond).Accepted {
					atomic.AddInt64(&accepted, int64(cost))
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	q.Close()
	<-consumerDone

	st := q.Stats()
	if st.MaxDepth > capacity {
		t.Fatalf("MaxDepth = %d exceeded capacity %d — queue was not bounded", st.MaxDepth, capacity)
	}
	if st.Depth != 0 {
		t.Fatalf("Depth = %d after full drain, want 0", st.Depth)
	}
	if accepted+st.DroppedSamples != offered {
		t.Fatalf("conservation broken: accepted(%d) + dropped(%d) = %d != offered %d",
			accepted, st.DroppedSamples, accepted+st.DroppedSamples, offered)
	}
	if dequeuedCost != accepted {
		t.Fatalf("drained cost %d != accepted cost %d — an accepted item was lost", dequeuedCost, accepted)
	}
	if st.Enqueued != accepted {
		t.Fatalf("Enqueued counter %d != accepted %d", st.Enqueued, accepted)
	}
	// The slow consumer must have forced real backpressure and shedding.
	if st.DroppedSamples == 0 {
		t.Fatal("expected the slow consumer to force some shedding")
	}
	if st.BackpressureEvents == 0 {
		t.Fatal("expected the slow consumer to force some blocking (backpressure)")
	}
}
