package backpressure

import (
	"sync"
	"sync/atomic"
	"time"
)

// A Shaper adds per-series fairness and priority-class admission on top of the
// uniform block-then-shed bound of a Queue (ADR-027). The Queue alone sheds the
// *next* arrival past the cap regardless of which series or how important it is, so
// a single hot/high-cardinality series can fill the queue and starve well-behaved
// ones, and a low-value series can crowd out a critical one. The Shaper is consulted
// *before* the Queue, on the same sample budget: under load it chooses shed victims
// by lowest priority and most-over-budget series, so the cap is spent on the traffic
// that matters.
//
// It layers two independent gates, both engaged only as the queue fills (there is no
// reason to be selective while there is room):
//
//   - Priority bands. Each series is classified into a priority class by a label (or
//     metric-name) match; the catch-all default class holds the rest. A class may
//     occupy at most its Ceiling fraction of the capacity, so reserving the top band
//     for higher classes sheds low priority first and protects high priority until the
//     queue is genuinely full. Classes are a small, configured set, so the class label
//     is bounded-cardinality.
//   - Per-series fair share. Above a contention threshold each series is metered by a
//     token bucket (rate + burst); a series that has burned its tokens is shed while
//     series still in budget pass. The buckets are a fixed array indexed by a hash of
//     the series identity — bounded memory regardless of cardinality — so a flood is
//     contained to the offending series' shard rather than charged to everyone.
//
// The Shaper never holds samples and is not the memory bound: it only decides what to
// offer the Queue, whose capacity remains the hard cap (depth ≤ capacity always). A
// shed decision here is folded into the Queue's grand-total drop counter by the caller
// (Queue.RecordShed) and additionally attributed by class, reason, and series bucket
// here, so /metrics can show *which* traffic was shed without unbounded cardinality.
// Within one series order is untouched (fairness is across series only), so the
// in-order, FIFO drain the out-of-order policy depends on (ADR-015) is preserved.
//
// The zero value is not usable; construct with NewShaper. A nil *Shaper is the
// disabled state — callers treat it as "admit everything" and fall back to the
// Queue's uniform shedding.
type Shaper struct {
	capacity      int
	contention    int  // depth at/above which fair-share buckets are enforced
	fairShare     bool // a positive rate enables the per-series token buckets
	rate          float64
	burst         float64
	metricBuckets uint64

	classes    []classRule
	defaultIdx int

	shards []tokenBucket // fixed-size; indexed by hash(series) — bounded memory

	// Cumulative counters. perClass and bucketDrops are fixed-size arrays sized at
	// construction, so accounting never grows with series cardinality.
	perClass      []classCounter
	bucketDrops   []atomic.Int64
	totalAdmitted atomic.Int64
	totalDropped  atomic.Int64

	now func() time.Time // injectable clock for deterministic token-bucket tests
}

// classRule is a compiled priority class: a matcher plus the absolute resident-cost
// ceiling derived from its capacity fraction.
type classRule struct {
	name        string
	label       string // "" => catch-all default; "__name__" matches the metric name
	value       string
	ceilingCost int
	isDefault   bool
}

func (c classRule) match(name string, labels map[string]string) bool {
	if c.label == metricNameLabel {
		return name == c.value
	}
	return labels[c.label] == c.value
}

// metricNameLabel is the pseudo-label that matches a series' metric name, mirroring
// the Prometheus convention so a class can be keyed on the name as well as a label.
const metricNameLabel = "__name__"

// classCounter holds the per-class cumulative tallies. atomic.Int64 is not copyable,
// so these live in a fixed slice and are only ever indexed, never moved.
type classCounter struct {
	admitted        atomic.Int64
	droppedPriority atomic.Int64
	droppedFair     atomic.Int64
}

// tokenBucket is one fair-share shard: a classic rate+burst bucket refilled lazily
// from the wall clock on each consult. Many series may hash to one shard; with enough
// shards a hot series dominates its own shard and is metered there while series in
// other shards are unaffected (colliding hot series share a budget — a bounded-memory
// trade-off, see ADR-027).
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// take refills the bucket for the elapsed time (capped at burst) and consumes cost
// tokens if available, reporting whether the series may pass under fair share.
func (b *tokenBucket) take(cost, rate, burst float64, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() { // first use: start full so a quiet series isn't penalised
		b.last = now
		b.tokens = burst
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * rate
		if b.tokens > burst {
			b.tokens = burst
		}
		b.last = now
	}
	if b.tokens >= cost {
		b.tokens -= cost
		return true
	}
	return false
}

// ShaperConfig configures a Shaper. The zero value (no classes, zero rate) yields a
// shaper with a single full-capacity default class and no fair-share metering, i.e.
// equivalent to the Queue's uniform shedding — enabling the layer is opt-in per knob.
type ShaperConfig struct {
	// Classes are the priority classes in descending priority order: the first whose
	// matcher matches a series wins. Exactly one catch-all (empty Label) is the default
	// for unmatched series; if none is given, a full-capacity default is synthesised.
	Classes []ClassRule
	// ContentionFraction is the depth/capacity at or above which per-series fair-share
	// metering engages. Below it there is room, so every series is admitted.
	ContentionFraction float64
	// FairShareRate is the per-series token refill in samples/sec. Zero disables the
	// fair-share gate, leaving only priority bands.
	FairShareRate float64
	// FairShareBurst is the per-series token-bucket depth in samples (the burst a quiet
	// series may spend at once). Defaults to FairShareRate when unset.
	FairShareBurst float64
	// Shards is the number of fair-share token buckets (bounded memory). More shards
	// means distinct hot series collide less often. Defaults to 4096.
	Shards int
	// MetricBuckets is the number of per-series shed counters exposed as metrics
	// (bounded cardinality — the series space is hashed into this many buckets).
	// Defaults to 16.
	MetricBuckets int
}

// ClassRule defines one priority class for ShaperConfig.
type ClassRule struct {
	// Name labels the class in metrics; it must be unique and non-empty.
	Name string
	// Label selects the class: a series matches when its label Label equals Value. The
	// special label "__name__" matches the metric name. An empty Label marks the
	// catch-all default class (Value is ignored).
	Label string
	Value string
	// Ceiling is the fraction (0,1] of the queue capacity this class may occupy before
	// it is shed. Higher-priority classes take a higher ceiling so the top band is
	// reserved for them.
	Ceiling float64
}

const (
	defaultShards        = 4096
	defaultMetricBuckets = 16
)

// NewShaper compiles cfg against a queue of the given capacity. Capacity fixes the
// absolute per-class ceilings and the contention threshold. Invalid values are
// clamped to keep the shaper usable: a missing default class is synthesised at full
// capacity, ceilings are clamped to (0,1], and shard/bucket counts fall back to
// defaults.
func NewShaper(capacity int, cfg ShaperConfig) *Shaper {
	return newShaperClock(capacity, cfg, time.Now)
}

// newShaperClock is NewShaper with an injectable clock so token-bucket refill can be
// driven deterministically in tests.
func newShaperClock(capacity int, cfg ShaperConfig, now func() time.Time) *Shaper {
	if capacity < 1 {
		capacity = 1
	}
	shards := cfg.Shards
	if shards < 1 {
		shards = defaultShards
	}
	metricBuckets := cfg.MetricBuckets
	if metricBuckets < 1 {
		metricBuckets = defaultMetricBuckets
	}

	contention := int(cfg.ContentionFraction * float64(capacity))
	if cfg.ContentionFraction <= 0 || contention < 1 {
		contention = 1 // any non-positive fraction means "meter as soon as anything is resident"
	}
	if contention > capacity {
		contention = capacity
	}

	burst := cfg.FairShareBurst
	if burst <= 0 {
		burst = cfg.FairShareRate
	}

	s := &Shaper{
		capacity:      capacity,
		contention:    contention,
		fairShare:     cfg.FairShareRate > 0,
		rate:          cfg.FairShareRate,
		burst:         burst,
		metricBuckets: uint64(metricBuckets),
		shards:        make([]tokenBucket, shards),
		bucketDrops:   make([]atomic.Int64, metricBuckets),
		now:           now,
	}

	s.defaultIdx = -1
	for _, c := range cfg.Classes {
		ceiling := c.Ceiling
		if ceiling <= 0 || ceiling > 1 {
			ceiling = 1
		}
		isDefault := c.Label == ""
		rule := classRule{
			name:        c.Name,
			label:       c.Label,
			value:       c.Value,
			ceilingCost: ceilingCost(ceiling, capacity),
			isDefault:   isDefault,
		}
		if isDefault && s.defaultIdx >= 0 {
			continue // ignore a second catch-all; the first wins
		}
		s.classes = append(s.classes, rule)
		if isDefault {
			s.defaultIdx = len(s.classes) - 1
		}
	}
	if s.defaultIdx < 0 {
		s.classes = append(s.classes, classRule{
			name:        "default",
			ceilingCost: capacity,
			isDefault:   true,
		})
		s.defaultIdx = len(s.classes) - 1
	}
	s.perClass = make([]classCounter, len(s.classes))
	return s
}

// ceilingCost converts a capacity fraction to an absolute resident-cost ceiling of at
// least 1 (so a class is never wedged shut by rounding).
func ceilingCost(fraction float64, capacity int) int {
	c := int(fraction * float64(capacity))
	if c < 1 {
		c = 1
	}
	if c > capacity {
		c = capacity
	}
	return c
}

// DropReason explains why the shaper shed a series.
type DropReason int

const (
	// DropNone marks an admitted series.
	DropNone DropReason = iota
	// DropPriority means the series' class had reached its capacity ceiling: a
	// lower-priority class shed to keep the top band for higher ones.
	DropPriority
	// DropFairShare means the series was over its per-series token-bucket budget under
	// contention: a hot series shed so well-behaved ones keep flowing.
	DropFairShare
)

// Decision is the outcome of Admit.
type Decision struct {
	Admit  bool
	Class  int // index of the matched class (for the caller's reference)
	Reason DropReason
}

// Admit decides whether one series' offering of cost samples may enter the queue,
// given the queue's current depth, and records the outcome (admitted/dropped, by class
// and series bucket). It is the admission *action*, not a pure query: it consumes
// fair-share tokens and bumps counters, so call it exactly once per series offering.
// A shed return leaves the cost for the caller to fold into the queue's grand-total
// drop counter (Queue.RecordShed); an admitted return means the caller should offer
// the series to the queue as usual.
func (s *Shaper) Admit(name string, labels map[string]string, cost, depth int) Decision {
	if cost < 1 {
		cost = 1
	}
	class := s.classify(name, labels)

	// Priority band: a class may not push resident cost past its ceiling. Reserving the
	// top (1-ceiling) for higher classes sheds low priority first.
	if depth+cost > s.classes[class].ceilingCost {
		s.recordDrop(class, name, labels, cost, DropPriority)
		return Decision{Class: class, Reason: DropPriority}
	}

	// Fair share: only under contention, and only when enabled. A series over its
	// token budget is shed so a flood cannot starve well-behaved series.
	if s.fairShare && depth >= s.contention {
		shard := &s.shards[s.hash(name, labels)%uint64(len(s.shards))]
		if !shard.take(float64(cost), s.rate, s.burst, s.now()) {
			s.recordDrop(class, name, labels, cost, DropFairShare)
			return Decision{Class: class, Reason: DropFairShare}
		}
	}

	s.perClass[class].admitted.Add(int64(cost))
	s.totalAdmitted.Add(int64(cost))
	return Decision{Admit: true, Class: class}
}

// classify returns the index of the highest-priority class matching the series, or the
// catch-all default. Classes are few, so the linear scan is cheap.
func (s *Shaper) classify(name string, labels map[string]string) int {
	for i := range s.classes {
		if s.classes[i].isDefault {
			continue
		}
		if s.classes[i].match(name, labels) {
			return i
		}
	}
	return s.defaultIdx
}

func (s *Shaper) recordDrop(class int, name string, labels map[string]string, cost int, reason DropReason) {
	switch reason {
	case DropPriority:
		s.perClass[class].droppedPriority.Add(int64(cost))
	case DropFairShare:
		s.perClass[class].droppedFair.Add(int64(cost))
	}
	s.bucketDrops[s.hash(name, labels)%s.metricBuckets].Add(int64(cost))
	s.totalDropped.Add(int64(cost))
}

// hash is an order-independent 64-bit FNV-1a over the series identity (name plus the
// unordered label set), used to pick a fair-share shard and a metric bucket. Labels
// are folded with XOR so map iteration order does not change the result, and no
// intermediate key string is allocated.
func (s *Shaper) hash(name string, labels map[string]string) uint64 {
	h := fnv1a(fnvOffset, name)
	var acc uint64
	for k, v := range labels {
		acc ^= fnv1a(fnvOffset, k, "=", v)
	}
	return h ^ acc
}

const (
	fnvOffset uint64 = 14695981039346656037
	fnvPrime  uint64 = 1099511628211
)

// fnv1a folds each argument string into the running FNV-1a hash without allocating.
func fnv1a(h uint64, parts ...string) uint64 {
	for _, p := range parts {
		for i := 0; i < len(p); i++ {
			h ^= uint64(p[i])
			h *= fnvPrime
		}
	}
	return h
}

// Shards reports the fixed number of fair-share token buckets. It never changes after
// construction, so it doubles as the bounded-memory witness: per-series state does not
// grow with cardinality.
func (s *Shaper) Shards() int { return len(s.shards) }

// ShaperStats is a consistent-enough snapshot of the shaper's cumulative counters for
// /metrics and /api/v1/stats. The counters are read independently, so a snapshot may
// straddle a concurrent Admit by a sample or two — acceptable for monotonic counters a
// scraper rate()s.
type ShaperStats struct {
	TotalAdmitted int64
	TotalDropped  int64
	Classes       []ClassStat
	BucketDrops   []int64 // per series-hash bucket; index is the bucket id
}

// ClassStat is one class's slice of ShaperStats.
type ClassStat struct {
	Name             string
	Admitted         int64
	DroppedPriority  int64
	DroppedFairShare int64
}

// Stats snapshots the shaper counters. A nil *Shaper (disabled) reports the zero value.
func (s *Shaper) Stats() ShaperStats {
	if s == nil {
		return ShaperStats{}
	}
	st := ShaperStats{
		TotalAdmitted: s.totalAdmitted.Load(),
		TotalDropped:  s.totalDropped.Load(),
		Classes:       make([]ClassStat, len(s.classes)),
		BucketDrops:   make([]int64, len(s.bucketDrops)),
	}
	for i := range s.classes {
		st.Classes[i] = ClassStat{
			Name:             s.classes[i].name,
			Admitted:         s.perClass[i].admitted.Load(),
			DroppedPriority:  s.perClass[i].droppedPriority.Load(),
			DroppedFairShare: s.perClass[i].droppedFair.Load(),
		}
	}
	for i := range s.bucketDrops {
		st.BucketDrops[i] = s.bucketDrops[i].Load()
	}
	return st
}
