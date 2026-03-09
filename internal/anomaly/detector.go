// Package anomaly implements a streaming, per-series anomaly detector for the
// live telemetry path. It is single-pass and O(1) in memory per series (Holt-Winters
// adds a bounded O(season) seasonal array), so it can run inline in the broadcast
// loop without buffering history.
//
// # Two models, one machinery
//
// Per-series scoring is factored behind a small model interface, so the surrounding
// machinery — dedup, debounce/hysteresis, event emission, the dispersion floor, and
// eviction — is shared and a model only has to turn a value into a (baseline, score).
// Two models are selectable by Config.Mode:
//
//   - EWMA (default, ModeEWMA): an exponentially-weighted moving baseline +
//     dispersion, described below. It tracks any slow movement (diurnal or drift) but
//     does not learn the daily shape.
//   - Holt-Winters (ModeHoltWinters, ADR-028): an additive level+trend+seasonal model
//     in holtwinters.go that learns the diurnal shape and scores each value against
//     the band for its own time of day — so it flags a value that is normal globally
//     but abnormal for that phase, which EWMA cannot. EWMA stays the default/fallback.
//
// The rest of this doc describes the EWMA model; see holtwinters.go for Holt-Winters.
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

// Mode selects the per-series scoring model.
type Mode string

const (
	// ModeEWMA is the default model: an EWMA baseline + dispersion (ADR-024). It
	// tracks any slow movement — diurnal or drift — but does not learn the daily
	// shape, so a value that is normal globally but wrong for the time of day only
	// looks anomalous until the level catches up.
	ModeEWMA Mode = "ewma"
	// ModeHoltWinters is the seasonal model (ADR-028): additive level+trend+seasonal
	// Holt-Winters. It learns the diurnal shape and scores each value against the band
	// for its own time of day, so it flags a value that is normal globally but
	// abnormal for that phase — which EWMA cannot.
	ModeHoltWinters Mode = "holt_winters"
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

	// Mode selects the scoring model: ModeEWMA (default) or ModeHoltWinters. An empty
	// or unrecognised value defaults to EWMA, so existing configs are unaffected.
	Mode Mode

	// Holt-Winters tunables (used only when Mode == ModeHoltWinters; ignored by EWMA).
	//
	// SeasonLength is the number of seasonal buckets the season is divided into; a
	// sample is scored against the band learned for its bucket. Must be >= 2.
	// SeasonPeriodMs is the wall-clock span of one full season in milliseconds (e.g.
	// 24h for a diurnal cycle); a sample's bucket is (TimestampMs mod SeasonPeriodMs)
	// mapped into [0, SeasonLength), so the phase comes from the timestamp, not a
	// sample counter. The model warms up over one full SeasonPeriodMs (so every bucket
	// is seeded) rather than over Warmup samples.
	SeasonLength   int
	SeasonPeriodMs int64
	// Beta is the trend smoothing factor and Gamma the seasonal smoothing factor, both
	// in (0,1]. Alpha (shared with EWMA) smooths the level. These take internal
	// defaults; the dispersion band reuses the EWMA Alpha and the shared scale floor.
	Beta  float64
	Gamma float64
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

	// Holt-Winters defaults (Mode == ModeHoltWinters).
	defaultMode = ModeEWMA
	// defaultSeasonLength divides the season into 48 buckets (30-minute resolution for
	// a 24h period) — fine enough to follow a diurnal sinusoid without over-fitting.
	defaultSeasonLength = 48
	// defaultSeasonPeriodMs is one day: the simulator and real infrastructure cycle on
	// a 24-hour diurnal period (ADR-013).
	defaultSeasonPeriodMs = 24 * 60 * 60 * 1000
	// defaultBeta keeps the trend slow so it absorbs genuine drift, not noise;
	// defaultGamma adapts the seasonal shape gradually so a single odd day does not
	// rewrite the learned season.
	defaultBeta  = 0.01
	defaultGamma = 0.05
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

		Mode:           defaultMode,
		SeasonLength:   defaultSeasonLength,
		SeasonPeriodMs: defaultSeasonPeriodMs,
		Beta:           defaultBeta,
		Gamma:          defaultGamma,
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
	if c.Mode != ModeEWMA && c.Mode != ModeHoltWinters {
		c.Mode = defaultMode
	}
	if c.SeasonLength < 2 {
		c.SeasonLength = defaultSeasonLength
	}
	if c.SeasonPeriodMs <= 0 {
		c.SeasonPeriodMs = defaultSeasonPeriodMs
	}
	if c.Beta <= 0 || c.Beta > 1 {
		c.Beta = defaultBeta
	}
	if c.Gamma <= 0 || c.Gamma > 1 {
		c.Gamma = defaultGamma
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

// seriesState is the per-series state. breaches/firing drive the debounce and
// hysteresis and lastTS feeds dedup and eviction, regardless of model. The EWMA
// fields (count/mean/m2/level/variance) carry that model's state: during warmup only
// count/mean/m2 are used (an exact Welford accumulation); afterwards level/variance
// carry the EWMA baseline and dispersion. The Holt-Winters model instead keeps its
// state behind hw (nil under EWMA), so per-series memory stays O(1) for EWMA and
// O(season) for Holt-Winters.
type seriesState struct {
	count    int
	mean     float64 // Welford running mean (EWMA warmup)
	m2       float64 // Welford running sum of squared deviations (EWMA warmup)
	level    float64 // EWMA level (post-warmup)
	variance float64 // EWMA variance of the residual (post-warmup)
	breaches int     // consecutive out-of-band samples
	firing   bool
	lastTS   int64
	hw       *hwState // Holt-Winters state; nil unless Mode == ModeHoltWinters
}

// model is the per-series scoring strategy behind the detector. It owns whatever
// per-series state its model needs (kept in seriesState) and turns each new value
// into the baseline it was judged against and a score, plus whether the series is
// still warming up. The Detector supplies everything around it — dedup, debounce,
// hysteresis, event emission, eviction — so a model only has to score. EWMA is the
// default (ewmaModel, this file); a seasonal Holt-Winters model is the alternative
// (hwModel, holtwinters.go), selected by Config.Mode. A model carries no per-series
// state of its own, so one instance serves every series.
type model interface {
	// observe folds value (sampled at tsMs) into st's per-series model state and
	// returns the baseline the value was scored against, its score
	// |value-baseline|/dispersion, and whether the model is still warming up (during
	// which no alert may be raised). It is called with the detector lock held, after
	// st.count has been advanced to include this sample.
	observe(st *seriesState, value float64, tsMs int64) (baseline, score float64, warming bool)
}

// ewmaModel is the default model (ADR-024): a per-series exponentially-weighted
// moving level + dispersion, robust to a moving baseline. Its math is identical to
// the original inline implementation — see the package doc for the rationale.
type ewmaModel struct{ cfg Config }

func newEWMAModel(cfg Config) *ewmaModel { return &ewmaModel{cfg: cfg} }

func (m *ewmaModel) observe(st *seriesState, value float64, _ int64) (float64, float64, bool) {
	// Warmup: accumulate an exact Welford mean/variance and raise nothing. On the
	// sample that completes warmup, seed the EWMA level/variance from it.
	if st.count <= m.cfg.Warmup {
		delta := value - st.mean
		st.mean += delta / float64(st.count)
		st.m2 += delta * (value - st.mean)
		if st.count == m.cfg.Warmup {
			st.level = st.mean
			st.variance = st.m2 / float64(st.count-1) // sample variance
		}
		return 0, 0, true
	}

	// Post-warmup: score against the current baseline, then fold the (clamped)
	// residual into the baseline. The baseline the point is judged against is the
	// pre-update level, so capture it for the emitted event.
	baseline := st.level
	scale := scaleFloor(m.cfg, st.variance, st.level)
	resid := value - st.level
	score := math.Abs(resid) / scale

	// Huber winsorization: bound the residual's pull on the baseline/dispersion.
	clip := m.cfg.ClipFactor * scale
	cresid := resid
	if cresid > clip {
		cresid = clip
	} else if cresid < -clip {
		cresid = -clip
	}
	incr := m.cfg.Alpha * cresid
	st.level += incr
	// EWMA variance (West's recurrence): variance = (1-α)(variance + α·cresid²).
	st.variance = (1 - m.cfg.Alpha) * (st.variance + cresid*incr)
	return baseline, score, false
}

// Detector is a concurrency-safe streaming anomaly detector over many series. It
// also keeps a bounded ring of recent events for late-joining consumers and the
// two counters (cumulative raised, currently active) used for metrics.
type Detector struct {
	cfg   Config
	model model // per-series scoring strategy (EWMA or Holt-Winters), chosen by cfg.Mode

	mu     sync.Mutex
	series map[string]*seriesState

	seq          uint64 // last assigned event Seq
	raisedTotal  uint64 // cumulative firing events (the anomalies_total counter)
	activeFiring int    // currently-firing series (the active-anomalies gauge)

	recent []Event // bounded ring of recent events, oldest-first
	rstart int     // index of the oldest element when the ring is full
	rfull  bool
}

// New constructs a Detector from cfg, filling unset fields with defaults and
// selecting the per-series scoring model from cfg.Mode (EWMA by default).
func New(cfg Config) *Detector {
	cfg = cfg.withDefaults()
	return &Detector{
		cfg:    cfg,
		model:  newModel(cfg),
		series: make(map[string]*seriesState),
		recent: make([]Event, 0, cfg.BufferSize),
	}
}

// newModel builds the scoring model named by cfg.Mode. EWMA is the default and the
// fallback for any unrecognised mode (withDefaults already normalises Mode, so this
// is belt-and-braces).
func newModel(cfg Config) model {
	switch cfg.Mode {
	case ModeHoltWinters:
		return newHWModel(cfg)
	default:
		return newEWMAModel(cfg)
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

	// Score this sample against its series' model. The model folds the value into the
	// per-series state and returns the baseline it was judged against, the score, and
	// whether the series is still warming up (no alert may be raised during warmup).
	baseline, score, warming := d.model.observe(st, s.Value, s.TimestampMs)
	if warming {
		return Event{}, false
	}

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
// scale toward zero. Shared by every model (EWMA and Holt-Winters) so the dynamic
// band and the flat-series floor behave identically regardless of mode.
func scaleFloor(cfg Config, variance, level float64) float64 {
	scale := math.Sqrt(math.Max(variance, 0))
	floor := cfg.FloorFrac*math.Abs(level) + cfg.FloorAbs
	if scale < floor {
		scale = floor
	}
	return scale
}

// scaleFloor is the detector-bound form of the package scaleFloor, scoring against
// the detector's own config.
func (d *Detector) scaleFloor(variance, level float64) float64 {
	return scaleFloor(d.cfg, variance, level)
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

// Mode returns the active scoring model (after defaulting), so the server can
// surface which model is running on /api/v1/stats, /metrics, and the dashboard. It
// is fixed at construction, so no lock is needed.
func (d *Detector) Mode() Mode { return d.cfg.Mode }

// SortEventsRecentFirst orders events most-recent (highest Seq) first. Helper for
// callers exposing the recent buffer to a UI.
func SortEventsRecentFirst(events []Event) {
	sort.Slice(events, func(i, j int) bool { return events[i].Seq > events[j].Seq })
}
