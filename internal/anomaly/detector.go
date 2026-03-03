// Package anomaly implements a streaming, per-series anomaly detector for the
// live telemetry path. It is single-pass and O(1) in memory per series, so it can
// run inline in the broadcast loop without buffering history.
//
// # Why not a naive global mean/z-score
//
// The telemetry this watches has a slow daily swing (a diurnal cycle) and slow
// upward drift (e.g. a leaking memory gauge). A detector that scored points
// against a global mean and standard deviation would flag the entire afternoon
// peak of a diurnal series as "high" and the late hours of a drifting series as
// "high" — the baseline it compares against is wrong because the baseline itself
// moves. The signal we actually want is a *spike*: a fast departure from the
// value the series was just holding.
//
// # The model
//
// Each series tracks an exponentially-weighted moving baseline (the level) and an
// exponentially-weighted moving dispersion (the variance of the residual). Because
// both decay old data geometrically, the baseline tracks the slow diurnal/drift
// movement and the dispersion tracks the series' own normal variability. The score
//
//	score = |value - level| / dispersion
//
// is therefore a *local* z-score: how far the point is from the recently-tracked
// baseline, in units of the series' own recent noise. A slow ramp keeps the
// residual small (the level follows it), so its score stays near zero; a spike
// produces a residual many times the dispersion, so its score is large.
//
// # Robustness (Huber winsorization)
//
// The classic failure of an EWMA z-score is that the spike, once it arrives, is
// folded into the level and the dispersion: the level lurches toward the spike (so
// the next point looks normal) and the dispersion inflates (so nothing looks
// anomalous for a while afterwards) — the detector blinds itself exactly when it
// should be most sensitive. We bound that influence: before a residual updates the
// level and the variance it is clamped to ±(ClipFactor · dispersion) (a Huber/
// winsorization step). A spike therefore moves the baseline only slightly, so its
// score stays high for the whole spike and the alert holds until the value
// genuinely recovers; meanwhile a slow ramp's residual is well inside the clamp and
// passes through unchanged, so the baseline still tracks it. The scale is also
// floored (see scaleFloor) so a series that goes briefly flat — common when a 1 Hz
// broadcast resamples a slower stream — cannot collapse the dispersion toward zero
// and make ordinary noise look anomalous.
//
// # Warmup, debounce, eviction
//
//   - Warmup: the first Warmup samples seed the level/variance with an exact
//     Welford mean and sample variance and raise no alerts, so the detector never
//     fires before a baseline exists.
//   - Debounce + hysteresis: an alert is raised only after DebounceK consecutive
//     out-of-band samples, and cleared only once the score falls back through a
//     lower hysteresis band — so transient single-sample noise and boundary
//     flapping do not produce alert storms.
//   - Eviction: Evict drops series not seen since a cutoff, so memory stays bounded
//     by live cardinality rather than by all series ever observed.
package anomaly

import (
	"math"
	"sort"
	"sync"
)

// Config tunes the detector. The zero value is not usable directly; pass a Config
// through DefaultConfig (or call (Config).withDefaults, which New applies) so unset
// fields take sane defaults.
type Config struct {
	// Enabled gates the detector. A disabled detector ingests nothing and never
	// alerts; Observe is a no-op.
	Enabled bool
	// Threshold is the score above which a sample is out-of-band. In units of the
	// tracked dispersion (a local standard deviation), so ~3–4 is the usual range.
	Threshold float64
	// Alpha is the EWMA smoothing factor in (0,1] for both the level and the
	// dispersion. Smaller tracks more slowly (steadier baseline); larger adapts
	// faster.
	Alpha float64
	// Warmup is the number of samples used to seed a per-series baseline (via an
	// exact Welford mean/variance) before any alert may be raised. Must be >= 2.
	Warmup int
	// DebounceK is the number of consecutive out-of-band samples required to raise
	// an alert (hysteresis against single-sample noise). Must be >= 1.
	DebounceK int

	// ClipFactor bounds a residual's influence on the level and dispersion to
	// ±(ClipFactor·dispersion) (the Huber winsorization). Defaults to Threshold so
	// anything that would alert cannot also corrupt the baseline it is measured
	// against.
	ClipFactor float64
	// ClearFactor sets the lower hysteresis band: a firing series clears only once
	// its score falls below ClearFactor·Threshold, which damps boundary flapping.
	// In (0,1].
	ClearFactor float64
	// CritFactor is the multiple of Threshold at or above which a raised alert is
	// "crit" rather than "warn".
	CritFactor float64
	// FloorFrac floors the dispersion at FloorFrac·|level| so a momentarily flat
	// series cannot drive the scale toward zero (scale-relative, so it adapts to
	// each series' magnitude). FloorAbs is an absolute floor for levels near zero.
	FloorFrac float64
	FloorAbs  float64
	// BufferSize is the capacity of the bounded recent-events ring used for
	// late-joining clients. <= 0 selects a default.
	BufferSize int
}

// Default tuning constants. These back DefaultConfig and (Config).withDefaults.
const (
	defaultThreshold = 3.5
	defaultAlpha     = 0.1
	defaultWarmup    = 20
	// defaultDebounceK requires two consecutive out-of-band samples to raise. Two
	// independent samples both exceeding the threshold by chance is vanishingly
	// likely (~1e-7), so this still rejects single-sample noise, while a transient
	// spike — which, once a slow stream is deduplicated to its genuine samples, may
	// only span a few points before it decays — is still caught. Three was too
	// strict for short spikes.
	defaultDebounceK   = 2
	defaultClearFactor = 0.5
	defaultCritFactor  = 2.0
	// defaultFloorFrac floors the dispersion at 4% of |level|. Beyond preventing a
	// flat series from collapsing the scale toward zero, this encodes a sane domain
	// default for noisy infrastructure gauges: a sub-~4% departure from the local
	// baseline is treated as in-band (so a memory GC drop or counter step is not an
	// anomaly), while a spike — many times the baseline — is far past it.
	defaultFloorFrac  = 0.04
	defaultFloorAbs   = 1e-9
	defaultBufferSize = 128
)

// DefaultConfig returns the recommended tuning. Enabled is false: callers opt in.
func DefaultConfig() Config {
	return Config{
		Enabled:     false,
		Threshold:   defaultThreshold,
		Alpha:       defaultAlpha,
		Warmup:      defaultWarmup,
		DebounceK:   defaultDebounceK,
		ClipFactor:  defaultThreshold,
		ClearFactor: defaultClearFactor,
		CritFactor:  defaultCritFactor,
		FloorFrac:   defaultFloorFrac,
		FloorAbs:    defaultFloorAbs,
		BufferSize:  defaultBufferSize,
	}
}

// withDefaults returns c with any unset (zero) field replaced by its default, so a
// partially-specified Config (e.g. just Threshold/Alpha/Warmup/DebounceK from YAML)
// is still internally complete.
func (c Config) withDefaults() Config {
	if c.Threshold <= 0 {
		c.Threshold = defaultThreshold
	}
	if c.Alpha <= 0 || c.Alpha > 1 {
		c.Alpha = defaultAlpha
	}
	if c.Warmup < 2 {
		c.Warmup = defaultWarmup
	}
	if c.DebounceK < 1 {
		c.DebounceK = defaultDebounceK
	}
	if c.ClipFactor <= 0 {
		c.ClipFactor = c.Threshold
	}
	if c.ClearFactor <= 0 || c.ClearFactor > 1 {
		c.ClearFactor = defaultClearFactor
	}
	if c.CritFactor <= 0 {
		c.CritFactor = defaultCritFactor
	}
	if c.FloorFrac < 0 {
		c.FloorFrac = defaultFloorFrac
	}
	if c.FloorAbs < 0 {
		c.FloorAbs = defaultFloorAbs
	}
	if c.BufferSize <= 0 {
		c.BufferSize = defaultBufferSize
	}
	return c
}

// Sample is one observation fed to the detector: the latest value of a series.
type Sample struct {
	// Series is the stable key for the series (e.g. `cpu_usage_percent{host="web-01"}`).
	Series string
	// Metric is the bare metric name.
	Metric string
	// Labels is the series' label set (may be nil).
	Labels map[string]string
	// Value is the observed value.
	Value float64
	// TimestampMs is the sample's own timestamp in Unix milliseconds. The detector
	// dedups on it: a sample whose timestamp does not advance the series is treated
	// as a repeat (e.g. a 1 Hz broadcast re-reading a slower series) and ignored, so
	// debounce counts genuine samples rather than re-reads.
	TimestampMs int64
}

// State is whether an event raises or clears an alert.
type State string

const (
	// StateFiring is emitted when a series first goes out-of-band past the debounce.
	StateFiring State = "firing"
	// StateResolved is emitted when a firing series returns in-band.
	StateResolved State = "resolved"
)

// Severity ranks a firing alert by how far out-of-band it is.
const (
	SeverityWarn = "warn"
	SeverityCrit = "crit"
)

// Event is an alert transition emitted by the detector. Firing and resolved events
// both carry the score/baseline at the transition so a consumer can render them
// without recomputation.
type Event struct {
	// Seq is a monotonic sequence number across all events from this detector. It
	// gives consumers a stable identity for de-duplication when they seed from the
	// recent buffer and then also receive the live event.
	Seq uint64 `json:"seq"`
	// Series is the series key; Metric/Labels identify it for display.
	Series string            `json:"series"`
	Metric string            `json:"metric"`
	Labels map[string]string `json:"labels,omitempty"`
	// Value is the out-of-band value (firing) or the recovery value (resolved).
	Value float64 `json:"value"`
	// Baseline is the tracked EWMA level at the transition.
	Baseline float64 `json:"baseline"`
	// Score is |Value-Baseline| / dispersion at the transition.
	Score float64 `json:"score"`
	// Severity is "warn" or "crit" (by Score vs CritFactor·Threshold).
	Severity string `json:"severity"`
	// State is "firing" or "resolved".
	State State `json:"state"`
	// TimestampMs is the sample timestamp that produced the transition.
	TimestampMs int64 `json:"timestamp"`
}

// seriesState is the O(1) per-series state. During warmup only count/mean/m2 are
// used (an exact Welford accumulation); afterwards level/variance carry the EWMA
// baseline and dispersion. breaches/firing drive the debounce and hysteresis, and
// lastTS feeds dedup and eviction.
type seriesState struct {
	count    int
	mean     float64 // Welford running mean (warmup)
	m2       float64 // Welford running sum of squared deviations (warmup)
	level    float64 // EWMA level (post-warmup)
	variance float64 // EWMA variance of the residual (post-warmup)
	breaches int     // consecutive out-of-band samples
	firing   bool
	lastTS   int64
}

// Detector is a concurrency-safe streaming anomaly detector over many series. It
// also keeps a bounded ring of recent events for late-joining consumers and the
// two counters (cumulative raised, currently active) used for metrics.
type Detector struct {
	cfg Config

	mu     sync.Mutex
	series map[string]*seriesState

	seq          uint64 // last assigned event Seq
	raisedTotal  uint64 // cumulative firing events (the anomalies_total counter)
	activeFiring int    // currently-firing series (the active-anomalies gauge)

	recent []Event // bounded ring of recent events, oldest-first
	rstart int     // index of the oldest element when the ring is full
	rfull  bool
}

// New constructs a Detector from cfg, filling unset fields with defaults.
func New(cfg Config) *Detector {
	cfg = cfg.withDefaults()
	return &Detector{
		cfg:    cfg,
		series: make(map[string]*seriesState),
		recent: make([]Event, 0, cfg.BufferSize),
	}
}

// Observe folds one sample into its series and returns an Event when the sample
// raises or clears an alert (ok=false otherwise). It is single-pass and updates
// only the one series' O(1) state.
func (d *Detector) Observe(s Sample) (Event, bool) {
	if !d.cfg.Enabled {
		return Event{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	st, ok := d.series[s.Series]
	if !ok {
		st = &seriesState{}
		d.series[s.Series] = st
	} else if s.TimestampMs <= st.lastTS {
		// Not a new sample for this series (a re-read of the same point). Ignore it
		// so debounce and warmup count genuine samples, but keep lastTS fresh enough
		// that the series is not evicted while it is still being observed.
		return Event{}, false
	}
	st.lastTS = s.TimestampMs
	st.count++

	// Warmup: accumulate an exact Welford mean/variance and raise nothing. On the
	// sample that completes warmup, seed the EWMA level/variance from it.
	if st.count <= d.cfg.Warmup {
		delta := s.Value - st.mean
		st.mean += delta / float64(st.count)
		st.m2 += delta * (s.Value - st.mean)
		if st.count == d.cfg.Warmup {
			st.level = st.mean
			st.variance = st.m2 / float64(st.count-1) // sample variance
		}
		return Event{}, false
	}

	// Post-warmup: score against the current baseline, then fold the (clamped)
	// residual into the baseline. The baseline the point is judged against is the
	// pre-update level, so capture it for the emitted event.
	baseline := st.level
	scale := d.scaleFloor(st.variance, st.level)
	resid := s.Value - st.level
	score := math.Abs(resid) / scale

	// Huber winsorization: bound the residual's pull on the baseline/dispersion.
	clip := d.cfg.ClipFactor * scale
	cresid := resid
	if cresid > clip {
		cresid = clip
	} else if cresid < -clip {
		cresid = -clip
	}
	incr := d.cfg.Alpha * cresid
	st.level += incr
	// EWMA variance (West's recurrence): variance = (1-α)(variance + α·cresid²).
	st.variance = (1 - d.cfg.Alpha) * (st.variance + cresid*incr)

	// Debounce + hysteresis. Raise after DebounceK consecutive out-of-band samples;
	// clear once the score falls back below the lower band.
	outOfBand := score > d.cfg.Threshold
	if outOfBand {
		st.breaches++
	} else {
		st.breaches = 0
	}

	if !st.firing {
		if st.breaches >= d.cfg.DebounceK {
			st.firing = true
			d.activeFiring++
			d.raisedTotal++
			return d.emit(s, baseline, score, StateFiring), true
		}
		return Event{}, false
	}
	// Firing: clear only once the score drops below the hysteresis band.
	if score < d.cfg.Threshold*d.cfg.ClearFactor {
		st.firing = false
		d.activeFiring--
		return d.emit(s, baseline, score, StateResolved), true
	}
	return Event{}, false
}

// ObserveBatch folds a whole tick's worth of samples and returns every event
// raised or cleared, in input order. Convenience for a broadcast loop.
func (d *Detector) ObserveBatch(samples []Sample) []Event {
	var events []Event
	for _, s := range samples {
		if ev, ok := d.Observe(s); ok {
			events = append(events, ev)
		}
	}
	return events
}

// scaleFloor returns the dispersion used for scoring: sqrt(variance), floored at
// FloorFrac·|level| + FloorAbs so a momentarily-flat series cannot collapse the
// scale toward zero.
func (d *Detector) scaleFloor(variance, level float64) float64 {
	scale := math.Sqrt(math.Max(variance, 0))
	floor := d.cfg.FloorFrac*math.Abs(level) + d.cfg.FloorAbs
	if scale < floor {
		scale = floor
	}
	return scale
}

// emit builds an Event, assigns it a Seq, and records it in the recent ring. Caller
// holds d.mu.
func (d *Detector) emit(s Sample, baseline, score float64, state State) Event {
	d.seq++
	ev := Event{
		Seq:         d.seq,
		Series:      s.Series,
		Metric:      s.Metric,
		Labels:      s.Labels,
		Value:       s.Value,
		Baseline:    baseline,
		Score:       score,
		Severity:    d.severity(score),
		State:       state,
		TimestampMs: s.TimestampMs,
	}
	d.record(ev)
	return ev
}

func (d *Detector) severity(score float64) string {
	if score >= d.cfg.CritFactor*d.cfg.Threshold {
		return SeverityCrit
	}
	return SeverityWarn
}

// record appends ev to the bounded ring, evicting the oldest when full. Caller
// holds d.mu.
func (d *Detector) record(ev Event) {
	if len(d.recent) < d.cfg.BufferSize {
		d.recent = append(d.recent, ev)
		return
	}
	d.recent[d.rstart] = ev
	d.rstart = (d.rstart + 1) % d.cfg.BufferSize
	d.rfull = true
}

// Recent returns a snapshot of the buffered recent events, oldest-first.
func (d *Detector) Recent() []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Event, 0, len(d.recent))
	if d.rfull {
		out = append(out, d.recent[d.rstart:]...)
		out = append(out, d.recent[:d.rstart]...)
	} else {
		out = append(out, d.recent...)
	}
	return out
}

// Evict removes series whose last sample is older than cutoffMs (Unix ms), so
// memory stays bounded by live cardinality. A firing series that is evicted
// decrements the active gauge. Returns the number of series removed.
func (d *Detector) Evict(cutoffMs int64) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	removed := 0
	for key, st := range d.series {
		if st.lastTS < cutoffMs {
			if st.firing {
				d.activeFiring--
			}
			delete(d.series, key)
			removed++
		}
	}
	return removed
}

// Total returns the cumulative number of alerts raised since startup (the
// meridian_anomalies_total counter).
func (d *Detector) Total() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.raisedTotal
}

// Active returns the number of series currently firing (the meridian_active_anomalies gauge).
func (d *Detector) Active() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.activeFiring
}

// Len returns the number of series with live state. Used for tests and sizing.
func (d *Detector) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.series)
}

// SortEventsRecentFirst orders events most-recent (highest Seq) first. Helper for
// callers exposing the recent buffer to a UI.
func SortEventsRecentFirst(events []Event) {
	sort.Slice(events, func(i, j int) bool { return events[i].Seq > events[j].Seq })
}
