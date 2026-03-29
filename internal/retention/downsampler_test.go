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
