package query

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// TestRateDividesByRange checks that rate() divides the increase by the selector
// range (not the sample span) and extrapolates Prometheus-style. The samples
// cover only the first 2 minutes of a 5-minute window, so dividing by the span
// (the old behavior) would report 10/s, while dividing by the range reports 5/s.
func TestRateDividesByRange(t *testing.T) {
	// 3 samples, +600 every 60s → local slope 10/s, but only 120s of a 300s window.
	points := []storage.Point{
		{Timestamp: 0, Value: 0},
		{Timestamp: 60_000, Value: 600},
		{Timestamp: 120_000, Value: 1200},
	}
	got, ok := rate(points, 300_000, 300_000)
	if !ok {
		t.Fatal("rate returned no value")
	}
	if math.Abs(got-5.0) > 0.01 {
		t.Fatalf("rate over [5m]: got %v, want 5.0 (divide by 300s range, not 120s span)", got)
	}
}

// TestRateCounterReset checks reset handling: a drop is treated as the counter
// restarting, so the post-reset value is added to the increase.
func TestRateCounterReset(t *testing.T) {
	points := []storage.Point{
		{Timestamp: 0, Value: 0},
		{Timestamp: 60_000, Value: 600},
		{Timestamp: 120_000, Value: 200}, // reset from 600
	}
	// increase = 600 + 200 = 800; factor = (150/120)/300 → 800*1.25/300 = 3.3333/s
	got, ok := rate(points, 300_000, 300_000)
	if !ok {
		t.Fatal("rate returned no value")
	}
	if math.Abs(got-3.3333) > 0.01 {
		t.Fatalf("rate with reset: got %v, want ~3.3333", got)
	}
}

// TestRateSparseInWideWindowEndToEnd drives the same scenario through the engine
// to confirm the range duration is threaded in and the window is anchored at end.
func TestRateSparseInWideWindowEndToEnd(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.Ingest("c", map[string]string{"job": "x"}, 0, 0)
	db.Ingest("c", map[string]string{"job": "x"}, 60_000, 600)
	db.Ingest("c", map[string]string{"job": "x"}, 120_000, 1200)
	engine := NewEngine(db)

	// Evaluate the instant at end=300000 over a [5m] window.
	res, err := engine.Execute(context.Background(), "rate(c[5m])", 300_000, 300_000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || len(res[0].Points) != 1 {
		t.Fatalf("expected one rate point, got %+v", res)
	}
	if math.Abs(res[0].Points[0].Value-5.0) > 0.01 {
		t.Fatalf("end-to-end rate: got %v, want 5.0", res[0].Points[0].Value)
	}
}
