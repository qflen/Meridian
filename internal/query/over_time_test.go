package query

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestOverTimeFunctionsRaw checks every *_over_time function against a hand-computed
// window over raw data. A ramp 1,2,…,10 is ingested at 1s spacing; an instant query at
// t=10s with a 10s range selects the half-open window (0,10s] = values 1..10.
func TestOverTimeFunctionsRaw(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	for i := 1; i <= 10; i++ {
		ts := int64(i) * 1000
		if err := db.Ingest("m", map[string]string{"host": "a"}, ts, float64(i)); err != nil {
			t.Fatal(err)
		}
	}

	eng := NewEngine(db)
	ctx := context.Background()
	const at = int64(10000)

	cases := []struct {
		fn   string
		want float64
	}{
		{"min_over_time", 1},
		{"max_over_time", 10},
		{"sum_over_time", 55},   // 1+2+…+10
		{"count_over_time", 10}, // ten samples in the window
		{"avg_over_time", 5.5},  // 55/10
		{"last_over_time", 10},  // newest sample in (0,10s]
	}
	for _, c := range cases {
		res, err := eng.Execute(ctx, c.fn+"(m[10s])", at, at, 0)
		if err != nil {
			t.Fatalf("%s: %v", c.fn, err)
		}
		if len(res) != 1 || len(res[0].Points) != 1 {
			t.Fatalf("%s: expected one series with one point, got %+v", c.fn, res)
		}
		if got := res[0].Points[0].Value; math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: got %v, want %v", c.fn, got, c.want)
		}
		if got := res[0].Points[0].Timestamp; got != at {
			t.Errorf("%s: timestamp %d, want %d (step-aligned)", c.fn, got, at)
		}
	}
}

// TestOverTimeHalfOpenWindow pins the (t-d, t] half-open windowing: the sample exactly at
// the lower bound is excluded, the one exactly at t is included.
func TestOverTimeHalfOpenWindow(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	// Samples at 0,1s,2s,3s with values 100,1,2,3.
	vals := []float64{100, 1, 2, 3}
	for i, v := range vals {
		if err := db.Ingest("m", map[string]string{"host": "a"}, int64(i)*1000, v); err != nil {
			t.Fatal(err)
		}
	}
	eng := NewEngine(db)
	// At t=3s with a 3s range the window is (0,3s] → {1,2,3}; the 100 at ts=0 is excluded.
	res, err := eng.Execute(context.Background(), "max_over_time(m[3s])", 3000, 3000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || len(res[0].Points) != 1 {
		t.Fatalf("expected one point, got %+v", res)
	}
	if got := res[0].Points[0].Value; got != 3 {
		t.Fatalf("max_over_time excluded-boundary: got %v, want 3 (100 at the open lower bound must not count)", got)
	}
}

// TestOverTimeMatrixPerSeries checks the stepped matrix path: each series is reduced
// independently at each step, and a window with no samples yields a gap (no point).
func TestOverTimeMatrixPerSeries(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	// host a: 1..6 at 1s spacing; host b: only two early samples, so its later windows
	// are empty.
	for i := 1; i <= 6; i++ {
		if err := db.Ingest("m", map[string]string{"host": "a"}, int64(i)*1000, float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 2; i++ {
		if err := db.Ingest("m", map[string]string{"host": "b"}, int64(i)*1000, float64(i*10)); err != nil {
			t.Fatal(err)
		}
	}
	eng := NewEngine(db)
	// Steps at 2s,4s,6s with a 2s window: a → {1,2},{3,4},{5,6}; b → {10,20},{},{}.
	res, err := eng.Execute(context.Background(), "sum_over_time(m[2s])", 2000, 6000, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	byHost := map[string][]float64{}
	for _, s := range res {
		for _, p := range s.Points {
			byHost[s.Labels["host"]] = append(byHost[s.Labels["host"]], p.Value)
		}
	}
	if got := byHost["a"]; len(got) != 3 || got[0] != 3 || got[1] != 7 || got[2] != 11 {
		t.Fatalf("host a sums: got %v, want [3 7 11]", got)
	}
	// host b only has data in the first window; the later two windows are gaps.
	if got := byHost["b"]; len(got) != 1 || got[0] != 30 {
		t.Fatalf("host b sums: got %v, want [30] (empty windows must be gaps, not zeros)", got)
	}
}

// TestOverTimeArgValidation rejects a non-range argument with a clear error.
func TestOverTimeArgValidation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	eng := NewEngine(db)
	if _, err := eng.Execute(context.Background(), "max_over_time(m)", 0, 0, 0); err == nil {
		t.Fatal("expected an error for max_over_time over an instant (non-range) vector")
	}
}
