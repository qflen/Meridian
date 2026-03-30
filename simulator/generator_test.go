package simulator

import (
	"testing"
	"time"
)

func TestGeneratorProducesMetrics(t *testing.T) {
	profiles := DefaultProfiles()
	gen := NewGenerator(profiles)

	ts := time.Date(2024, 1, 15, 14, 0, 0, 0, time.UTC) // peak hour
	samples := gen.Generate(ts)

	if len(samples) == 0 {
		t.Fatal("expected samples")
	}

	// Count total metrics across all profiles
	expected := 0
	for _, p := range profiles {
		expected += len(p.Metrics)
	}
	if len(samples) != expected {
		t.Fatalf("expected %d samples, got %d", expected, len(samples))
	}

	t.Logf("Generated %d samples for %d hosts", len(samples), len(profiles))
}

func TestCounterMonotonicity(t *testing.T) {
	profiles := []HostProfile{{
		Hostname: "test",
		Role:     "web",
		Metrics: []MetricProfile{
			{Name: "counter", Type: MetricCounter, BaseVal: 100, NoiseAmp: 10, Labels: map[string]string{"host": "test"}},
		},
	}}
	gen := NewGenerator(profiles)

	var prevVal float64
	ts := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 1000; i++ {
		samples := gen.Generate(ts.Add(time.Duration(i) * 5 * time.Second))
		if len(samples) != 1 {
			t.Fatal("expected 1 sample")
		}
		if samples[0].Value < prevVal {
			t.Fatalf("counter decreased at tick %d: %f < %f", i, samples[0].Value, prevVal)
		}
		prevVal = samples[0].Value
	}
}

func TestGaugeWithinBounds(t *testing.T) {
	profiles := []HostProfile{{
		Hostname: "test",
		Role:     "web",
		Metrics: []MetricProfile{
			{Name: "cpu", Type: MetricGaugeCPU, BaseVal: 50, NoiseAmp: 5, Labels: map[string]string{"host": "test"}},
		},
	}}
	gen := NewGenerator(profiles)

	ts := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		samples := gen.Generate(ts.Add(time.Duration(i) * 5 * time.Second))
		val := samples[0].Value
		if val < 0 || val > 100 {
			t.Fatalf("CPU out of bounds at tick %d: %f", i, val)
		}
	}
}

func TestDiurnalPattern(t *testing.T) {
	peakVal := diurnalPattern(14.0)  // 2 PM - peak
	troughVal := diurnalPattern(4.0) // 4 AM - trough

	t.Logf("peak (14:00): %f, trough (04:00): %f", peakVal, troughVal)

	if peakVal <= troughVal {
		t.Fatalf("peak should be greater than trough: %f <= %f", peakVal, troughVal)
	}
	if peakVal < 0.7 {
		t.Fatalf("peak too low: %f", peakVal)
	}
	if troughVal > 0.3 {
		t.Fatalf("trough too high: %f", troughVal)
	}
}

func TestRatioWithinBounds(t *testing.T) {
	profiles := []HostProfile{{
		Hostname: "test",
		Role:     "cache",
		Metrics: []MetricProfile{
			{Name: "hit_ratio", Type: MetricGaugeRatio, BaseVal: 0.95, NoiseAmp: 0.03, Labels: map[string]string{"host": "test"}},
		},
	}}
	gen := NewGenerator(profiles)

	ts := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		samples := gen.Generate(ts.Add(time.Duration(i) * 5 * time.Second))
		val := samples[0].Value
		if val < 0 || val > 1 {
			t.Fatalf("ratio out of bounds at tick %d: %f", i, val)
		}
	}
}

func TestDefaultProfilesHostCount(t *testing.T) {
	profiles := DefaultProfiles()
	if len(profiles) != 8 {
		t.Fatalf("expected 8 hosts, got %d", len(profiles))
	}

	roles := make(map[string]int)
	for _, p := range profiles {
		roles[p.Role]++
	}
	if roles["web"] != 3 {
		t.Fatalf("expected 3 web hosts, got %d", roles["web"])
	}
	if roles["database"] != 2 {
		t.Fatalf("expected 2 db hosts, got %d", roles["database"])
	}
}

func TestMetricSampleCount(t *testing.T) {
	profiles := DefaultProfiles()
	gen := NewGenerator(profiles)

	ts := time.Now()
	samples := gen.Generate(ts)

	// Count unique metric names
	names := make(map[string]bool)
	for _, s := range samples {
		names[s.Name] = true
	}
	t.Logf("Unique metrics: %d, total samples: %d", len(names), len(samples))

	// Should have ~40 total samples (as per spec)
	if len(samples) < 35 || len(samples) > 50 {
		t.Fatalf("expected ~40 samples, got %d", len(samples))
	}
}
