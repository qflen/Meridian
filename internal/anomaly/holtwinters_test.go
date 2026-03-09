package anomaly

import (
	"math"
	"math/rand"
	"testing"
)

// hwConfig returns an enabled detector config in Holt-Winters mode with a short test
// season: 48 buckets over a 240 s period, so at the 1 Hz spacing feed() uses there
// are 5 samples per bucket and one full season is 240 samples. This keeps the
// seasonal tests fast while exercising the same code paths as a 24 h diurnal season.
func hwConfig() Config {
	c := DefaultConfig()
	c.Enabled = true
	c.Mode = ModeHoltWinters
	c.SeasonLength = 48
	c.SeasonPeriodMs = 240_000
	return c
}

// firingAtOrAfter counts firing transitions whose sample timestamp is at or after
// cutoffMs — used to look only at the off-season window in the EWMA comparison.
func firingAtOrAfter(events []Event, cutoffMs int64) int {
	n := 0
	for _, e := range events {
		if e.State == StateFiring && e.TimestampMs >= cutoffMs {
			n++
		}
	}
	return n
}

// ── Warmup over one full season, bounded state, post-warmup spike ────────────

func TestHoltWintersWarmsOverOneSeasonThenFires(t *testing.T) {
	cfg := hwConfig()
	d := New(cfg)
	rng := rand.New(rand.NewSource(10))
	period := int(cfg.SeasonPeriodMs / 1000) // 240 samples per season at 1 Hz
	diurnal := func(i int) float64 {
		return 50 + 25*math.Sin(2*math.Pi*float64(i)/float64(period)) + 0.3*rng.NormFloat64()
	}

	// Feed just under one full season: the model is still warming up and raises
	// nothing, and no alert may fire before a full season has been seen.
	short := make([]float64, period-5)
	for i := range short {
		short[i] = diurnal(i)
	}
	if ev := feed(d, 0, short); len(ev) != 0 {
		t.Fatalf("no alert may be raised before one full season elapses, got %+v", ev)
	}
	st := d.series["s"]
	if st == nil || st.hw == nil || st.hw.warm == nil {
		t.Fatalf("series should still be warming up before a full season has elapsed")
	}

	// Cross the one-season boundary: the model seeds (warmup accumulators released)
	// and its seasonal state stays bounded to SeasonLength.
	more := make([]float64, 10)
	for i := range more {
		more[i] = diurnal(period - 5 + i)
	}
	feed(d, int64(period-5)*1000, more)
	if st.hw.warm != nil {
		t.Fatalf("model should be seeded once one full season has elapsed (warmup accumulators not released)")
	}
	if len(st.hw.seasonal) != cfg.SeasonLength {
		t.Fatalf("seasonal state must stay bounded to SeasonLength=%d, got %d", cfg.SeasonLength, len(st.hw.seasonal))
	}

	// Post-warmup a clear spike at an arbitrary time fires.
	post := feed(d, int64(period+5)*1000, []float64{400, 400, 400, 400})
	if countState(post, StateFiring) == 0 {
		t.Fatalf("a clear spike must fire after warmup, got %+v", post)
	}
}

// ── Clean diurnal series raises no alerts (it learns the season) ─────────────

func TestHoltWintersCleanDiurnalRaisesNoAlerts(t *testing.T) {
	cfg := hwConfig()
	d := New(cfg)
	rng := rand.New(rand.NewSource(11))
	period := int(cfg.SeasonPeriodMs / 1000)
	n := 5 * period // one warmup season + four scored seasons
	vals := make([]float64, n)
	for i := range vals {
		vals[i] = 50 + 25*math.Sin(2*math.Pi*float64(i)/float64(period)) + 0.3*rng.NormFloat64()
	}
	events := feed(d, 0, vals)
	if len(events) != 0 {
		t.Fatalf("a clean diurnal series must raise no alerts once the season is learned, got %d: %+v", len(events), events)
	}
}

// ── The seasonal payoff: an off-season value that EWMA cannot catch ──────────

func TestHoltWintersFlagsOffSeasonValueThatEWMAMisses(t *testing.T) {
	// The defining case for a seasonal model: a value that is perfectly normal
	// *globally* but wrong *for its time of day*. The series sits at a baseline with a
	// sharp scheduled dip each season (a nightly batch job, say). On the test season
	// the dip does not happen — the value stays at the baseline. That baseline is the
	// most normal value the series has; held where the season expects the dip it never
	// jumps, so EWMA — which only sees departures from the value the series was just
	// holding — tracks it and stays silent. Holt-Winters knows a dip is due at that
	// phase and flags its absence. Both detectors get identical input.
	const period = 240
	periodMs := int64(period) * 1000
	const dipStart, dipEnd = 100, 120 // 20 samples, aligned to whole 5-sample buckets
	rng := rand.New(rand.NewSource(12))
	value := func(i int) float64 {
		base := 50.0
		if phase := i % period; phase >= dipStart && phase < dipEnd {
			base = 20.0 // the scheduled dip
		}
		return base + 0.3*rng.NormFloat64()
	}

	const learnSeasons = 4 // one warmup season + three the dip is learned over
	n := (learnSeasons + 1) * period
	vals := make([]float64, n)
	for i := range vals {
		vals[i] = value(i)
	}
	// On the final season the dip does not happen: the value holds the baseline.
	anomalyStart := learnSeasons*period + dipStart
	for i := anomalyStart; i < learnSeasons*period+dipEnd; i++ {
		vals[i] = 50 + 0.3*rng.NormFloat64()
	}

	cfgHW := hwConfig()
	cfgHW.SeasonPeriodMs = periodMs
	hw := New(cfgHW)
	ewma := New(testConfig()) // default EWMA model

	hwEvents := feed(hw, 0, vals)
	ewmaEvents := feed(ewma, 0, vals)

	windowMs := int64(anomalyStart) * 1000
	if firingAtOrAfter(hwEvents, windowMs) == 0 {
		t.Fatalf("Holt-Winters must flag the missing scheduled dip (a globally-normal value at the wrong time of day), but it never fired")
	}
	if got := firingAtOrAfter(ewmaEvents, windowMs); got != 0 {
		t.Fatalf("EWMA cannot model the season and should not flag a value equal to the one the series was just holding, but it fired %d times in the window", got)
	}
	// Holt-Winters fires only for the missing dip, not for the three dips it learned —
	// the seasonal model both adds the catch EWMA misses and avoids EWMA's own
	// false positives on the legitimate dips.
	if total, win := countState(hwEvents, StateFiring), firingAtOrAfter(hwEvents, windowMs); total != win {
		t.Fatalf("Holt-Winters should fire only for the missing dip, not the learned dips: total firing=%d, in-window=%d", total, win)
	}
}

// ── Phase is derived deterministically from the timestamp ────────────────────

func TestHoltWintersBucketFromTimestamp(t *testing.T) {
	hm := newHWModel(hwConfig()) // 48 buckets over 240 s ⇒ 5 s per bucket
	cases := []struct {
		ts   int64
		want int
	}{
		{0, 0},
		{4_999, 0},
		{5_000, 1},
		{239_999, 47},
		{240_000, 0},         // wraps at the period boundary
		{245_000, 1},         // same phase one period later
		{240_000 + 4_999, 0}, // still in bucket 0 of the next period
	}
	for _, c := range cases {
		if got := hm.bucket(c.ts); got != c.want {
			t.Errorf("bucket(%d) = %d, want %d", c.ts, got, c.want)
		}
	}
}

// ── Mode selection and seasonal defaults ─────────────────────────────────────

func TestModeSelectionAndSeasonalDefaults(t *testing.T) {
	if m := New(testConfig()).Mode(); m != ModeEWMA {
		t.Fatalf("default mode should be EWMA, got %q", m)
	}
	if m := New(hwConfig()).Mode(); m != ModeHoltWinters {
		t.Fatalf("hwConfig should select Holt-Winters, got %q", m)
	}
	// An unset/unknown mode normalises to EWMA, and the seasonal tunables are filled.
	c := Config{Mode: "bogus"}.withDefaults()
	if c.Mode != ModeEWMA {
		t.Errorf("unknown mode should default to EWMA, got %q", c.Mode)
	}
	if c.SeasonLength < 2 || c.SeasonPeriodMs <= 0 || c.Beta <= 0 || c.Gamma <= 0 {
		t.Errorf("seasonal defaults not filled: SeasonLength=%d SeasonPeriodMs=%d Beta=%g Gamma=%g",
			c.SeasonLength, c.SeasonPeriodMs, c.Beta, c.Gamma)
	}
}
