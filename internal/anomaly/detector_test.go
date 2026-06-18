package anomaly

import (
	"math"
	"math/rand"
	"testing"
)

// testConfig returns an enabled detector config with the standard defaults.
func testConfig() Config {
	c := DefaultConfig()
	c.Enabled = true
	return c
}

// feed pushes a sequence of values at 1 s spacing starting at startTS and returns
// every event emitted. Timestamps advance so none are deduped.
func feed(d *Detector, startTS int64, values []float64) []Event {
	var events []Event
	for i, v := range values {
		ts := startTS + int64(i)*1000
		if ev, ok := d.Observe(Sample{Series: "s", Metric: "m", Value: v, TimestampMs: ts}); ok {
			events = append(events, ev)
		}
	}
	return events
}

func countState(events []Event, state State) int {
	n := 0
	for _, e := range events {
		if e.State == state {
			n++
		}
	}
	return n
}

// ── Core requirement: what must and must not alert ──────────────────────────

func TestStationarySeriesRaisesNoAlerts(t *testing.T) {
	d := New(testConfig())
	rng := rand.New(rand.NewSource(1))
	vals := make([]float64, 600)
	for i := range vals {
		vals[i] = 50 + rng.NormFloat64() // base 50, unit-variance noise
	}
	events := feed(d, 0, vals)
	if len(events) != 0 {
		t.Fatalf("stationary series should raise no alerts, got %d events: %+v", len(events), events)
	}
	if d.Active() != 0 || d.Total() != 0 {
		t.Fatalf("expected no active/total anomalies, got active=%d total=%d", d.Active(), d.Total())
	}
}

func TestDiurnalSwingRaisesNoAlerts(t *testing.T) {
	// A slow sinusoidal swing (amplitude 25 on a base of 50) sampled far faster
	// than its period: the EWMA baseline tracks it, so the residual stays small and
	// nothing fires. This is the diurnal-cycle requirement.
	d := New(testConfig())
	rng := rand.New(rand.NewSource(2))
	const period = 480
	vals := make([]float64, 2*period)
	for i := range vals {
		vals[i] = 50 + 25*math.Sin(2*math.Pi*float64(i)/period) + rng.NormFloat64()
	}
	events := feed(d, 0, vals)
	if len(events) != 0 {
		t.Fatalf("diurnal swing should raise no alerts, got %d events: %+v", len(events), events)
	}
}

func TestSlowDriftRaisesNoAlertsButNaiveBaselineWould(t *testing.T) {
	// A slow upward drift (a leaking-gauge model). The EWMA baseline follows it, so
	// the residual stays small and nothing fires — yet the data ends many standard
	// deviations above the *seed* baseline, so a naive frozen-baseline z-score would
	// flag the whole tail. This is exactly why the moving baseline is used.
	cfg := testConfig()
	d := New(cfg)
	const n = 600
	vals := make([]float64, n)
	rng := rand.New(rand.NewSource(3))
	for i := range vals {
		vals[i] = 50 + 0.4*float64(i) + rng.NormFloat64() // drift +0.4 per sample
	}

	// Frozen baseline a naive detector would key on: mean/std of the warmup window.
	mean, std := meanStd(vals[:cfg.Warmup])
	frozenZ := math.Abs(vals[n-1]-mean) / std
	if frozenZ <= cfg.Threshold {
		t.Fatalf("test setup: drift too gentle to prove the point (frozen z=%.2f <= %.1f)", frozenZ, cfg.Threshold)
	}

	events := feed(d, 0, vals)
	if got := countState(events, StateFiring); got != 0 {
		t.Fatalf("EWMA detector must not flag slow drift, but raised %d alerts (frozen-baseline z at end was %.1f)", got, frozenZ)
	}
}

func TestSpikeRaisesThenClearsOnRecovery(t *testing.T) {
	d := New(testConfig())
	rng := rand.New(rand.NewSource(4))

	// Warm up and stabilise on a quiet baseline.
	base := make([]float64, 80)
	for i := range base {
		base[i] = 50 + 0.5*rng.NormFloat64()
	}
	pre := feed(d, 0, base)
	if len(pre) != 0 {
		t.Fatalf("no alerts expected on the quiet baseline, got %+v", pre)
	}

	// Inject a sustained spike (sampled several times, as a slow stream resampled at
	// 1 Hz would be), then recover.
	spike := []float64{82, 81, 83, 80, 82, 81} // ~30 above baseline ≈ huge score
	spikeEvents := feed(d, 80_000, spike)
	if countState(spikeEvents, StateFiring) == 0 {
		t.Fatalf("a spike must raise an alert, got %+v", spikeEvents)
	}
	if d.Active() != 1 {
		t.Fatalf("expected exactly one active anomaly during the spike, got %d", d.Active())
	}
	// The first out-of-band sample must not fire on its own (debounce).
	if spikeEvents[0].State == StateFiring && spikeEvents[0].TimestampMs == 80_000 {
		t.Fatalf("alert raised on the first out-of-band sample, debounce not applied")
	}

	recover := make([]float64, 40)
	for i := range recover {
		recover[i] = 50 + 0.5*rng.NormFloat64()
	}
	recEvents := feed(d, 86_000, recover)
	if countState(recEvents, StateResolved) == 0 {
		t.Fatalf("the alert must clear once the series recovers, got %+v", recEvents)
	}
	if d.Active() != 0 {
		t.Fatalf("expected no active anomalies after recovery, got %d", d.Active())
	}
}

func TestSecondSpikeStillFires_WinsorizationKeepsScaleStable(t *testing.T) {
	// Robustness: a first spike must not inflate the dispersion so much that an
	// identical second spike is missed. The Huber clamp bounds the spike's pull on
	// the variance, so the detector stays sensitive.
	d := New(testConfig())
	rng := rand.New(rand.NewSource(5))
	quiet := func(n int) []float64 {
		out := make([]float64, n)
		for i := range out {
			out[i] = 50 + 0.5*rng.NormFloat64()
		}
		return out
	}
	feed(d, 0, quiet(80))
	first := feed(d, 80_000, []float64{82, 81, 83, 80})
	feed(d, 84_000, quiet(40))
	second := feed(d, 124_000, []float64{82, 81, 83, 80})

	if countState(first, StateFiring) == 0 {
		t.Fatalf("first spike should fire")
	}
	if countState(second, StateFiring) == 0 {
		t.Fatalf("second identical spike should also fire — dispersion was not kept stable")
	}
}

// ── Warmup, debounce, threshold ─────────────────────────────────────────────

func TestNoAlertsDuringWarmup(t *testing.T) {
	cfg := testConfig()
	cfg.Warmup = 10
	d := New(cfg)
	// An out-of-band value lands inside the warmup window: it only seeds the
	// baseline, it must not raise an alert.
	vals := []float64{50, 50, 50, 200, 50, 50, 50, 50, 50, 50}
	events := feed(d, 0, vals)
	if len(events) != 0 {
		t.Fatalf("no alert may be raised during warmup, got %+v", events)
	}
	// After warmup the detector is functional: a genuine spike fires.
	post := feed(d, 10_000, []float64{50, 50, 600, 600, 600, 600})
	if countState(post, StateFiring) == 0 {
		t.Fatalf("detector should fire on a post-warmup spike, got %+v", post)
	}
}

func TestDebounceRequiresKConsecutive(t *testing.T) {
	cfg := testConfig()
	cfg.Warmup = 20
	cfg.DebounceK = 3
	d := New(cfg)

	// Quiet baseline to establish a small dispersion.
	base := make([]float64, 60)
	for i := range base {
		base[i] = 100
	}
	// A few deduped-equal values give zero variance; add tiny structure so the
	// scale floor governs and out-of-band is well-defined.
	rng := rand.New(rand.NewSource(6))
	for i := range base {
		base[i] = 100 + 0.5*rng.NormFloat64()
	}
	if ev := feed(d, 0, base); len(ev) != 0 {
		t.Fatalf("baseline should be quiet, got %+v", ev)
	}

	// Two out-of-band samples then back in-band: fewer than K, so no alert.
	twoThenBack := feed(d, 60_000, []float64{180, 181, 100, 100})
	if countState(twoThenBack, StateFiring) != 0 {
		t.Fatalf("2 < K=3 out-of-band samples must not raise, got %+v", twoThenBack)
	}

	// Three consecutive out-of-band samples: now it raises.
	three := feed(d, 70_000, []float64{180, 181, 182})
	if countState(three, StateFiring) != 1 {
		t.Fatalf("3 >= K=3 consecutive out-of-band samples must raise exactly once, got %+v", three)
	}
}

func TestThresholdGovernsSensitivity(t *testing.T) {
	// The same moderate deviation should fire under a low threshold and stay silent
	// under a high one.
	mk := func(threshold float64) []Event {
		cfg := testConfig()
		cfg.Threshold = threshold
		cfg.DebounceK = 1
		cfg.Warmup = 20
		d := New(cfg)
		rng := rand.New(rand.NewSource(7))
		base := make([]float64, 80)
		for i := range base {
			base[i] = 50 + rng.NormFloat64() // unit dispersion
		}
		feed(d, 0, base)
		// ~4 standard deviations out.
		return feed(d, 80_000, []float64{54, 54, 54})
	}
	if countState(mk(3.0), StateFiring) == 0 {
		t.Fatalf("a ~4σ deviation should fire under threshold 3.0")
	}
	if countState(mk(6.0), StateFiring) != 0 {
		t.Fatalf("a ~4σ deviation should NOT fire under threshold 6.0")
	}
}

// ── Numerical correctness of the Welford warmup and EWMA recurrence ──────────

func TestWelfordWarmupMatchesExactMeanAndVariance(t *testing.T) {
	cfg := testConfig()
	cfg.Warmup = 8
	d := New(cfg)
	vals := []float64{2, 4, 4, 4, 5, 5, 7, 9} // textbook dataset: mean 5, sample var 32/7
	feed(d, 0, vals)

	st := d.series["s"]
	if st == nil {
		t.Fatal("series state missing after warmup")
	}
	wantMean, wantVar := meanVarSample(vals)
	if math.Abs(st.mean-wantMean) > 1e-9 {
		t.Errorf("Welford mean = %.12f, want %.12f", st.mean, wantMean)
	}
	if math.Abs(st.level-wantMean) > 1e-9 {
		t.Errorf("seeded level = %.12f, want mean %.12f", st.level, wantMean)
	}
	if math.Abs(st.variance-wantVar) > 1e-9 {
		t.Errorf("seeded variance = %.12f, want sample variance %.12f", st.variance, wantVar)
	}
}

func TestEWMARecurrenceMatchesHandComputation(t *testing.T) {
	cfg := testConfig()
	cfg.Warmup = 4
	cfg.Alpha = 0.2
	d := New(cfg)
	// Constant warmup → seeded level 10, variance 0. The floor governs the scale.
	feed(d, 0, []float64{10, 10, 10, 10})
	st := d.series["s"]
	level0, var0 := st.level, st.variance
	if level0 != 10 || var0 != 0 {
		t.Fatalf("warmup seed: level=%v var=%v, want 10/0", level0, var0)
	}

	// One in-band post-warmup sample whose residual is within the clamp, so the
	// recurrence applies unclamped.
	scale := d.scaleFloor(var0, level0) // FloorFrac·|10| + FloorAbs
	resid := 10.3 - level0
	if math.Abs(resid) >= cfg.ClipFactor*scale {
		t.Fatalf("test setup: residual %.4f should be within clamp %.4f", resid, cfg.ClipFactor*scale)
	}
	d.Observe(Sample{Series: "s", Metric: "m", Value: 10.3, TimestampMs: 10_000})

	wantIncr := cfg.Alpha * resid
	wantLevel := level0 + wantIncr
	wantVar := (1 - cfg.Alpha) * (var0 + cfg.Alpha*resid*resid)
	if math.Abs(st.level-wantLevel) > 1e-12 {
		t.Errorf("EWMA level = %.15f, want %.15f", st.level, wantLevel)
	}
	if math.Abs(st.variance-wantVar) > 1e-12 {
		t.Errorf("EWMA variance = %.15f, want %.15f", st.variance, wantVar)
	}
}

func TestScoreIsLocalZScore(t *testing.T) {
	cfg := testConfig()
	cfg.Warmup = 4
	cfg.DebounceK = 1
	cfg.Threshold = 3
	d := New(cfg)
	// Seed level 0 with a known dispersion: values centred at 0 with sample std 1.
	feed(d, 0, []float64{-1.5, -0.5, 0.5, 1.5}) // mean 0, sample var = 5/3
	st := d.series["s"]

	// Capture the pre-update baseline and scale the score will be measured against.
	baseline := st.level
	scale := d.scaleFloor(st.variance, st.level)
	val := 6.0
	wantScore := math.Abs(val-baseline) / scale

	ev, ok := d.Observe(Sample{Series: "s", Metric: "m", Value: val, TimestampMs: 10_000})
	if !ok || ev.State != StateFiring {
		t.Fatalf("expected a firing event for a far-out value, got ok=%v ev=%+v", ok, ev)
	}
	if math.Abs(ev.Score-wantScore) > 1e-12 {
		t.Errorf("score = %.12f, want |value-baseline|/scale = %.12f", ev.Score, wantScore)
	}
	if math.Abs(ev.Baseline-baseline) > 1e-12 {
		t.Errorf("reported baseline = %.12f, want the pre-update level %.12f", ev.Baseline, baseline)
	}
}

// ── Dedup, eviction, buffer, enable gate ────────────────────────────────────

func TestRepeatedTimestampIsDeduped(t *testing.T) {
	d := New(testConfig())
	d.Observe(Sample{Series: "s", Value: 1, TimestampMs: 1000})
	d.Observe(Sample{Series: "s", Value: 2, TimestampMs: 1000}) // same ts → ignored
	d.Observe(Sample{Series: "s", Value: 3, TimestampMs: 500})  // older ts → ignored
	st := d.series["s"]
	if st.count != 1 {
		t.Fatalf("expected 1 counted sample after dedup, got %d", st.count)
	}
	d.Observe(Sample{Series: "s", Value: 4, TimestampMs: 2000}) // advances
	if st.count != 2 {
		t.Fatalf("expected 2 counted samples, got %d", st.count)
	}
}

func TestEvictionBoundsBySeenTime(t *testing.T) {
	d := New(testConfig())
	d.Observe(Sample{Series: "old", Value: 1, TimestampMs: 1_000})
	d.Observe(Sample{Series: "fresh", Value: 1, TimestampMs: 10_000})
	if d.Len() != 2 {
		t.Fatalf("expected 2 series, got %d", d.Len())
	}
	removed := d.Evict(5_000) // drop anything last seen before t=5s
	if removed != 1 {
		t.Fatalf("expected to evict 1 stale series, removed %d", removed)
	}
	if d.Len() != 1 {
		t.Fatalf("expected 1 live series after eviction, got %d", d.Len())
	}
}

func TestEvictingFiringSeriesDecrementsActive(t *testing.T) {
	cfg := testConfig()
	cfg.Warmup = 4
	cfg.DebounceK = 1
	d := New(cfg)
	feed(d, 0, []float64{50, 50, 50, 50}) // seed flat
	d.Observe(Sample{Series: "s", Value: 5000, TimestampMs: 10_000})
	if d.Active() != 1 {
		t.Fatalf("expected 1 active anomaly, got %d", d.Active())
	}
	d.Evict(1_000_000) // evict everything
	if d.Active() != 0 {
		t.Fatalf("evicting a firing series must decrement the active gauge, got %d", d.Active())
	}
}

func TestRecentBufferIsBoundedAndOrdered(t *testing.T) {
	cfg := testConfig()
	cfg.Warmup = 2
	cfg.DebounceK = 1
	cfg.BufferSize = 4
	d := New(cfg)
	// Drive many raise/clear transitions on distinct series so the ring overflows.
	for i := 0; i < 10; i++ {
		s := Sample{Series: string(rune('a' + i)), Value: 0, TimestampMs: 1000}
		d.Observe(s)                                                                            // warmup 1
		d.Observe(Sample{Series: s.Series, Value: 0, TimestampMs: 2000})                        // warmup 2 → seed
		d.Observe(Sample{Series: s.Series, Value: 1e6, TimestampMs: 3000})                      // fire
	}
	recent := d.Recent()
	if len(recent) != cfg.BufferSize {
		t.Fatalf("recent buffer should be bounded to %d, got %d", cfg.BufferSize, len(recent))
	}
	// Oldest-first: sequence numbers must be strictly increasing.
	for i := 1; i < len(recent); i++ {
		if recent[i].Seq <= recent[i-1].Seq {
			t.Fatalf("recent buffer not ordered oldest-first: %v", seqs(recent))
		}
	}
}

func TestDisabledDetectorIsInert(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false
	d := New(cfg)
	events := feed(d, 0, []float64{1, 2, 3, 1e9, 1e9, 1e9, 1e9})
	if len(events) != 0 || d.Len() != 0 {
		t.Fatalf("a disabled detector must ingest nothing and never alert, got events=%d len=%d", len(events), d.Len())
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func meanStd(xs []float64) (float64, float64) {
	m, v := meanVarSample(xs)
	return m, math.Sqrt(v)
}

func meanVarSample(xs []float64) (float64, float64) {
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return mean, ss / float64(len(xs)-1)
}

func seqs(events []Event) []uint64 {
	out := make([]uint64, len(events))
	for i, e := range events {
		out[i] = e.Seq
	}
	return out
}
