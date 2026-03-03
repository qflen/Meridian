package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingWriter is a Writer whose Write blocks until released, used to stall the
// pool workers so the bounded queue fills. release is idempotent.
type blockingWriter struct {
	open  chan struct{}
	once  sync.Once
	calls int64
}

func newBlockingWriter() *blockingWriter { return &blockingWriter{open: make(chan struct{})} }

func (b *blockingWriter) Write(ctx context.Context, req WriteRequest) (*WriteResponse, error) {
	<-b.open
	atomic.AddInt64(&b.calls, 1)
	return &WriteResponse{SamplesIngested: int64(sampleCount(req))}, nil
}

func (b *blockingWriter) release() { b.once.Do(func() { close(b.open) }) }

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

func oneSample(ts int64) WriteRequest {
	return WriteRequest{TimeSeries: []TimeSeries{{Name: "m", Samples: []Sample{{TimestampMs: ts, Value: 1}}}}}
}

// TestWritePoolBackpressureAndShedding is the core proof for the ingestor path: a
// stalled downstream saturates the bounded queue, Submit blocks then sheds with
// ErrShed past the deadline, the queue never exceeds capacity, drop counts are
// exact, and once the downstream recovers writes succeed and shedding stops.
func TestWritePoolBackpressureAndShedding(t *testing.T) {
	w := newBlockingWriter()
	pool := NewWritePool(w, PoolOptions{Capacity: 5, HighWatermark: 4, BlockDeadline: 20 * time.Millisecond, Workers: 1})
	defer func() {
		w.release()
		pool.Close()
	}()

	const offered = 40
	errs := make(chan error, offered)
	var wg sync.WaitGroup
	for i := 0; i < offered; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := pool.Submit(context.Background(), oneSample(int64(i+1)*1000))
			errs <- err
		}(i)
	}

	// Wait until the stalled worker has forced real shedding and blocking.
	waitFor(t, 2*time.Second, func() bool {
		st := pool.Stats()
		return st.ShedEvents >= 5 && st.BackpressureEvents >= 1
	})
	if st := pool.Stats(); st.MaxDepth > st.Capacity {
		t.Fatalf("max depth %d exceeded capacity %d — in-flight work not bounded", st.MaxDepth, st.Capacity)
	}

	// Release the downstream: accepted submits complete; all goroutines finish.
	w.release()
	wg.Wait()
	close(errs)

	var shed, ok int
	for err := range errs {
		switch {
		case errors.Is(err, ErrShed):
			shed++
		case err == nil:
			ok++
		default:
			t.Fatalf("unexpected Submit error: %v", err)
		}
	}
	if shed == 0 || ok == 0 {
		t.Fatalf("expected a mix of shed and accepted submits, got shed=%d ok=%d", shed, ok)
	}
	if shed+ok != offered {
		t.Fatalf("shed(%d) + ok(%d) != offered %d", shed, ok, offered)
	}
	final := pool.Stats()
	if final.DroppedSamples != int64(shed) {
		t.Fatalf("dropped samples %d != shed submits %d (each carries 1 sample)", final.DroppedSamples, shed)
	}

	// Recovery: a fresh submit succeeds and adds no drops.
	before := final.DroppedSamples
	resp, _, err := pool.Submit(context.Background(), oneSample(99_000))
	if err != nil {
		t.Fatalf("post-recovery submit failed: %v", err)
	}
	if resp == nil || resp.SamplesIngested != 1 {
		t.Fatalf("post-recovery response = %+v", resp)
	}
	if d := pool.Stats().DroppedSamples; d != before {
		t.Fatalf("shedding continued after recovery: dropped %d -> %d", before, d)
	}
}

// TestWritePoolPreservesResult: an accepted submit returns the downstream's actual
// response (quorum result), so bounding the rate does not change write semantics.
func TestWritePoolPreservesResult(t *testing.T) {
	w := newBlockingWriter()
	w.release() // never blocks
	pool := NewWritePool(w, PoolOptions{Capacity: 100, HighWatermark: 80, BlockDeadline: time.Second, Workers: 4})
	defer pool.Close()

	req := WriteRequest{TimeSeries: []TimeSeries{{Name: "m", Samples: []Sample{{TimestampMs: 1000, Value: 1}, {TimestampMs: 2000, Value: 2}}}}}
	resp, r, err := pool.Submit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.SamplesIngested != 2 {
		t.Fatalf("SamplesIngested = %d, want 2", resp.SamplesIngested)
	}
	if !r.Accepted {
		t.Fatal("expected the submit to be accepted")
	}
}

// TestWritePoolContextCancel: a Submit whose context is cancelled while queued
// returns the context error rather than hanging.
func TestWritePoolContextCancel(t *testing.T) {
	w := newBlockingWriter() // stays blocked
	pool := NewWritePool(w, PoolOptions{Capacity: 10, HighWatermark: 8, BlockDeadline: time.Second, Workers: 1})
	defer func() {
		w.release()
		pool.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := pool.Submit(ctx, oneSample(1000))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Submit did not return after context cancellation")
	}
}
