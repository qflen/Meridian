package query

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

func setupTestDB(t *testing.T) *storage.TSDB {
	t.Helper()
	dir := t.TempDir()
	opts := storage.DefaultTSDBOptions()
	opts.WALDir = dir + "/wal"
	opts.BlockDir = dir + "/blocks"
	opts.FlushInterval = 1 * time.Hour

	db, err := storage.Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestExecutorRateComputation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Ingest a monotonically increasing counter rising 10/s for 3 minutes.
	for i := 0; i <= 180; i++ {
		ts := int64(i) * 1000 // 1-second intervals
		db.Ingest("http_requests_total", map[string]string{"method": "GET"}, ts, float64(i*10))
	}

	engine := NewEngine(db)
	// Range query: evaluate rate at every 15s step over [2m, 3m]. Each step's 1m
	// window is fully populated, so rate() is a multi-point series ~10/s, not one
	// number. This is the per-step rate that single-instant evaluation could not
	// produce.
	const step = 15 * time.Second
	results, err := engine.Execute(context.Background(), "rate(http_requests_total[1m])", 120000, 180000, step)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result series, got %d", len(results))
	}
	pts := results[0].Points
	if len(pts) != 5 { // steps at 120s,135s,150s,165s,180s
		t.Fatalf("expected 5 rate points across the range, got %d: %+v", len(pts), pts)
	}
	for i, p := range pts {
		wantTS := int64(120000) + int64(i)*step.Milliseconds()
		if p.Timestamp != wantTS {
			t.Fatalf("point %d timestamp: got %d, want %d (step-aligned)", i, p.Timestamp, wantTS)
		}
		if math.Abs(p.Value-10.0) > 0.5 {
			t.Fatalf("point %d rate: got %f, want ~10.0", i, p.Value)
		}
	}
}

func TestExecutorSumAggregation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	for i := 0; i < 10; i++ {
		ts := int64(i) * 5000
		db.Ingest("cpu_usage", map[string]string{"host": "web-01"}, ts, float64(40+i))
		db.Ingest("cpu_usage", map[string]string{"host": "web-02"}, ts, float64(60+i))
	}

	engine := NewEngine(db)
	const step = 15 * time.Second
	results, err := engine.Execute(context.Background(), "sum(cpu_usage)", 0, 50000, step)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 aggregated series, got %d", len(results))
	}
	// The sum is aggregated independently at each 15s step over the instant vector
	// (most recent sample per host within the look-back window). Steps land on
	// 0,15s,30s,45s; the held samples there are i=0,3,6,9 → (40+60),(43+63),... .
	want := []storage.Point{
		{Timestamp: 0, Value: 100},
		{Timestamp: 15000, Value: 106},
		{Timestamp: 30000, Value: 112},
		{Timestamp: 45000, Value: 118},
	}
	if len(results[0].Points) != len(want) {
		t.Fatalf("expected %d step points, got %d: %+v", len(want), len(results[0].Points), results[0].Points)
	}
	for i, w := range want {
		got := results[0].Points[i]
		if got.Timestamp != w.Timestamp || got.Value != w.Value {
			t.Fatalf("point %d: got %+v, want %+v", i, got, w)
		}
	}
}

func TestExecutorAvgAggregation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	for i := 0; i < 5; i++ {
		db.Ingest("cpu_usage", map[string]string{"host": "web-01"}, int64(i)*1000, 40)
		db.Ingest("cpu_usage", map[string]string{"host": "web-02"}, int64(i)*1000, 60)
	}

	engine := NewEngine(db)
	results, err := engine.Execute(context.Background(), "avg(cpu_usage)", 0, 5000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 series, got %d", len(results))
	}
	// avg(40, 60) = 50
	if results[0].Points[0].Value != 50 {
		t.Fatalf("avg: got %f, want 50", results[0].Points[0].Value)
	}
}

func TestExecutorMaxMinAggregation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	db.Ingest("metric", map[string]string{"host": "a"}, 1000, 10)
	db.Ingest("metric", map[string]string{"host": "b"}, 1000, 30)
	db.Ingest("metric", map[string]string{"host": "c"}, 1000, 20)

	engine := NewEngine(db)

	// Evaluate at the instant the samples exist (start == end → single instant);
	// the result is a one-point matrix carrying the aggregate at that step.
	maxResults, err := engine.Execute(context.Background(), "max(metric)", 1000, 1000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if maxResults[0].Points[0].Value != 30 {
		t.Fatalf("max: got %f, want 30", maxResults[0].Points[0].Value)
	}

	// Test min
	minResults, err := engine.Execute(context.Background(), "min(metric)", 1000, 1000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if minResults[0].Points[0].Value != 10 {
		t.Fatalf("min: got %f, want 10", minResults[0].Points[0].Value)
	}
}

func TestExecutorGroupBy(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	db.Ingest("http_requests", map[string]string{"method": "GET", "host": "a"}, 1000, 100)
	db.Ingest("http_requests", map[string]string{"method": "GET", "host": "b"}, 1000, 200)
	db.Ingest("http_requests", map[string]string{"method": "POST", "host": "a"}, 1000, 50)
	db.Ingest("http_requests", map[string]string{"method": "POST", "host": "b"}, 1000, 75)

	engine := NewEngine(db)
	results, err := engine.Execute(context.Background(), `sum(http_requests) by (method)`, 1000, 1000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(results))
	}

	for _, r := range results {
		method := r.Labels["method"]
		val := r.Points[0].Value
		switch method {
		case "GET":
			if val != 300 {
				t.Fatalf("GET sum: %f", val)
			}
		case "POST":
			if val != 125 {
				t.Fatalf("POST sum: %f", val)
			}
		default:
			t.Fatalf("unexpected method: %s", method)
		}
	}
}

func TestExecutorArithmetic(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	db.Ingest("cpu", nil, 1000, 0.45)

	engine := NewEngine(db)
	results, err := engine.Execute(context.Background(), "cpu * 100", 1000, 1000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 series, got %d", len(results))
	}
	if math.Abs(results[0].Points[0].Value-45.0) > 0.001 {
		t.Fatalf("expected 45.0, got %f", results[0].Points[0].Value)
	}
}

func TestExecutorEmptyResult(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	engine := NewEngine(db)
	results, err := engine.Execute(context.Background(), "nonexistent_metric", 0, 1000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestExecutorCountAggregation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	db.Ingest("up", map[string]string{"host": "a"}, 1000, 1)
	db.Ingest("up", map[string]string{"host": "b"}, 1000, 1)
	db.Ingest("up", map[string]string{"host": "c"}, 1000, 1)

	engine := NewEngine(db)
	results, err := engine.Execute(context.Background(), "count(up)", 1000, 1000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Points[0].Value != 3 {
		t.Fatalf("count: got %f, want 3", results[0].Points[0].Value)
	}
}
