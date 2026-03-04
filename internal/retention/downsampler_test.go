package retention

import (
	"math"
	"testing"

	"github.com/meridiandb/meridian/internal/storage"
)

func TestRollupBasic(t *testing.T) {
	// 60 points at 1-second intervals → rollup into 1-minute windows
	points := make([]storage.Point, 120)
	for i := range points {
		points[i] = storage.Point{
			Timestamp: int64(i) * 1000, // ms
			Value:     float64(i),
		}
	}

	results := Rollup(points, 60000) // 1-minute windows
	if len(results) != 2 {
		t.Fatalf("expected 2 rollup windows, got %d", len(results))
	}

	// First window: values 0–59
	r0 := results[0]
	if r0.Min != 0 {
		t.Fatalf("window 0 min: %f", r0.Min)
	}
	if r0.Max != 59 {
		t.Fatalf("window 0 max: %f", r0.Max)
	}
	if r0.Count != 60 {
		t.Fatalf("window 0 count: %d", r0.Count)
	}
	expectedAvg := (0.0 + 59.0) / 2.0 // arithmetic mean of 0..59
	if math.Abs(r0.Avg-expectedAvg) > 0.01 {
		t.Fatalf("window 0 avg: got %f, want %f", r0.Avg, expectedAvg)
	}
	expectedSum := float64(60*59) / 2.0
	if r0.Sum != expectedSum {
		t.Fatalf("window 0 sum: got %f, want %f", r0.Sum, expectedSum)
	}

	// Second window: values 60–119
	r1 := results[1]
	if r1.Min != 60 {
		t.Fatalf("window 1 min: %f", r1.Min)
	}
	if r1.Max != 119 {
		t.Fatalf("window 1 max: %f", r1.Max)
	}
	if r1.Count != 60 {
		t.Fatalf("window 1 count: %d", r1.Count)
	}
}

func TestRollupEmptyInput(t *testing.T) {
	results := Rollup(nil, 60000)
	if results != nil {
		t.Fatal("expected nil for empty input")
	}
}

func TestRollupSinglePoint(t *testing.T) {
	points := []storage.Point{{Timestamp: 5000, Value: 42.0}}
	results := Rollup(points, 60000)
	if len(results) != 1 {
		t.Fatalf("expected 1 window, got %d", len(results))
	}
	if results[0].Min != 42.0 || results[0].Max != 42.0 || results[0].Count != 1 {
		t.Fatalf("unexpected: %+v", results[0])
	}
}

func TestRollupIrregularTimestamps(t *testing.T) {
	points := []storage.Point{
		{Timestamp: 1000, Value: 10},
		{Timestamp: 25000, Value: 20},
		{Timestamp: 55000, Value: 30},
		{Timestamp: 65000, Value: 40},
		{Timestamp: 90000, Value: 50},
	}

	results := Rollup(points, 60000) // 1-minute windows
	if len(results) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(results))
	}
	if results[0].Count != 3 { // first 3 points in [0, 60000)
		t.Fatalf("window 0 count: %d", results[0].Count)
	}
	if results[1].Count != 2 { // last 2 points in [60000, 120000)
		t.Fatalf("window 1 count: %d", results[1].Count)
	}
}

func TestRollup5sTo1m(t *testing.T) {
	// Simulate 5-second interval data rolled up to 1-minute windows
	n := 300 // 5 minutes of data at 5s intervals
	points := make([]storage.Point, n)
	for i := range points {
		points[i] = storage.Point{
			Timestamp: int64(i) * 5000,
			Value:     float64(50 + i%12), // cycling pattern
		}
	}

	results := Rollup(points, 60000)
	// 300 * 5s = 1500s = 25 minutes → 25 windows
	if len(results) != 25 {
		t.Fatalf("expected 25 windows, got %d", len(results))
	}

	// Each window should have 12 points (60s / 5s)
	for i, r := range results {
		if r.Count != 12 {
			t.Fatalf("window %d: expected 12 points, got %d", i, r.Count)
		}
	}
}

func TestRollup1mTo1h(t *testing.T) {
	// 1-minute data rolled up to 1-hour windows
	n := 120 // 2 hours of 1-minute data
	points := make([]storage.Point, n)
	for i := range points {
		points[i] = storage.Point{
			Timestamp: int64(i) * 60000,
			Value:     float64(i),
		}
	}

	results := Rollup(points, 3600000) // 1 hour
	if len(results) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(results))
	}
	if results[0].Count != 60 {
		t.Fatalf("window 0 count: %d", results[0].Count)
	}
}

// makeUnevenRaw builds raw points spanning `hours` whole hours where each minute
// holds a DIFFERENT number of samples, so 1-minute windows have unequal counts. That
// inequality is exactly what makes a count-weighted hourly average diverge from a
// plain mean of the minute-averages — it is what the weighted cascade must get right.
func makeUnevenRaw(hours int) []storage.Point {
	var points []storage.Point
	for h := 0; h < hours; h++ {
		for m := 0; m < 60; m++ {
			minuteStart := int64(h)*3600000 + int64(m)*60000
			samplesThisMinute := (m%7 + 1) + (h % 3) // 1..9, varies by minute and hour
			for j := 0; j < samplesThisMinute; j++ {
				points = append(points, storage.Point{
					Timestamp: minuteStart + int64(j)*1000, // within the minute
					Value:     float64(h*1000+m*10) + float64(j)*0.5,
				})
			}
		}
	}
	return points
}

func rollupsEqual(t *testing.T, label string, a, b RollupResult) {
	t.Helper()
	if a.Timestamp != b.Timestamp {
		t.Fatalf("%s: timestamp %d != %d", label, a.Timestamp, b.Timestamp)
	}
	if a.Min != b.Min {
		t.Fatalf("%s: min %v != %v", label, a.Min, b.Min)
	}
	if a.Max != b.Max {
		t.Fatalf("%s: max %v != %v", label, a.Max, b.Max)
	}
	if a.Count != b.Count {
		t.Fatalf("%s: count %d != %d", label, a.Count, b.Count)
	}
	if math.Abs(a.Sum-b.Sum) > 1e-6 {
		t.Fatalf("%s: sum %v != %v", label, a.Sum, b.Sum)
	}
	if math.Abs(a.Avg-b.Avg) > 1e-9 {
		t.Fatalf("%s: avg %v != %v", label, a.Avg, b.Avg)
	}
}

// TestWeightedCascadeEquivalence is the core A16 proof: a 1h tier built by chaining
// the 1m tier must equal — for ALL five aggregates — the 1h tier built directly from
// raw. It uses uneven per-minute counts so a naive (unweighted) average would fail.
func TestWeightedCascadeEquivalence(t *testing.T) {
	raw := makeUnevenRaw(3) // 3 whole hours

	direct1h := Rollup(raw, 3600000)
	via1m := Rollup(raw, 60000)
	chained1h := ChainRollups(via1m, 3600000)

	if len(direct1h) != 3 {
		t.Fatalf("expected 3 hourly windows, got %d", len(direct1h))
	}
	if len(chained1h) != len(direct1h) {
		t.Fatalf("chained produced %d windows, direct %d", len(chained1h), len(direct1h))
	}
	if len(via1m) != 3*60 {
		t.Fatalf("expected %d minute windows, got %d", 3*60, len(via1m))
	}

	for i := range direct1h {
		rollupsEqual(t, "hour", chained1h[i], direct1h[i])
	}

	// Guard the guard: confirm the data really is uneven, so a naive mean-of-minute-
	// averages would have produced a different hourly average than the weighted one.
	for i, hour := range direct1h {
		lo, hi := i*60, i*60+60
		var naive float64
		for _, m := range via1m[lo:hi] {
			naive += m.Avg
		}
		naive /= 60
		if math.Abs(naive-hour.Avg) < 1e-9 {
			t.Fatalf("hour %d: counts not uneven enough — naive avg %v == weighted %v; test cannot catch the bug",
				i, naive, hour.Avg)
		}
	}
}

// TestWindowAggregatesKnownValues checks every aggregate against hand-computed values
// for a single known window.
func TestWindowAggregatesKnownValues(t *testing.T) {
	// One 1-minute window holding values 10, 4, 7, 20, 9 at distinct timestamps.
	vals := []float64{10, 4, 7, 20, 9}
	points := make([]storage.Point, len(vals))
	for i, v := range vals {
		points[i] = storage.Point{Timestamp: int64(i) * 1000, Value: v}
	}
	r := Rollup(points, 60000)
	if len(r) != 1 {
		t.Fatalf("expected 1 window, got %d", len(r))
	}
	w := r[0]
	if w.Min != 4 || w.Max != 20 {
		t.Fatalf("min/max: %v/%v want 4/20", w.Min, w.Max)
	}
	if w.Count != 5 {
		t.Fatalf("count: %d want 5", w.Count)
	}
	if w.Sum != 50 {
		t.Fatalf("sum: %v want 50", w.Sum)
	}
	if w.Avg != 10 {
		t.Fatalf("avg: %v want 10", w.Avg)
	}
	if w.Timestamp != 30000 {
		t.Fatalf("centre: %d want 30000", w.Timestamp)
	}
}
