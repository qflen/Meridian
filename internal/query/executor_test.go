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

	// Ingest a monotonically increasing counter
	for i := 0; i < 60; i++ {
		ts := int64(i) * 1000 // 1-second intervals
		db.Ingest("http_requests_total", map[string]string{"method": "GET"}, ts, float64(i*10))
	}

	engine := NewEngine(db)
	results, err := engine.Execute(context.Background(), "rate(http_requests_total[1m])", 0, 60000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result series, got %d", len(results))
	}
	if len(results[0].Points) != 1 {
		t.Fatalf("expected 1 rate point, got %d", len(results[0].Points))
	}
	// 60 points, each increasing by 10, over 59 seconds = rate of ~10/sec
	rateVal := results[0].Points[0].Value
	if math.Abs(rateVal-10.0) > 0.5 {
		t.Fatalf("expected rate ~10.0, got %f", rateVal)
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
	results, err := engine.Execute(context.Background(), "sum(cpu_usage)", 0, 50000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 aggregated series, got %d", len(results))
	}
	// At t=0: 40+60=100, at t=5000: 41+61=102, etc.
	if results[0].Points[0].Value != 100 {
		t.Fatalf("first point: got %f, want 100", results[0].Points[0].Value)
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

	// Test max
	maxResults, err := engine.Execute(context.Background(), "max(metric)", 0, 2000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if maxResults[0].Points[0].Value != 30 {
		t.Fatalf("max: got %f, want 30", maxResults[0].Points[0].Value)
	}

	// Test min
	minResults, err := engine.Execute(context.Background(), "min(metric)", 0, 2000, 15*time.Second)
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
	results, err := engine.Execute(context.Background(), `sum(http_requests) by (method)`, 0, 2000, 15*time.Second)
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
	results, err := engine.Execute(context.Background(), "cpu * 100", 0, 2000, 15*time.Second)
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
	results, err := engine.Execute(context.Background(), "count(up)", 0, 2000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Points[0].Value != 3 {
		t.Fatalf("count: got %f, want 3", results[0].Points[0].Value)
	}
}
