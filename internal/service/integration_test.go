package service_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/query"
	"github.com/meridiandb/meridian/internal/service"
	"github.com/meridiandb/meridian/internal/storage"
)

// startStorageNode starts a minimal storage HTTP server for testing.
func startStorageNode(t *testing.T, db *storage.TSDB, nodeID string) (string, func()) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "node_id": nodeID, "role": "storage"})
	})
	mux.HandleFunc("/api/internal/write", func(w http.ResponseWriter, r *http.Request) {
		var req service.WriteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var count int64
		for _, ts := range req.TimeSeries {
			labels := make(map[string]string, len(ts.Labels))
			for _, l := range ts.Labels {
				labels[l.Name] = l.Value
			}
			for _, s := range ts.Samples {
				db.Ingest(ts.Name, labels, s.TimestampMs, s.Value)
				count++
			}
		}
		json.NewEncoder(w).Encode(service.WriteResponse{SamplesIngested: count})
	})
	mux.HandleFunc("/api/internal/query", func(w http.ResponseWriter, r *http.Request) {
		var req service.QueryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		matchers := make([]storage.LabelMatcher, len(req.Matchers))
		for i, m := range req.Matchers {
			matchers[i] = service.MatcherToStorage(m)
		}
		ss, err := db.Query(r.Context(), matchers, req.Start, req.End)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		data := make([]service.SeriesResult, len(ss))
		for i, rs := range ss {
			points := make([]service.PointJSON, len(rs.Points))
			for j, p := range rs.Points {
				points[j] = service.PointJSON{Timestamp: p.Timestamp, Value: p.Value}
			}
			data[i] = service.SeriesResult{Name: rs.Name, Labels: rs.Labels, Points: points}
		}
		json.NewEncoder(w).Encode(service.QueryResponse{Status: "success", Data: data})
	})
	mux.HandleFunc("/api/internal/series", func(w http.ResponseWriter, r *http.Request) {
		series := db.Series()
		data := make([]service.SeriesInfo, len(series))
		for i, si := range series {
			data[i] = service.SeriesInfo{Name: si.Name, Labels: si.Labels, SampleCount: si.SampleCount}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"data": data})
	})
	mux.HandleFunc("/api/internal/labels", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"data": db.LabelNames()})
	})
	mux.HandleFunc("/api/internal/stats", func(w http.ResponseWriter, r *http.Request) {
		stats := db.Stats()
		json.NewEncoder(w).Encode(service.StatsResponse{
			TotalSamples: stats.TotalSamples,
			TotalSeries:  stats.TotalSeries,
			BlockCount:   stats.BlockCount,
		})
	})
	mux.HandleFunc("/api/internal/blocks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]service.BlockInfo{})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)

	return ln.Addr().String(), func() {
		srv.Close()
		db.Close()
	}
}

func TestIntegration_WriteQueryPipeline(t *testing.T) {
	// Create 3 storage nodes
	var addrs []string
	var closers []func()

	for i := 0; i < 3; i++ {
		dir := t.TempDir()
		db, err := storage.Open(dir, storage.DefaultTSDBOptions())
		if err != nil {
			t.Fatalf("open TSDB %d: %v", i, err)
		}
		addr, closer := startStorageNode(t, db, fmt.Sprintf("storage-%d", i+1))
		addrs = append(addrs, addr)
		closers = append(closers, closer)
	}
	defer func() {
		for _, fn := range closers {
			fn()
		}
	}()

	sc := service.NewStorageClient(addrs)

	// 1. Write metrics through the client (simulates what ingestor does)
	now := time.Now().UnixMilli()
	writeReq := service.WriteRequest{
		TimeSeries: []service.TimeSeries{
			{
				Name:   "cpu_usage_percent",
				Labels: []service.Label{{Name: "host", Value: "web-1"}, {Name: "env", Value: "prod"}},
				Samples: []service.Sample{
					{TimestampMs: now - 30000, Value: 45.0},
					{TimestampMs: now - 20000, Value: 52.0},
					{TimestampMs: now - 10000, Value: 48.0},
					{TimestampMs: now, Value: 50.0},
				},
			},
			{
				Name:   "cpu_usage_percent",
				Labels: []service.Label{{Name: "host", Value: "web-2"}, {Name: "env", Value: "prod"}},
				Samples: []service.Sample{
					{TimestampMs: now - 30000, Value: 30.0},
					{TimestampMs: now - 20000, Value: 35.0},
					{TimestampMs: now - 10000, Value: 32.0},
					{TimestampMs: now, Value: 33.0},
				},
			},
			{
				Name:   "http_requests_total",
				Labels: []service.Label{{Name: "host", Value: "web-1"}, {Name: "method", Value: "GET"}},
				Samples: []service.Sample{
					{TimestampMs: now - 30000, Value: 100.0},
					{TimestampMs: now - 20000, Value: 150.0},
					{TimestampMs: now - 10000, Value: 200.0},
					{TimestampMs: now, Value: 250.0},
				},
			},
		},
	}

	ctx := context.Background()
	resp, err := sc.Write(ctx, writeReq)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if resp.SamplesIngested == 0 {
		t.Fatal("Expected some samples to be ingested")
	}
	t.Logf("Wrote %d samples across %d storage nodes", resp.SamplesIngested, len(addrs))

	// 2. Query through the StorageClient (simulates what querier does)
	matchers := []storage.LabelMatcher{
		{Name: "__name__", Value: "cpu_usage_percent", Type: storage.MatchEqual},
	}
	ss, err := sc.Query(ctx, matchers, now-60000, now+1000)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if len(ss) == 0 {
		t.Fatal("Expected query to return series")
	}
	t.Logf("Query returned %d series", len(ss))

	// Verify we got data for cpu_usage_percent
	for _, s := range ss {
		if s.Name != "cpu_usage_percent" {
			t.Errorf("Expected cpu_usage_percent, got %s", s.Name)
		}
		if len(s.Points) == 0 {
			t.Error("Expected points in series")
		}
	}

	// 3. Query through the Engine (simulates full PromQL pipeline)
	engine := query.NewEngine(sc) // StorageClient implements DataSource
	results, err := engine.Execute(ctx, "cpu_usage_percent", now-60000, now+1000, 15*time.Second)
	if err != nil {
		t.Fatalf("Engine.Execute failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Expected engine to return results")
	}
	t.Logf("Engine returned %d result series", len(results))

	// 4. Test aggregation query
	results, err = engine.Execute(ctx, "avg(cpu_usage_percent)", now-60000, now+1000, 15*time.Second)
	if err != nil {
		t.Fatalf("Aggregate query failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Expected aggregate to return results")
	}
	t.Logf("Aggregate returned %d results, values: %v", len(results), results[0].Points)

	// 5. Test rate() function
	results, err = engine.Execute(ctx, "rate(http_requests_total[5m])", now-60000, now+1000, 15*time.Second)
	if err != nil {
		t.Fatalf("Rate query failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Expected rate() to return results")
	}
	// Rate should produce per-second values
	for _, r := range results {
		for _, p := range r.Points {
			if p.Value < 0 {
				t.Errorf("rate() produced negative value: %f", p.Value)
			}
			// For a counter going from 100→250 over 30s, rate should be ~5/s
			if p.Value > 100 {
				t.Errorf("rate() value too high: %f (expected ~5/s)", p.Value)
			}
		}
	}
	t.Logf("rate() returned %d series, value: %v", len(results), results[0].Points)

	// 6. Test FetchStats
	stats, err := sc.FetchStats(ctx)
	if err != nil {
		t.Fatalf("FetchStats failed: %v", err)
	}
	if stats.TotalSamples == 0 {
		t.Error("Expected total samples > 0")
	}
	t.Logf("Aggregated stats: %d samples, %d series", stats.TotalSamples, stats.TotalSeries)

	// 7. Test FetchSeries
	series, err := sc.FetchSeries(ctx)
	if err != nil {
		t.Fatalf("FetchSeries failed: %v", err)
	}
	if len(series) == 0 {
		t.Error("Expected series from FetchSeries")
	}
	t.Logf("FetchSeries returned %d series", len(series))

	// 8. Test FetchLabels
	labels, err := sc.FetchLabels(ctx)
	if err != nil {
		t.Fatalf("FetchLabels failed: %v", err)
	}
	if len(labels) == 0 {
		t.Error("Expected labels from FetchLabels")
	}
	t.Logf("FetchLabels returned: %v", labels)

	// 9. Verify health checks
	for _, addr := range addrs {
		id, ok := service.HealthCheck(addr)
		if !ok {
			t.Errorf("Health check failed for %s", addr)
		}
		if !strings.HasPrefix(id, "storage-") {
			t.Errorf("Expected storage- prefix, got %s", id)
		}
	}
}

func TestIntegration_HashSharding(t *testing.T) {
	// Verify that writes are distributed across storage nodes
	var addrs []string
	var closers []func()
	dbs := make([]*storage.TSDB, 3)

	for i := 0; i < 3; i++ {
		dir := t.TempDir()
		db, err := storage.Open(dir, storage.DefaultTSDBOptions())
		if err != nil {
			t.Fatalf("open TSDB %d: %v", i, err)
		}
		dbs[i] = db
		addr, closer := startStorageNode(t, db, fmt.Sprintf("storage-%d", i+1))
		addrs = append(addrs, addr)
		closers = append(closers, closer)
	}
	defer func() {
		for _, fn := range closers {
			fn()
		}
	}()

	sc := service.NewStorageClient(addrs)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Write 10 different metrics
	var series []service.TimeSeries
	for i := 0; i < 10; i++ {
		series = append(series, service.TimeSeries{
			Name:    fmt.Sprintf("metric_%d", i),
			Labels:  []service.Label{{Name: "host", Value: "web-1"}},
			Samples: []service.Sample{{TimestampMs: now, Value: float64(i) * 10}},
		})
	}

	_, err := sc.Write(ctx, service.WriteRequest{TimeSeries: series})
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Check that data is distributed
	nodesWithData := 0
	for i, db := range dbs {
		stats := db.Stats()
		t.Logf("Storage node %d: %d samples, %d series", i+1, stats.TotalSamples, stats.TotalSeries)
		if stats.TotalSamples > 0 {
			nodesWithData++
		}
	}

	// With 10 metrics and FNV hashing, we should hit at least 2 nodes
	if nodesWithData < 2 {
		t.Errorf("Expected data on at least 2 nodes, got %d", nodesWithData)
	}
	t.Logf("Data distributed across %d/%d nodes", nodesWithData, len(dbs))
}
