package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/backpressure"
)

// recordWriter captures every request it is handed and completes immediately, so a
// test can assert exactly which series survived admission filtering.
type recordWriter struct {
	mu   sync.Mutex
	reqs []WriteRequest
}

func (w *recordWriter) Write(_ context.Context, req WriteRequest) (*WriteResponse, error) {
	w.mu.Lock()
	w.reqs = append(w.reqs, req)
	w.mu.Unlock()
	return &WriteResponse{SamplesIngested: int64(sampleCount(req))}, nil
}

func (w *recordWriter) written() []WriteRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]WriteRequest(nil), w.reqs...)
}

func series(class string, n int) TimeSeries {
	ts := TimeSeries{Name: "m", Labels: []Label{{Name: "class", Value: class}}}
	for i := 0; i < n; i++ {
		ts.Samples = append(ts.Samples, Sample{TimestampMs: int64(i+1) * 1000, Value: 1})
	}
	return ts
}

// TestWritePoolAdmissionTrimsLowPrioritySeries proves the service write path applies
// admission per-series within a request: a low-priority series (whose cost exceeds the
// default class's tiny ceiling) is dropped while the high-priority series in the same
// request is written through, and the drop is attributed by class and folded into the
// queue grand-total.
func TestWritePoolAdmissionTrimsLowPrioritySeries(t *testing.T) {
	w := &recordWriter{}
	adm := &backpressure.ShaperConfig{
		Classes: []backpressure.ClassRule{
			{Name: "high", Label: "class", Value: "high", Ceiling: 1.0},
			{Name: "default", Ceiling: 0.001}, // ceiling rounds to the 1-sample floor
		},
	}
	pool := NewWritePool(w, PoolOptions{Capacity: 100, HighWatermark: 80, BlockDeadline: time.Second, Workers: 2, Admission: adm})
	defer pool.Close()

	// A multi-series request: the high series (cost 3) fits; the low series (cost 3 >
	// the 1-sample default ceiling) is shed by priority.
	req := WriteRequest{TimeSeries: []TimeSeries{series("high", 3), series("low", 3)}}
	resp, r, err := pool.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("submit with a surviving high-priority series should not error: %v", err)
	}
	if !r.Accepted {
		t.Fatal("expected the trimmed request to be accepted")
	}
	if resp.SamplesIngested != 3 {
		t.Fatalf("SamplesIngested = %d, want 3 (only the high series)", resp.SamplesIngested)
	}

	wr := w.written()
	if len(wr) != 1 || len(wr[0].TimeSeries) != 1 || wr[0].TimeSeries[0].Labels[0].Value != "high" {
		t.Fatalf("writer received %+v, want exactly the high-priority series", wr)
	}
	st := pool.AdmissionStats()
	if got := classAdmStat(t, st, "high").Admitted; got != 3 {
		t.Fatalf("high admitted = %d, want 3", got)
	}
	if got := classAdmStat(t, st, "default").DroppedPriority; got != 3 {
		t.Fatalf("default droppedPriority = %d, want 3", got)
	}
	if d := pool.Stats().DroppedSamples; d != 3 {
		t.Fatalf("queue grand-total drops = %d, want 3 (admission folds in via RecordShed)", d)
	}
}

// TestWritePoolAdmissionAllShedReturnsErrShed: a request whose only series is shed by
// admission is rejected with ErrShed (mapped to 429 / NACK), and nothing is written.
func TestWritePoolAdmissionAllShedReturnsErrShed(t *testing.T) {
	w := &recordWriter{}
	adm := &backpressure.ShaperConfig{
		Classes: []backpressure.ClassRule{{Name: "default", Ceiling: 0.001}},
	}
	pool := NewWritePool(w, PoolOptions{Capacity: 100, HighWatermark: 80, BlockDeadline: time.Second, Workers: 1, Admission: adm})
	defer pool.Close()

	_, r, err := pool.Submit(context.Background(), WriteRequest{TimeSeries: []TimeSeries{series("low", 5)}})
	if !errors.Is(err, ErrShed) {
		t.Fatalf("expected ErrShed when every series is shed, got %v", err)
	}
	if r.Accepted {
		t.Fatal("an all-shed request must not be accepted")
	}
	if len(w.written()) != 0 {
		t.Fatal("nothing should be written when the whole request is shed")
	}
	if d := pool.Stats().DroppedSamples; d != 5 {
		t.Fatalf("queue grand-total drops = %d, want 5", d)
	}
}

// TestWritePoolAdmissionFairShareThrottlesFlood proves per-series fairness on the
// service path: with a stalled worker forcing contention, a flooding series is throttled
// to its token budget (its submits shed) while a well-behaved series is never shed.
func TestWritePoolAdmissionFairShareThrottlesFlood(t *testing.T) {
	w := newParkWriter()
	adm := &backpressure.ShaperConfig{
		ContentionFraction: 0,
		FairShareRate:      1,
		FairShareBurst:     10,
		Shards:             512,
	}
	// Huge capacity so the queue itself never sheds: only fair-share admission does.
	pool := NewWritePool(w, PoolOptions{Capacity: 1_000_000, HighWatermark: 999_999, BlockDeadline: 50 * time.Millisecond, Workers: 1, Admission: adm})
	defer func() { w.release(); pool.Close() }()

	// Seed two resident jobs and wait for the worker to park, so the flood sees depth ≥
	// the contention threshold.
	var seed sync.WaitGroup
	for i := 0; i < 2; i++ {
		seed.Add(1)
		go func(i int) { defer seed.Done(); pool.Submit(context.Background(), oneSeries("warm", i)) }(i)
	}
	waitFor(t, time.Second, func() bool { return w.parked() >= 1 })

	const flood = 800
	errsA := make(chan error, flood)
	errsB := make(chan error, 5)
	var wg sync.WaitGroup
	for i := 0; i < flood; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := pool.Submit(context.Background(), oneSeries("A", i))
			errsA <- err
		}(i)
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := pool.Submit(context.Background(), oneSeries("B", i))
			errsB <- err
		}(i)
	}

	// Wait until the flood has driven real fair-share shedding, then release everything.
	waitFor(t, 2*time.Second, func() bool { return pool.AdmissionStats().TotalDropped >= flood/2 })
	w.release()
	wg.Wait()
	close(errsA)
	close(errsB)

	var shedA int
	for err := range errsA {
		if errors.Is(err, ErrShed) {
			shedA++
		}
	}
	for err := range errsB {
		if errors.Is(err, ErrShed) {
			t.Fatal("a well-behaved series was shed — fair share did not protect it")
		}
	}
	if shedA == 0 {
		t.Fatal("the flooding series was never shed — fair share did not engage")
	}
	if d := pool.Stats().DroppedSamples; d != pool.AdmissionStats().TotalDropped {
		t.Fatalf("queue drops %d != shaper drops %d", d, pool.AdmissionStats().TotalDropped)
	}
}

// TestWritePoolAdmissionDisabledUnchanged confirms a pool with no admission config keeps
// the original behaviour and reports an empty admission snapshot.
func TestWritePoolAdmissionDisabledUnchanged(t *testing.T) {
	w := newBlockingWriter()
	w.release()
	pool := NewWritePool(w, PoolOptions{Capacity: 100, HighWatermark: 80, BlockDeadline: time.Second, Workers: 2})
	defer pool.Close()

	if _, _, err := pool.Submit(context.Background(), oneSample(1000)); err != nil {
		t.Fatal(err)
	}
	if st := pool.AdmissionStats(); st.TotalAdmitted != 0 || st.TotalDropped != 0 || len(st.Classes) != 0 {
		t.Fatalf("admission snapshot not empty with shaping disabled: %+v", st)
	}
}

// --- helpers ---

// parkWriter increments a counter before blocking, so a test can detect that a worker
// has parked (unlike blockingWriter, which counts only after release).
type parkWriter struct {
	started int64
	open    chan struct{}
	once    sync.Once
}

func newParkWriter() *parkWriter { return &parkWriter{open: make(chan struct{})} }

func (w *parkWriter) Write(_ context.Context, req WriteRequest) (*WriteResponse, error) {
	atomic.AddInt64(&w.started, 1)
	<-w.open
	return &WriteResponse{SamplesIngested: int64(sampleCount(req))}, nil
}

func (w *parkWriter) release()      { w.once.Do(func() { close(w.open) }) }
func (w *parkWriter) parked() int64 { return atomic.LoadInt64(&w.started) }

func oneSeries(id string, ts int) WriteRequest {
	return WriteRequest{TimeSeries: []TimeSeries{{
		Name:    "m",
		Labels:  []Label{{Name: "id", Value: id}},
		Samples: []Sample{{TimestampMs: int64(ts+1) * 1000, Value: 1}},
	}}}
}

func classAdmStat(t *testing.T, st backpressure.ShaperStats, name string) backpressure.ClassStat {
	t.Helper()
	for _, c := range st.Classes {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("class %q absent from admission stats", name)
	return backpressure.ClassStat{}
}
