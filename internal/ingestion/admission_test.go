package ingestion

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/backpressure"
	"github.com/meridiandb/meridian/internal/storage"
)

// controlSink is a batchSink that parks the drain on its first batch (so the queue
// depth is stable and observable) and records per-series delivery once released. It
// underpins the admission integration tests: the priority test reads the depth-driven
// admission counters while the drain is parked; the fair-share test releases the drain
// at the end and asserts which series' samples actually made it through.
type controlSink struct {
	mu     sync.Mutex
	counts map[string]int
	pulls  int64
	open   chan struct{}
	once   sync.Once
}

func newControlSink() *controlSink {
	return &controlSink{counts: make(map[string]int), open: make(chan struct{})}
}

func (s *controlSink) IngestBatch(b []storage.IngestSample) error {
	atomic.AddInt64(&s.pulls, 1)
	<-s.open // park until released
	s.mu.Lock()
	for _, x := range b {
		s.counts[x.Labels["id"]]++
	}
	s.mu.Unlock()
	return nil
}

func (s *controlSink) release()      { s.once.Do(func() { close(s.open) }) }
func (s *controlSink) pulled() int64 { return atomic.LoadInt64(&s.pulls) }

func (s *controlSink) count(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[id]
}

func (s *controlSink) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.counts {
		n += c
	}
	return n
}

func classStat(t *testing.T, st backpressure.ShaperStats, name string) backpressure.ClassStat {
	t.Helper()
	for _, c := range st.Classes {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("class %q absent from admission stats", name)
	return backpressure.ClassStat{}
}

// TestBatchWriterAdmissionPriorityShedsLowFirst proves the priority band end-to-end on
// the monolith ingest path: at a fixed queue depth a low-priority series is shed while a
// high-priority series keeps being admitted to full capacity, the counters attribute the
// drops by class, and the admission drops fold into the queue's grand-total counter.
func TestBatchWriterAdmissionPriorityShedsLowFirst(t *testing.T) {
	sink := newControlSink()
	adm := &backpressure.ShaperConfig{
		Classes: []backpressure.ClassRule{
			{Name: "high", Label: "class", Value: "high", Ceiling: 1.0}, // may fill the queue
			{Name: "default", Ceiling: 0.25},                            // 25% of 20 == 5 samples
		},
	}
	opts := QueueOptions{Capacity: 20, HighWatermark: 16, BlockDeadline: 10 * time.Millisecond, Admission: adm}
	bw := newBatchWriterSink(sink, 1, time.Hour, opts) // batchSize 1 ⇒ every Add enqueues
	defer func() { sink.release(); bw.Close() }()

	// Park the drain on a high-priority warmup so depth is stable (back to 0) for the rest.
	bw.Add("m", map[string]string{"class": "high", "id": "warm"}, 1, 1)
	waitFor(t, time.Second, func() bool { return sink.pulled() >= 1 })

	// Fill the default class's 25% band (5 samples), all admitted.
	for i := 0; i < 5; i++ {
		if r := bw.Add("m", map[string]string{"class": "low"}, int64(i), 1); r.Shed != 0 {
			t.Fatalf("low sample %d shed below its ceiling (depth still has room)", i)
		}
	}
	// Past the band, every further low-priority sample is shed by priority.
	var lowShed int64
	for i := 0; i < 20; i++ {
		lowShed += bw.Add("m", map[string]string{"class": "low"}, int64(i), 1).Shed
	}
	if lowShed != 20 {
		t.Fatalf("low-priority shed = %d, want 20 (its band is full)", lowShed)
	}
	// High priority keeps being admitted past the low ceiling, all the way to capacity.
	for i := 0; i < 15; i++ {
		if r := bw.Add("m", map[string]string{"class": "high"}, int64(i), 1); r.Shed != 0 {
			t.Fatalf("high sample %d shed while the queue was below capacity", i)
		}
	}

	st := bw.AdmissionStats()
	def := classStat(t, st, "default")
	if def.Admitted != 5 || def.DroppedPriority != 20 || def.DroppedFairShare != 0 {
		t.Fatalf("default class = %+v, want {Admitted:5 DroppedPriority:20 DroppedFairShare:0}", def)
	}
	high := classStat(t, st, "high")
	if high.Admitted != 16 || high.DroppedPriority != 0 { // 1 warmup + 15
		t.Fatalf("high class = %+v, want {Admitted:16 DroppedPriority:0}", high)
	}
	if st.TotalAdmitted != 21 || st.TotalDropped != 20 {
		t.Fatalf("totals = admitted %d / dropped %d, want 21 / 20", st.TotalAdmitted, st.TotalDropped)
	}
	q := bw.QueueStats()
	if q.DroppedSamples != 20 {
		t.Fatalf("queue grand-total drops = %d, want 20 (admission folds in via RecordShed)", q.DroppedSamples)
	}
	if q.Depth != 20 {
		t.Fatalf("queue depth = %d, want 20 (5 low + 15 high resident)", q.Depth)
	}
}

// TestBatchWriterAdmissionFairShareContainsFlood proves per-series fairness end-to-end: a
// single series flooding under contention is throttled to its token budget while a
// well-behaved series is fully admitted and delivered. The queue capacity is huge so the
// only shedding is fair-share, isolating the new behaviour from the uniform fallback.
func TestBatchWriterAdmissionFairShareContainsFlood(t *testing.T) {
	sink := newControlSink()
	adm := &backpressure.ShaperConfig{
		ContentionFraction: 0,  // meter as soon as anything is resident
		FairShareRate:      1,  // ~0 refill over the test window
		FairShareBurst:     10, // each series may spend 10 before metering bites
		Shards:             512,
	}
	opts := QueueOptions{Capacity: 100_000, HighWatermark: 99_999, BlockDeadline: 50 * time.Millisecond, Admission: adm}
	bw := newBatchWriterSink(sink, 1, time.Hour, opts)
	defer func() { sink.release(); bw.Close() }()

	// Park the drain and keep one sample resident so depth ≥ contention for the flood.
	bw.Add("m", map[string]string{"id": "warm"}, 1, 1)
	bw.Add("m", map[string]string{"id": "warm"}, 2, 1)
	waitFor(t, time.Second, func() bool { return sink.pulled() >= 1 })

	const flood = 1000
	for i := 0; i < flood; i++ {
		bw.Add("m", map[string]string{"id": "A"}, int64(i), 1) // hot series
	}
	for i := 0; i < 5; i++ {
		bw.Add("m", map[string]string{"id": "B"}, int64(i), 1) // well-behaved series
	}

	// Release the drain; everything the shaper admitted is delivered and recorded.
	admitted := bw.AdmissionStats().TotalAdmitted
	sink.release()
	waitFor(t, 2*time.Second, func() bool { return sink.total() == int(admitted) })

	if got := sink.count("B"); got != 5 {
		t.Fatalf("well-behaved series B delivered %d/5 — it was starved by the flood", got)
	}
	if got := sink.count("A"); got == 0 || got > 20 {
		t.Fatalf("hot series A delivered %d, expected throttling near its burst of 10", got)
	}
	st := bw.AdmissionStats()
	if st.TotalDropped < flood-50 {
		t.Fatalf("total fair-share drops = %d, expected ~%d from the flood", st.TotalDropped, flood)
	}
	q := bw.QueueStats()
	if q.DroppedSamples != st.TotalDropped {
		t.Fatalf("queue drops %d != shaper drops %d (RecordShed must fold them in)", q.DroppedSamples, st.TotalDropped)
	}
	if q.BackpressureEvents != 0 {
		t.Fatalf("queue blocked %d times — with huge capacity only fair-share should shed", q.BackpressureEvents)
	}
}

// TestBatchWriterAdmissionDisabledUnchanged confirms the default path is untouched: with
// no admission config the BatchWriter sheds uniformly exactly as before and reports an
// empty admission snapshot.
func TestBatchWriterAdmissionDisabledUnchanged(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	bw := NewBatchWriter(db, 10, time.Hour)
	defer bw.Close()

	bw.Add("m", map[string]string{"id": "A"}, 1000, 1)
	if st := bw.AdmissionStats(); st.TotalAdmitted != 0 || st.TotalDropped != 0 || len(st.Classes) != 0 {
		t.Fatalf("admission snapshot not empty with shaping disabled: %+v", st)
	}
}
