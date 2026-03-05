package backpressure

import (
	"sync"
	"testing"
	"time"
)

// highLowConfig is a two-class priority setup used across the priority tests: a "high"
// class keyed on label class=high may fill the whole queue, while everything else falls
// to a default class capped at 40% of capacity.
func highLowConfig() ShaperConfig {
	return ShaperConfig{
		Classes: []ClassRule{
			{Name: "high", Label: "class", Value: "high", Ceiling: 1.0},
			{Name: "default", Ceiling: 0.4},
		},
	}
}

// TestShaperPriorityShedsLowBeforeHigh proves the core priority guarantee: as the queue
// fills, a low-priority (default) series is shed once it would push past its 40% ceiling
// while a high-priority series is still admitted all the way to capacity.
func TestShaperPriorityShedsLowBeforeHigh(t *testing.T) {
	s := NewShaper(100, highLowConfig())

	// At depth 50 (half full) the default class is already over its 40-sample ceiling.
	if d := s.Admit("m", map[string]string{"class": "low"}, 1, 50); d.Admit {
		t.Fatal("low-priority series admitted past its ceiling at depth 50")
	} else if d.Reason != DropPriority {
		t.Fatalf("expected DropPriority, got reason %v", d.Reason)
	}
	// A high-priority series at the same depth is protected: its ceiling is the whole queue.
	if d := s.Admit("m", map[string]string{"class": "high"}, 1, 50); !d.Admit {
		t.Fatal("high-priority series shed while below capacity")
	}
	// High priority is only shed when the queue is genuinely full.
	if d := s.Admit("m", map[string]string{"class": "high"}, 1, 100); d.Admit {
		t.Fatal("high-priority series admitted past full capacity")
	}

	st := s.Stats()
	high := classByName(t, st, "high")
	def := classByName(t, st, "default")
	if def.DroppedPriority == 0 {
		t.Fatal("expected the default class to record a priority drop")
	}
	if high.DroppedPriority != 1 {
		t.Fatalf("high class priority drops = %d, want 1 (only the at-capacity drop)", high.DroppedPriority)
	}
	if high.Admitted != 1 {
		t.Fatalf("high class admitted = %d, want 1", high.Admitted)
	}
}

// TestShaperBelowCeilingAdmitsAll confirms the gates are inert while there is room: with
// the queue near empty every class is admitted, so the shaper never sheds traffic the
// queue could have held.
func TestShaperBelowCeilingAdmitsAll(t *testing.T) {
	s := NewShaper(100, highLowConfig())
	for _, class := range []string{"high", "low", "other"} {
		if d := s.Admit("m", map[string]string{"class": class}, 1, 0); !d.Admit {
			t.Fatalf("series class=%s shed with an empty queue", class)
		}
	}
	if st := s.Stats(); st.TotalDropped != 0 {
		t.Fatalf("dropped %d with room to spare, want 0", st.TotalDropped)
	}
}

// TestShaperFairShareContainsFlood proves per-series fairness: under contention a series
// that floods past its token budget is shed while a well-behaved series in another shard
// keeps being admitted.
func TestShaperFairShareContainsFlood(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	cfg := ShaperConfig{
		ContentionFraction: 0.5,
		FairShareRate:      1,  // 1 sample/sec refill — negligible over the test
		FairShareBurst:     10, // each series may spend 10 before metering bites
	}
	s := newShaperClock(100, cfg, clk.now)

	const depth = 80 // above the 50-sample contention threshold
	var hotAdmitted, hotShed, coolAdmitted int
	for i := 0; i < 200; i++ {
		if s.Admit("hot", map[string]string{"id": "A"}, 1, depth).Admit {
			hotAdmitted++
		} else {
			hotShed++
		}
		// The cool series sends one sample for every 40 hot samples — well under burst.
		if i%40 == 0 {
			if s.Admit("cool", map[string]string{"id": "B"}, 1, depth).Admit {
				coolAdmitted++
			}
		}
	}

	if hotShed == 0 {
		t.Fatal("the flooding series was never shed — fair share did not engage")
	}
	if hotAdmitted > 15 {
		t.Fatalf("hot series admitted %d, expected to be capped near its burst of 10", hotAdmitted)
	}
	if coolAdmitted != 5 {
		t.Fatalf("well-behaved series admitted %d/5 — it was starved by the flood", coolAdmitted)
	}
}

// TestShaperFairShareInertBelowContention confirms fair share does not meter a high-rate
// series while the queue is uncontended: even a flood is admitted when there is room.
func TestShaperFairShareInertBelowContention(t *testing.T) {
	cfg := ShaperConfig{ContentionFraction: 0.8, FairShareRate: 1, FairShareBurst: 5}
	s := NewShaper(100, cfg)
	for i := 0; i < 100; i++ {
		if d := s.Admit("hot", map[string]string{"id": "A"}, 1, 10); !d.Admit { // depth 10 << 80
			t.Fatalf("series shed below the contention threshold at offer %d", i)
		}
	}
}

// TestShaperFairShareRefills proves the token bucket recovers over time: a series drained
// to empty under contention is admitted again once the clock advances enough to refill.
func TestShaperFairShareRefills(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	cfg := ShaperConfig{ContentionFraction: 0.5, FairShareRate: 10, FairShareBurst: 10}
	s := newShaperClock(100, cfg, clk.now)

	// Drain the burst, then confirm the next offer is metered out.
	for i := 0; i < 10; i++ {
		if !s.Admit("x", nil, 1, 80).Admit {
			t.Fatalf("offer %d shed before the burst was spent", i)
		}
	}
	if s.Admit("x", nil, 1, 80).Admit {
		t.Fatal("series admitted past an exhausted token bucket")
	}
	// Advance one second: 10 tokens/sec refills the bucket.
	clk.advance(time.Second)
	if !s.Admit("x", nil, 1, 80).Admit {
		t.Fatal("series not admitted after the bucket refilled")
	}
}

// TestShaperBoundedMemory is the memory-safety proof: flooding the shaper with a huge
// number of *distinct* series must not grow its per-series state. The token-bucket and
// metric-bucket arrays are fixed at construction, so the footprint is independent of
// cardinality.
func TestShaperBoundedMemory(t *testing.T) {
	cfg := ShaperConfig{
		ContentionFraction: 0.1,
		FairShareRate:      1,
		FairShareBurst:     1,
		Shards:             256,
		MetricBuckets:      8,
	}
	s := NewShaper(100, cfg)
	for i := 0; i < 100_000; i++ {
		// Each series identity is unique, so a map-backed design would grow without bound.
		s.Admit("m", map[string]string{"series": uniqueKey(i)}, 1, 90)
	}
	if got := s.Shards(); got != 256 {
		t.Fatalf("shard count = %d after 100k distinct series, want a fixed 256", got)
	}
	st := s.Stats()
	if len(st.BucketDrops) != 8 {
		t.Fatalf("metric buckets = %d, want a fixed 8", len(st.BucketDrops))
	}
	// Every offer is accounted as admitted or dropped — nothing is lost.
	if st.TotalAdmitted+st.TotalDropped != 100_000 {
		t.Fatalf("accounting lost samples: admitted %d + dropped %d != 100000", st.TotalAdmitted, st.TotalDropped)
	}
}

// TestShaperClassificationOrder verifies first-match-wins priority ordering and the
// metric-name pseudo-label matcher.
func TestShaperClassificationOrder(t *testing.T) {
	s := NewShaper(100, ShaperConfig{
		Classes: []ClassRule{
			{Name: "critical", Label: "__name__", Value: "heartbeat", Ceiling: 1.0},
			{Name: "tagged", Label: "tier", Value: "gold", Ceiling: 0.8},
			{Name: "default", Ceiling: 0.5},
		},
	})
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"heartbeat", map[string]string{"tier": "gold"}, "critical"}, // name match wins over label match
		{"cpu", map[string]string{"tier": "gold"}, "tagged"},
		{"cpu", map[string]string{"tier": "bronze"}, "default"},
		{"cpu", nil, "default"},
	}
	for _, c := range cases {
		idx := s.classify(c.name, c.labels)
		if got := s.classes[idx].name; got != c.want {
			t.Fatalf("classify(%q,%v) = %q, want %q", c.name, c.labels, got, c.want)
		}
	}
}

// TestShaperSynthesizesDefault confirms a config with no catch-all still classifies
// unmatched series, into a synthesised full-capacity default.
func TestShaperSynthesizesDefault(t *testing.T) {
	s := NewShaper(100, ShaperConfig{
		Classes: []ClassRule{{Name: "high", Label: "class", Value: "high", Ceiling: 1.0}},
	})
	// An unmatched series must be admitted up to full capacity (default ceiling 1.0).
	if d := s.Admit("m", map[string]string{"class": "low"}, 1, 99); !d.Admit {
		t.Fatal("unmatched series shed below capacity — synthesised default is not full-capacity")
	}
	if d := s.Admit("m", map[string]string{"class": "low"}, 1, 100); d.Admit {
		t.Fatal("unmatched series admitted past full capacity")
	}
}

// TestShaperHashOrderIndependent guards the series hash against label map iteration
// order: the same label set must always land in the same shard/bucket.
func TestShaperHashOrderIndependent(t *testing.T) {
	s := NewShaper(100, ShaperConfig{})
	a := s.hash("m", map[string]string{"a": "1", "b": "2", "c": "3"})
	b := s.hash("m", map[string]string{"c": "3", "a": "1", "b": "2"})
	if a != b {
		t.Fatalf("hash depends on label order: %d != %d", a, b)
	}
	if s.hash("m", map[string]string{"a": "1"}) == s.hash("m", map[string]string{"a": "2"}) {
		t.Fatal("hash collapsed two distinct label values")
	}
}

// TestShaperNilDisabled documents that a nil *Shaper is the disabled state with a
// zero-value snapshot, so callers can treat "no shaper" uniformly.
func TestShaperNilDisabled(t *testing.T) {
	var s *Shaper
	if st := s.Stats(); st.TotalAdmitted != 0 || st.TotalDropped != 0 || len(st.Classes) != 0 {
		t.Fatalf("nil shaper Stats not zero-valued: %+v", st)
	}
}

// TestShaperConcurrentAdmit exercises Admit from many goroutines under -race to prove the
// token buckets and counters are safe, and that accounting is exact under concurrency.
func TestShaperConcurrentAdmit(t *testing.T) {
	cfg := ShaperConfig{ContentionFraction: 0.5, FairShareRate: 100, FairShareBurst: 50, Shards: 64}
	s := NewShaper(100, cfg)
	const goroutines, each = 16, 1000
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				s.Admit("m", map[string]string{"g": uniqueKey(g)}, 1, 80)
			}
		}(g)
	}
	wg.Wait()
	st := s.Stats()
	if st.TotalAdmitted+st.TotalDropped != goroutines*each {
		t.Fatalf("conservation broken under concurrency: admitted %d + dropped %d != %d",
			st.TotalAdmitted, st.TotalDropped, goroutines*each)
	}
}

// --- helpers ---

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func classByName(t *testing.T, st ShaperStats, name string) ClassStat {
	t.Helper()
	for _, c := range st.Classes {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("class %q not found in stats", name)
	return ClassStat{}
}

func uniqueKey(i int) string {
	const digits = "0123456789abcdef"
	var b [16]byte
	n := 0
	for {
		b[n] = digits[i&0xf]
		n++
		i >>= 4
		if i == 0 {
			break
		}
	}
	return string(b[:n])
}
