package query

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// TestMatrixShapeAndStepAlignment checks the fundamental contract of range
// evaluation: a series gets one point per step that has data, and each output
// point is stamped at the step grid {start, start+step, ...} — not at the raw
// sample timestamps.
func TestMatrixShapeAndStepAlignment(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// A sample every 10s, values 0,10,20,... so each step lands exactly on one.
	for i := 0; i <= 10; i++ {
		db.Ingest("g", map[string]string{"host": "a"}, int64(i)*10_000, float64(i*10))
	}

	engine := NewEngine(db)
	const start, end, step = int64(0), int64(100_000), 10 * time.Second
	res, err := engine.Execute(context.Background(), "g", start, end, step)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 series, got %d", len(res))
	}

	wantSteps := int((end-start)/step.Milliseconds()) + 1 // 11
	if len(res[0].Points) != wantSteps {
		t.Fatalf("expected %d points (one per step with data), got %d", wantSteps, len(res[0].Points))
	}
	for i, p := range res[0].Points {
		wantTS := start + int64(i)*step.Milliseconds()
		if p.Timestamp != wantTS {
			t.Fatalf("point %d stamped at %d, want step-aligned %d", i, p.Timestamp, wantTS)
		}
		if p.Value != float64(i*10) {
			t.Fatalf("point %d value %v, want %v", i, p.Value, float64(i*10))
		}
	}
}

// TestInstantVectorStalenessHeldThenGap verifies look-back semantics: the most
// recent sample is held forward for up to the look-back delta (5m), after which
// the series produces a gap (no point) rather than a stale or zero value.
func TestInstantVectorStalenessHeldThenGap(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	db.Ingest("g", map[string]string{"host": "a"}, 0, 7)
	db.Ingest("g", map[string]string{"host": "a"}, 10_000, 9) // last sample; nothing after

	engine := NewEngine(db)
	// Steps every 100s out to 700s. Look-back is 5m=300s from the last sample at 10s,
	// so the held value 9 survives through t=300s and then the series goes stale.
	res, err := engine.Execute(context.Background(), "g", 0, 700_000, 100*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 series, got %d", len(res))
	}

	want := []storage.Point{
		{Timestamp: 0, Value: 7},       // exact sample
		{Timestamp: 100_000, Value: 9}, // held
		{Timestamp: 200_000, Value: 9}, // held
		{Timestamp: 300_000, Value: 9}, // held, at the look-back edge (300s-10s=290s <= 5m)
		// t=400_000: 400s-10s=390s > 5m → stale gap, and everything after.
	}
	if len(res[0].Points) != len(want) {
		t.Fatalf("expected %d points (held then gap), got %d: %+v", len(want), len(res[0].Points), res[0].Points)
	}
	for i, w := range want {
		if res[0].Points[i] != w {
			t.Fatalf("point %d: got %+v, want %+v", i, res[0].Points[i], w)
		}
	}
}

// TestPerStepSumByOverTime checks per-step aggregation with grouping: sum by (dc)
// must produce one multi-point series per group, aggregated independently at each
// step.
func TestPerStepSumByOverTime(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	for i, ts := range []int64{0, 10_000, 20_000} {
		f := float64(i + 1) // 1,2,3
		db.Ingest("m", map[string]string{"dc": "x", "host": "a"}, ts, 10*f)
		db.Ingest("m", map[string]string{"dc": "x", "host": "b"}, ts, f)
		db.Ingest("m", map[string]string{"dc": "y", "host": "c"}, ts, 100*f)
	}

	engine := NewEngine(db)
	res, err := engine.Execute(context.Background(), "sum(m) by (dc)", 0, 20_000, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(res))
	}

	byDC := map[string][]storage.Point{}
	for _, s := range res {
		byDC[s.Labels["dc"]] = s.Points
	}
	wantX := []storage.Point{{Timestamp: 0, Value: 11}, {Timestamp: 10_000, Value: 22}, {Timestamp: 20_000, Value: 33}}
	wantY := []storage.Point{{Timestamp: 0, Value: 100}, {Timestamp: 10_000, Value: 200}, {Timestamp: 20_000, Value: 300}}
	for _, tc := range []struct {
		dc   string
		want []storage.Point
	}{{"x", wantX}, {"y", wantY}} {
		got := byDC[tc.dc]
		if len(got) != len(tc.want) {
			t.Fatalf("dc=%s: expected %d points, got %d: %+v", tc.dc, len(tc.want), len(got), got)
		}
		for i, w := range tc.want {
			if got[i] != w {
				t.Fatalf("dc=%s point %d: got %+v, want %+v", tc.dc, i, got[i], w)
			}
		}
	}
}

// TestPerStepVectorDivision checks per-step vector÷vector matching across a range:
// the quotient is computed at every step where both sides have a matching series.
func TestPerStepVectorDivision(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	reqs := []float64{100, 200, 400}
	errs := []float64{10, 40, 40}
	for i, ts := range []int64{0, 10_000, 20_000} {
		db.Ingest("requests", map[string]string{"host": "a"}, ts, reqs[i])
		db.Ingest("errors", map[string]string{"host": "a"}, ts, errs[i])
	}

	engine := NewEngine(db)
	res, err := engine.Execute(context.Background(), "errors / requests", 0, 20_000, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 matched series, got %d", len(res))
	}
	if _, ok := res[0].Labels["__name__"]; ok {
		t.Fatalf("vector/vector result must drop __name__: %+v", res[0].Labels)
	}
	want := []storage.Point{{Timestamp: 0, Value: 0.1}, {Timestamp: 10_000, Value: 0.2}, {Timestamp: 20_000, Value: 0.1}}
	if len(res[0].Points) != len(want) {
		t.Fatalf("expected %d ratio points, got %d: %+v", len(want), len(res[0].Points), res[0].Points)
	}
	for i, w := range want {
		if res[0].Points[i].Timestamp != w.Timestamp || math.Abs(res[0].Points[i].Value-w.Value) > 1e-9 {
			t.Fatalf("point %d: got %+v, want %+v", i, res[0].Points[i], w)
		}
	}
}

// TestRateCounterResetAcrossSteps ensures a counter reset that falls between step
// windows does not produce a negative or NaN rate: every step's rate stays a
// sensible positive value, because rate() corrects the reset within each window.
func TestRateCounterResetAcrossSteps(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Counter rising ~10/s but reset at ts=30s (200 → 50). Ingest in timestamp order:
	// the storage layer rejects out-of-order samples, so a map's random iteration
	// order would otherwise drop points.
	samples := []storage.Point{
		{Timestamp: 0, Value: 0}, {Timestamp: 10_000, Value: 100}, {Timestamp: 20_000, Value: 200},
		{Timestamp: 30_000, Value: 50}, {Timestamp: 40_000, Value: 150}, {Timestamp: 50_000, Value: 250},
		{Timestamp: 60_000, Value: 350},
	}
	for _, s := range samples {
		db.Ingest("c_total", map[string]string{"job": "x"}, s.Timestamp, s.Value)
	}

	engine := NewEngine(db)
	res, err := engine.Execute(context.Background(), "rate(c_total[30s])", 30_000, 60_000, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 series, got %d", len(res))
	}
	if len(res[0].Points) != 4 { // steps 30s,40s,50s,60s — each window has >=2 samples
		t.Fatalf("expected 4 rate points, got %d: %+v", len(res[0].Points), res[0].Points)
	}
	for _, p := range res[0].Points {
		if math.IsNaN(p.Value) || p.Value <= 0 || p.Value > 50 {
			t.Fatalf("rate across reset must stay sensibly positive; got %v at %d", p.Value, p.Timestamp)
		}
	}
}

// TestSeriesAppearDisappearGaps checks that series entering and leaving over the
// range contribute points only where they have data: a late-starting series has no
// points before it appears, and a series whose samples stop produces a gap once it
// passes the look-back delta — never a zero fill.
func TestSeriesAppearDisappearGaps(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	for _, ts := range []int64{0, 10_000, 20_000} {
		db.Ingest("metric", map[string]string{"s": "a"}, ts, 3) // a: only early
	}
	for _, ts := range []int64{200_000, 210_000, 220_000} {
		db.Ingest("metric", map[string]string{"s": "b"}, ts, 9) // b: only later
	}

	engine := NewEngine(db)
	res, err := engine.Execute(context.Background(), "metric", 0, 600_000, 100*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 series, got %d", len(res))
	}

	bySeries := map[string][]storage.Point{}
	for _, s := range res {
		bySeries[s.Labels["s"]] = s.Points
	}

	a := bySeries["a"]
	if len(a) == 0 || a[0].Timestamp != 0 {
		t.Fatalf("series a should start at t=0: %+v", a)
	}
	for _, p := range a {
		if p.Timestamp >= 400_000 {
			t.Fatalf("series a went stale after 5m past its last sample (20s); unexpected point at %d", p.Timestamp)
		}
	}

	b := bySeries["b"]
	if len(b) == 0 {
		t.Fatal("series b should have points once it appears")
	}
	for _, p := range b {
		if p.Timestamp < 200_000 {
			t.Fatalf("series b has no data before it appears at 200s; unexpected point at %d", p.Timestamp)
		}
	}
}

// TestNaNPropagation verifies a NaN sample flows through aggregation as NaN rather
// than being dropped or coerced to zero.
func TestNaNPropagation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	db.Ingest("m", map[string]string{"h": "a"}, 1000, math.NaN())
	db.Ingest("m", map[string]string{"h": "b"}, 1000, 5)

	engine := NewEngine(db)
	res, err := engine.Execute(context.Background(), "sum(m)", 1000, 1000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || len(res[0].Points) != 1 {
		t.Fatalf("expected one aggregated point, got %+v", res)
	}
	if !math.IsNaN(res[0].Points[0].Value) {
		t.Fatalf("NaN must propagate through sum, got %v", res[0].Points[0].Value)
	}
}

// TestStartEqualsEndInstant exercises the single-instant path: start == end yields
// a one-point matrix carrying the value at that instant.
func TestStartEqualsEndInstant(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.Ingest("g", map[string]string{"host": "a"}, 1000, 42)

	engine := NewEngine(db)
	for _, step := range []time.Duration{0, 15 * time.Second} { // step is irrelevant at a single instant
		res, err := engine.Execute(context.Background(), "g", 1000, 1000, step)
		if err != nil {
			t.Fatalf("step %v: %v", step, err)
		}
		if len(res) != 1 || len(res[0].Points) != 1 {
			t.Fatalf("step %v: expected one series with one point, got %+v", step, res)
		}
		if res[0].Points[0] != (storage.Point{Timestamp: 1000, Value: 42}) {
			t.Fatalf("step %v: got %+v, want {1000, 42}", step, res[0].Points[0])
		}
	}
}

// TestMaxStepCountError checks the DoS guard: a step grid larger than the cap is
// rejected with a clear error, while a grid just under the cap is accepted.
func TestMaxStepCountError(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	engine := NewEngine(db)

	// 1ms step over a 20s span → 20001 steps, over the 11000 cap.
	_, err := engine.Execute(context.Background(), "x", 0, 20_000, time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the step count exceeds the cap")
	}
	if !strings.Contains(err.Error(), "exceeding the maximum") {
		t.Fatalf("error should explain the cap, got: %v", err)
	}

	// Just under the cap must succeed (empty matrix for a missing metric).
	res, err := engine.Execute(context.Background(), "x", 0, 10_000, time.Millisecond) // 10001 steps
	if err != nil {
		t.Fatalf("a sub-cap query must not error: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("expected empty matrix for a missing metric, got %d series", len(res))
	}
}

// TestDefaultStepDoesNotOversample verifies that dense raw data is sampled onto the
// step grid: with no step the range yields ~250 points, not one per raw sample, so
// a wide query over fine-grained data cannot blow up into millions of points.
func TestDefaultStepDoesNotOversample(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	const rawSamples = 1000
	batch := make([]storage.IngestSample, rawSamples)
	for i := range batch { // a sample every 1s for ~16.6 minutes
		batch[i] = storage.IngestSample{Name: "dense", Labels: map[string]string{"host": "a"}, Timestamp: int64(i) * 1000, Value: float64(i)}
	}
	if err := db.IngestBatch(batch); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(db)
	res, err := engine.Execute(context.Background(), "dense", 0, int64(rawSamples-1)*1000, 0) // step 0 → adaptive
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 series, got %d", len(res))
	}
	n := len(res[0].Points)
	if n >= rawSamples {
		t.Fatalf("default step oversampled dense data: %d points for %d raw samples", n, rawSamples)
	}
	if n < 200 || n > 300 { // ~250 target
		t.Fatalf("default step should yield ~250 points, got %d", n)
	}
}
