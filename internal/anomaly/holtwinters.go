package anomaly

import "math"

// Holt-Winters seasonal model (ADR-028).
//
// EWMA (detector.go) compares a value only against the level the series was just
// holding, so it tracks any slow movement — diurnal or drift — but never learns the
// daily *shape*. A value that is normal globally yet wrong for the time of day (a
// load that stays at its midday level through the small hours) looks normal to EWMA
// until the level slowly catches up. Holt-Winters closes that gap: it learns an
// additive level + trend + seasonal decomposition and scores each value against the
// band for its own seasonal phase, so an off-season value is out-of-band immediately.
//
// For one series at sample y_t falling in seasonal bucket k:
//
//	forecast  f_t = L + T + S[k]                 (one-step-ahead)
//	residual  e_t = y_t − f_t
//	score         = |e_t| / dispersion           (scored against a dynamic band)
//	L' = L + T + α·e ;  T' = T + β·(L'−L−T) ;  S'[k] = S[k] + γ·(1−α)·e
//
// where the residual is Huber-clamped before it updates the model (so one spike
// cannot rewrite the learned level/season or inflate the band), the dispersion is an
// EWMA variance of the residual floored by the shared scaleFloor, and α (the level
// factor) is shared with EWMA while β (trend) and γ (seasonal) are Holt-Winters'
// own. Everything else — debounce, hysteresis, severity, emission, eviction — is the
// detector's, unchanged.
//
// Phase comes from the sample timestamp, not a counter: bucket k = (ts mod period)
// mapped into [0, SeasonLength), so a value is always judged against the band learned
// for its own time of day even if samples are deduped or irregular. The model warms
// up over one full season (every bucket seeded) rather than over a sample count.

// hwModel is the seasonal scoring model. It is stateless across series — all
// per-series state lives in seriesState.hw — so one instance serves every series.
type hwModel struct {
	cfg      Config
	m        int   // number of seasonal buckets (cfg.SeasonLength)
	periodMs int64 // wall-clock span of one full season (cfg.SeasonPeriodMs)
}

func newHWModel(cfg Config) *hwModel {
	return &hwModel{cfg: cfg, m: cfg.SeasonLength, periodMs: cfg.SeasonPeriodMs}
}

// bucket maps a sample timestamp to its seasonal index in [0, m). Computed as
// (ts mod period)·m / period so the result is always in range without a separate
// clamp (ts mod period < period ⇒ the product is < period·m).
func (hm *hwModel) bucket(tsMs int64) int {
	p := tsMs % hm.periodMs
	if p < 0 {
		p += hm.periodMs // Unix ms are positive in practice; guard negatives anyway
	}
	return int(p * int64(hm.m) / hm.periodMs)
}

// hwState is the per-series Holt-Winters state. seasonal carries one additive offset
// per bucket; variance is an EWMA of the one-step residual (the dynamic band). During
// warmup, warm gathers the first season's per-bucket statistics and is released once
// the model is seeded, so steady-state memory is O(season).
type hwState struct {
	level    float64
	trend    float64
	seasonal []float64 // length m; additive offset for each seasonal bucket
	variance float64   // EWMA variance of the one-step forecast residual
	firstTS  int64     // timestamp of the first sample (start of the warmup season)
	warm     *hwWarmup // non-nil only during warmup
}

// hwWarmup holds the sufficient statistics gathered over the warmup season: per
// bucket the sum, sum of squares and count — enough to seed the level (overall mean),
// the seasonal offsets (per-bucket mean − level) and the residual variance (pooled
// within-bucket variance) in a single pass, with no history buffer.
type hwWarmup struct {
	bSum   []float64
	bSumSq []float64
	bCount []int
}

func newHWState(m int, firstTS int64) *hwState {
	return &hwState{
		seasonal: make([]float64, m),
		firstTS:  firstTS,
		warm: &hwWarmup{
			bSum:   make([]float64, m),
			bSumSq: make([]float64, m),
			bCount: make([]int, m),
		},
	}
}

func (hm *hwModel) observe(st *seriesState, value float64, tsMs int64) (float64, float64, bool) {
	if st.hw == nil {
		st.hw = newHWState(hm.m, tsMs)
	}
	hs := st.hw
	k := hm.bucket(tsMs)

	// Warmup: gather one full season of per-bucket statistics and raise nothing. Seed
	// the model on the first sample at or past one whole period from the first sample,
	// so every bucket has been observed before any value is scored. The seeding sample
	// itself raises no alert (like the EWMA warmup-completing sample).
	if hs.warm != nil {
		hs.warm.bSum[k] += value
		hs.warm.bSumSq[k] += value * value
		hs.warm.bCount[k]++
		if tsMs-hs.firstTS >= hm.periodMs {
			hm.seed(hs)
		}
		return 0, 0, true
	}

	// Post-warmup: one-step-ahead forecast for this bucket, score the residual against
	// the dynamic band, then fold the (clamped) residual back into level/trend/seasonal
	// and the dispersion. The forecast is reported as the event baseline.
	forecast := hs.level + hs.trend + hs.seasonal[k]
	scale := scaleFloor(hm.cfg, hs.variance, hs.level)
	resid := value - forecast
	score := math.Abs(resid) / scale

	// Huber winsorization (shared with EWMA): bound a spike's pull on the model so one
	// outlier cannot rewrite the learned level/season or inflate the band, keeping the
	// alert up for the whole excursion rather than self-blinding after one sample.
	clip := hm.cfg.ClipFactor * scale
	cresid := resid
	if cresid > clip {
		cresid = clip
	} else if cresid < -clip {
		cresid = -clip
	}

	// Additive Holt-Winters updates in residual form (so the clamp applies cleanly):
	// L' = L + T + α·e ; T' = T + β·(L'−L−T) ; S'[k] = S[k] + γ·(1−α)·e. Order matters:
	// the trend uses the old trend and the new level.
	prevLevel := hs.level
	newLevel := hs.level + hs.trend + hm.cfg.Alpha*cresid
	hs.trend += hm.cfg.Beta * (newLevel - prevLevel - hs.trend)
	hs.level = newLevel
	hs.seasonal[k] += hm.cfg.Gamma * (1 - hm.cfg.Alpha) * cresid

	// Dispersion band: EWMA variance of the residual (West's recurrence) — the same
	// form the EWMA model uses, so the band tracks the genuine noise around the season.
	incr := hm.cfg.Alpha * cresid
	hs.variance = (1 - hm.cfg.Alpha) * (hs.variance + cresid*incr)

	return forecast, score, false
}

// seed initialises level/trend/seasonal/variance from the warmup season's per-bucket
// statistics, then releases the warmup accumulators. level is the overall mean,
// seasonal[k] the bucket mean minus that level (0 for a bucket not seen during
// warmup), variance the pooled within-bucket variance (the residual noise around the
// season, which is the band an off-season value is judged against), and trend 0 — the
// post-warmup recurrence picks up genuine drift. Seeding the seasonal from the warmup
// season is what keeps the first post-warmup season from spuriously alerting.
func (hm *hwModel) seed(hs *hwState) {
	w := hs.warm
	var totalSum float64
	var totalCount int
	for k := 0; k < hm.m; k++ {
		totalSum += w.bSum[k]
		totalCount += w.bCount[k]
	}
	if totalCount > 0 {
		hs.level = totalSum / float64(totalCount)
	}
	var ss float64 // pooled within-bucket sum of squared deviations
	var dof int
	for k := 0; k < hm.m; k++ {
		if w.bCount[k] > 0 {
			mean := w.bSum[k] / float64(w.bCount[k])
			hs.seasonal[k] = mean - hs.level
			ss += w.bSumSq[k] - w.bSum[k]*w.bSum[k]/float64(w.bCount[k])
			dof += w.bCount[k] - 1
		}
	}
	if dof > 0 {
		hs.variance = ss / float64(dof)
	}
	hs.trend = 0
	hs.warm = nil
}
