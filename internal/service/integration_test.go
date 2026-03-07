package service_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/cluster"
	"github.com/meridiandb/meridian/internal/query"
	"github.com/meridiandb/meridian/internal/service"
	"github.com/meridiandb/meridian/internal/storage"
)

// wireRangesToStorage converts the wire [lo,hi] arc pairs to storage hash ranges, the
// same conversion cmd/storage's anti-entropy handlers do.
func wireRangesToStorage(ranges [][2]uint64) []storage.HashRange {
	out := make([]storage.HashRange, len(ranges))
	for i, p := range ranges {
		out[i] = storage.HashRange{Lo: p[0], Hi: p[1]}
	}
	return out
}

// exportToWrite renders exported series as a WriteRequest (the backfill wire shape),
// dropping the synthetic __name__ label so a backfilled series recreates the same key.
func exportToWrite(series []storage.ResultSeries) service.WriteRequest {
	wr := service.WriteRequest{}
	for _, rs := range series {
		var labels []service.Label
		for k, v := range rs.Labels {
			if k != "__name__" {
				labels = append(labels, service.Label{Name: k, Value: v})
			}
		}
		samples := make([]service.Sample, len(rs.Points))
		for i, p := range rs.Points {
			samples[i] = service.Sample{TimestampMs: p.Timestamp, Value: p.Value}
		}
		wr.TimeSeries = append(wr.TimeSeries, service.TimeSeries{Name: rs.Name, Labels: labels, Samples: samples})
	}
	return wr
}

// openTestDB opens a TSDB whose WAL and blocks live under a unique temp dir.
// DefaultTSDBOptions hardcodes ./data/{wal,blocks}, so without overriding those every
// in-process node would share one on-disk store and contaminate the others; this keeps
// each node's storage isolated, which the exact replica assertions below depend on.
func openTestDB(t *testing.T) *storage.TSDB {
	t.Helper()
	dir := t.TempDir()
	opts := storage.DefaultTSDBOptions()
	opts.WALDir = filepath.Join(dir, "wal")
	opts.BlockDir = filepath.Join(dir, "blocks")
	db, err := storage.Open(dir, opts)
	if err != nil {
		t.Fatalf("open TSDB: %v", err)
	}
	return db
}

// writeQueryResult serves a QueryRequest against db exactly as the production storage node
// does: when a rollup resolution is requested it serves the coarsest tier the node holds at
// or below it (the requested aggregate column) and reports the resolution actually served;
// otherwise it reads raw. The in-process nodes share this so the cluster tests exercise the
// real node-local tier selection (storage.QueryAtMostResolution).
func writeQueryResult(ctx context.Context, w http.ResponseWriter, db *storage.TSDB, req service.QueryRequest) {
	matchers := make([]storage.LabelMatcher, len(req.Matchers))
	for i, m := range req.Matchers {
		matchers[i] = service.MatcherToStorage(m)
	}
	var (
		ss        storage.SeriesSet
		servedRes int64
		err       error
	)
	if req.Resolution > 0 {
		ss, servedRes, err = db.QueryAtMostResolution(ctx, matchers, req.Start, req.End, req.Resolution, service.AggregateFromWire(req.Aggregate))
	} else {
		ss, err = db.Query(ctx, matchers, req.Start, req.End)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	json.NewEncoder(w).Encode(service.QueryResponse{Status: "success", Data: data, ResolutionMs: servedRes})
}

// writeResolutions advertises a node's rollup tier availability for cluster-side planning.
func writeResolutions(w http.ResponseWriter, db *storage.TSDB) {
	json.NewEncoder(w).Encode(service.ResolutionsResponse{
		Resolutions:         db.RollupResolutions(),
		IncreaseResolutions: db.RollupIncreaseResolutions(),
	})
}

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
		writeQueryResult(r.Context(), w, db, req)
	})
	mux.HandleFunc("/api/internal/resolutions", func(w http.ResponseWriter, r *http.Request) {
		writeResolutions(w, db)
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
		db := openTestDB(t)
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

// ── Replication harness ──────────────────────────────────────────────────────
//
// replNode is a toggleable in-process storage node: flipping alive to false makes
// every endpoint (including /health) return 503, so the client sees the node as dead
// without tearing down its listener or losing its on-disk state — exactly the "kill a
// storage node, then bring it back" scenario, but deterministic.

type replNode struct {
	id    string
	db    *storage.TSDB
	srv   *httptest.Server
	alive atomic.Bool
}

func (n *replNode) addr() string { return strings.TrimPrefix(n.srv.URL, "http://") }
func (n *replNode) up()          { n.alive.Store(true) }
func (n *replNode) down()        { n.alive.Store(false) }

// startReplCluster brings up count toggleable storage nodes, each cleaned up via
// t.Cleanup.
func startReplCluster(t *testing.T, count int) []*replNode {
	t.Helper()
	nodes := make([]*replNode, count)
	for i := 0; i < count; i++ {
		db := openTestDB(t)
		n := &replNode{id: fmt.Sprintf("storage-%d", i+1), db: db}
		n.alive.Store(true)
		n.srv = httptest.NewServer(replMux(n))
		t.Cleanup(func() {
			n.srv.Close()
			db.Close()
		})
		nodes[i] = n
	}
	return nodes
}

func replMux(n *replNode) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "node_id": n.id, "role": "storage"})
	})
	mux.HandleFunc("/api/internal/write", func(w http.ResponseWriter, r *http.Request) {
		var req service.WriteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var count int64
		for _, ts := range req.TimeSeries {
			labels := make(map[string]string, len(ts.Labels))
			for _, l := range ts.Labels {
				labels[l.Name] = l.Value
			}
			for _, s := range ts.Samples {
				if err := n.db.Ingest(ts.Name, labels, s.TimestampMs, s.Value); err == nil {
					count++
				}
			}
		}
		json.NewEncoder(w).Encode(service.WriteResponse{SamplesIngested: count})
	})
	mux.HandleFunc("/api/internal/backfill", func(w http.ResponseWriter, r *http.Request) {
		var req service.WriteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var samples []storage.IngestSample
		for _, ts := range req.TimeSeries {
			labels := make(map[string]string, len(ts.Labels))
			for _, l := range ts.Labels {
				labels[l.Name] = l.Value
			}
			for _, s := range ts.Samples {
				samples = append(samples, storage.IngestSample{Name: ts.Name, Labels: labels, Timestamp: s.TimestampMs, Value: s.Value})
			}
		}
		applied, err := n.db.Backfill(samples)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(service.WriteResponse{SamplesIngested: int64(applied)})
	})
	mux.HandleFunc("/api/internal/query", func(w http.ResponseWriter, r *http.Request) {
		var req service.QueryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeQueryResult(r.Context(), w, n.db, req)
	})
	mux.HandleFunc("/api/internal/resolutions", func(w http.ResponseWriter, r *http.Request) {
		writeResolutions(w, n.db)
	})
	// Anti-entropy (ADR-030): a digest endpoint and a range-export endpoint, mirroring
	// the storage node. The ring hash is injected (cluster.HashKey), exactly as
	// cmd/storage does, so a series is classified the same way it was routed.
	mux.HandleFunc("/api/internal/antientropy/digest", func(w http.ResponseWriter, r *http.Request) {
		var req service.DigestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d, err := n.db.RangeDigest(wireRangesToStorage(req.Ranges), req.Start, req.End, req.Window, cluster.HashKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(d)
	})
	mux.HandleFunc("/api/internal/antientropy/range", func(w http.ResponseWriter, r *http.Request) {
		var req service.RangeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		series, err := n.db.RangeExport(wireRangesToStorage(req.Ranges), req.Start, req.End, cluster.HashKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(exportToWrite(series))
	})
	// Rebalance GC (ADR-031): drop the series in the requested hash arcs — the data this node
	// no longer owns — exactly as cmd/storage does.
	mux.HandleFunc("/api/internal/rebalance/drop", func(w http.ResponseWriter, r *http.Request) {
		var req service.DropRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res, err := n.db.DropSeriesInRanges(wireRangesToStorage(req.Ranges), cluster.HashKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(service.DropResponse{
			SeriesDropped:   res.SeriesDropped,
			SamplesDropped:  res.SamplesDropped,
			RollupWindows:   res.RollupWindows,
			BlocksRewritten: res.BlocksRewritten,
			BlocksDeleted:   res.BlocksDeleted,
		})
	})
	// Gate: a "down" node fails every request, the way a crashed process would.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !n.alive.Load() {
			http.Error(w, "node down", http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func replAddrs(nodes []*replNode) []string {
	addrs := make([]string, len(nodes))
	for i, n := range nodes {
		addrs[i] = n.addr()
	}
	return addrs
}

func replOpts(n, w, r int) service.ReplicationOptions {
	return service.ReplicationOptions{ReplicationFactor: n, WriteQuorum: w, ReadQuorum: r, VirtualNodes: 256}
}

// pointsOnNode reads a metric's points straight from one node's local TSDB, so a test
// can assert exactly which replicas physically hold which data.
func pointsOnNode(t *testing.T, db *storage.TSDB, name string) []storage.Point {
	t.Helper()
	ss, err := db.Query(context.Background(),
		[]storage.LabelMatcher{{Name: "__name__", Value: name, Type: storage.MatchEqual}}, 0, 1<<62)
	if err != nil {
		t.Fatalf("node query: %v", err)
	}
	for _, s := range ss {
		if s.Name == name {
			return s.Points
		}
	}
	return nil
}

func writeOneSeries(name string, labels map[string]string, samples []service.Sample) service.WriteRequest {
	lbls := make([]service.Label, 0, len(labels))
	for k, v := range labels {
		lbls = append(lbls, service.Label{Name: k, Value: v})
	}
	return service.WriteRequest{TimeSeries: []service.TimeSeries{{Name: name, Labels: lbls, Samples: samples}}}
}

func nameMatcher(name string) []storage.LabelMatcher {
	return []storage.LabelMatcher{{Name: "__name__", Value: name, Type: storage.MatchEqual}}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func ramp(now int64, n int) []service.Sample {
	s := make([]service.Sample, n)
	for i := 0; i < n; i++ {
		s[i] = service.Sample{TimestampMs: now + int64(i)*1000, Value: float64(i)}
	}
	return s
}

// TestReplication_WriteLandsOnExactlyN proves a write replicates to exactly N nodes —
// not one (the old RF=1 fnv shard) and not all of them — and that the replicas the
// client computes from the ring are precisely the nodes that physically hold the data.
func TestReplication_WriteLandsOnExactlyN(t *testing.T) {
	nodes := startReplCluster(t, 5) // N=3 < 5 nodes, so "exactly N" is meaningful
	sc := service.NewReplicatedStorageClient(replAddrs(nodes), replOpts(3, 2, 2))
	sc.RefreshHealth()
	now := time.Now().UnixMilli()

	for _, name := range []string{"metric_a", "metric_b", "metric_c", "metric_d"} {
		labels := map[string]string{"host": "web-1"}
		if _, err := sc.Write(context.Background(), writeOneSeries(name, labels, ramp(now, 4))); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}

		want := sc.Replicas(name, labels)
		if len(want) != 3 {
			t.Fatalf("expected 3 replicas for %s, got %d", name, len(want))
		}
		wantSet := map[string]bool{want[0]: true, want[1]: true, want[2]: true}

		held := 0
		for _, n := range nodes {
			pts := pointsOnNode(t, n.db, name)
			onReplica := wantSet[n.addr()]
			if len(pts) > 0 {
				held++
				if !onReplica {
					t.Errorf("%s has data on non-replica %s", name, n.id)
				}
			} else if onReplica {
				t.Errorf("%s missing on replica %s", name, n.id)
			}
		}
		if held != 3 {
			t.Errorf("%s landed on %d nodes, want exactly 3", name, held)
		}
	}
}

// TestReplication_WriteAtQuorumWithReplicaDown proves a write still succeeds when one
// replica is down (W=2 of N=3) and that the surviving replicas hold the data.
func TestReplication_WriteAtQuorumWithReplicaDown(t *testing.T) {
	nodes := startReplCluster(t, 3) // N=3 == cluster: every series replicates to all three
	sc := service.NewReplicatedStorageClient(replAddrs(nodes), replOpts(3, 2, 2))
	sc.RefreshHealth()
	now := time.Now().UnixMilli()

	nodes[2].down()
	sc.RefreshHealth()

	name := "cpu_usage_percent"
	labels := map[string]string{"host": "web-1"}
	resp, err := sc.Write(context.Background(), writeOneSeries(name, labels, ramp(now, 4)))
	if err != nil {
		t.Fatalf("write at quorum with one replica down should succeed: %v", err)
	}
	if resp.SamplesIngested != 4 {
		t.Fatalf("expected 4 samples ingested, got %d", resp.SamplesIngested)
	}

	if got := len(pointsOnNode(t, nodes[0].db, name)); got != 4 {
		t.Errorf("survivor node-1 has %d points, want 4", got)
	}
	if got := len(pointsOnNode(t, nodes[1].db, name)); got != 4 {
		t.Errorf("survivor node-2 has %d points, want 4", got)
	}
	if got := len(pointsOnNode(t, nodes[2].db, name)); got != 0 {
		t.Errorf("downed node-3 should hold nothing, has %d points", got)
	}
}

// TestReplication_ReadCompleteWithReplicaDown proves a quorum read returns complete
// data even though one replica is unreachable — the node-loss-doesn't-lose-data claim.
func TestReplication_ReadCompleteWithReplicaDown(t *testing.T) {
	nodes := startReplCluster(t, 3)
	sc := service.NewReplicatedStorageClient(replAddrs(nodes), replOpts(3, 2, 2))
	sc.RefreshHealth()
	now := time.Now().UnixMilli()

	name := "http_requests_total"
	labels := map[string]string{"host": "web-1"}
	if _, err := sc.Write(context.Background(), writeOneSeries(name, labels, ramp(now, 6))); err != nil {
		t.Fatalf("write: %v", err)
	}

	nodes[1].down()
	sc.RefreshHealth()

	ss, err := sc.Query(context.Background(), nameMatcher(name), 0, 1<<62)
	if err != nil {
		t.Fatalf("read with one replica down should succeed: %v", err)
	}
	if len(ss) != 1 {
		t.Fatalf("expected 1 series, got %d", len(ss))
	}
	if len(ss[0].Points) != 6 {
		t.Errorf("expected complete 6 points despite a down replica, got %d", len(ss[0].Points))
	}
}

// TestReplication_ReadRepairConverges proves a replica that missed writes while down
// is brought up to date by a subsequent read — without that read losing or corrupting
// data — i.e. asynchronous read-repair converges.
func TestReplication_ReadRepairConverges(t *testing.T) {
	nodes := startReplCluster(t, 3)
	sc := service.NewReplicatedStorageClient(replAddrs(nodes), replOpts(3, 2, 2))
	sc.RefreshHealth()
	now := time.Now().UnixMilli()

	name := "disk_io"
	labels := map[string]string{"host": "db-1"}

	// node-3 misses the write entirely.
	nodes[2].down()
	sc.RefreshHealth()
	samples := ramp(now, 8)
	if _, err := sc.Write(context.Background(), writeOneSeries(name, labels, samples)); err != nil {
		t.Fatalf("write at quorum: %v", err)
	}
	if got := len(pointsOnNode(t, nodes[2].db, name)); got != 0 {
		t.Fatalf("down replica should be stale (0 points), has %d", got)
	}

	// node-3 returns and a read repairs it.
	nodes[2].up()
	sc.RefreshHealth()
	if _, err := sc.Query(context.Background(), nameMatcher(name), 0, 1<<62); err != nil {
		t.Fatalf("read: %v", err)
	}

	if !waitFor(t, 3*time.Second, func() bool {
		return len(pointsOnNode(t, nodes[2].db, name)) == len(samples)
	}) {
		t.Fatalf("read-repair did not converge: node-3 has %d/%d points",
			len(pointsOnNode(t, nodes[2].db, name)), len(samples))
	}

	// The repaired points match the originals exactly.
	got := pointsOnNode(t, nodes[2].db, name)
	for i, p := range got {
		if p.Timestamp != samples[i].TimestampMs || p.Value != samples[i].Value {
			t.Errorf("repaired point %d = (%d,%v), want (%d,%v)", i, p.Timestamp, p.Value, samples[i].TimestampMs, samples[i].Value)
		}
	}
}

// TestReplication_ReadYourWrites proves W+R>N gives read-your-writes even as the live
// membership shifts between the write and the read: the write set {node-2,node-3} and
// the read replica set {node-1,node-3} always overlap, so the read observes the write.
func TestReplication_ReadYourWrites(t *testing.T) {
	nodes := startReplCluster(t, 3)
	sc := service.NewReplicatedStorageClient(replAddrs(nodes), replOpts(3, 2, 2))
	sc.RefreshHealth()
	now := time.Now().UnixMilli()

	name := "mem_used"
	labels := map[string]string{"host": "web-9"}

	// Write while node-1 is down → lands on node-2 and node-3.
	nodes[0].down()
	sc.RefreshHealth()
	samples := ramp(now, 5)
	if _, err := sc.Write(context.Background(), writeOneSeries(name, labels, samples)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Now node-1 returns and node-2 dies → read quorum is {node-1, node-3}.
	nodes[0].up()
	nodes[1].down()
	sc.RefreshHealth()

	ss, err := sc.Query(context.Background(), nameMatcher(name), 0, 1<<62)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ss) != 1 || len(ss[0].Points) != len(samples) {
		t.Fatalf("read-your-writes violated: got %d series with %v points, want 1 series of %d",
			len(ss), seriesPointLen(ss), len(samples))
	}

	// Let the read-repair of node-1 finish before teardown.
	waitFor(t, 3*time.Second, func() bool {
		return len(pointsOnNode(t, nodes[0].db, name)) == len(samples)
	})
}

func seriesPointLen(ss storage.SeriesSet) int {
	if len(ss) == 0 {
		return 0
	}
	return len(ss[0].Points)
}

// TestReplication_QuorumFailureErrors proves that when too few replicas are live, both
// writes and reads return a clear quorum error instead of silently succeeding with
// partial data.
func TestReplication_QuorumFailureErrors(t *testing.T) {
	nodes := startReplCluster(t, 3)
	sc := service.NewReplicatedStorageClient(replAddrs(nodes), replOpts(3, 2, 2))
	sc.RefreshHealth()
	now := time.Now().UnixMilli()

	// Two of three down → only one live replica, below W=2 and R=2.
	nodes[1].down()
	nodes[2].down()
	sc.RefreshHealth()

	name := "net_rx"
	labels := map[string]string{"host": "edge-1"}
	if _, err := sc.Write(context.Background(), writeOneSeries(name, labels, ramp(now, 3))); err == nil {
		t.Error("write below write quorum must return an error, not silent partial success")
	} else if !strings.Contains(err.Error(), "quorum") {
		t.Errorf("write error should mention quorum, got %q", err)
	}

	if _, err := sc.Query(context.Background(), nameMatcher(name), 0, 1<<62); err == nil {
		t.Error("read below read quorum must return an error, not silent partial data")
	} else if !strings.Contains(err.Error(), "quorum") {
		t.Errorf("read error should mention quorum, got %q", err)
	}

	// With a second node restored, quorum is available again and the write succeeds.
	nodes[1].up()
	sc.RefreshHealth()
	if _, err := sc.Write(context.Background(), writeOneSeries(name, labels, ramp(now, 3))); err != nil {
		t.Errorf("write should succeed once two replicas are live again: %v", err)
	}
}
