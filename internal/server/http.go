// Package server provides the HTTP API and WebSocket endpoints for Meridian.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/meridiandb/meridian/internal/query"
	"github.com/meridiandb/meridian/internal/storage"
)

// HTTPServer serves the REST API, dashboard, and WebSocket endpoints.
type HTTPServer struct {
	db         *storage.TSDB
	engine     *query.Engine
	wsHub      *WebSocketHub
	mux        *http.ServeMux
	httpServer *http.Server
	startTime  time.Time
	nodeID     string
	peerAddrs  []string // HTTP addresses of cluster peers
	latency    *latencyTracker
}

// latencyTracker records query execution latency into histogram buckets.
type latencyTracker struct {
	buckets []latencyBucket
}

type latencyBucket struct {
	LE    string `json:"le"`
	Count int64  `json:"count"`
}

func newLatencyTracker() *latencyTracker {
	return &latencyTracker{
		buckets: []latencyBucket{
			{LE: "1ms"}, {LE: "5ms"}, {LE: "10ms"}, {LE: "25ms"},
			{LE: "50ms"}, {LE: "100ms"}, {LE: "250ms"}, {LE: "500ms"}, {LE: "1s"},
		},
	}
}

func (lt *latencyTracker) record(d time.Duration) {
	ms := d.Milliseconds()
	thresholds := []int64{1, 5, 10, 25, 50, 100, 250, 500, 1000}
	for i, t := range thresholds {
		if ms <= t {
			lt.buckets[i].Count++
			return
		}
	}
	lt.buckets[len(lt.buckets)-1].Count++
}

// NewHTTPServer creates a new HTTP server.
func NewHTTPServer(db *storage.TSDB, nodeID string, peerAddrs []string) *HTTPServer {
	s := &HTTPServer{
		db:        db,
		engine:    query.NewEngine(db),
		wsHub:     NewWebSocketHub(),
		mux:       http.NewServeMux(),
		startTime: time.Now(),
		nodeID:    nodeID,
		peerAddrs: peerAddrs,
		latency:   newLatencyTracker(),
	}

	s.registerRoutes()
	return s
}

// Start begins serving HTTP requests.
func (s *HTTPServer) Start(addr string) error {
	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: s.corsMiddleware(s.mux),
	}
	go s.wsHub.Run()
	log.Printf("HTTP server listening on %s", addr)
	go func() {
		if err := s.httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()
	return nil
}

// Stop gracefully shuts down the HTTP server.
func (s *HTTPServer) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.httpServer.Shutdown(ctx)
}

// Hub returns the WebSocket hub for broadcasting messages.
func (s *HTTPServer) Hub() *WebSocketHub {
	return s.wsHub
}

func (s *HTTPServer) registerRoutes() {
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/api/v1/query", s.handleQuery)
	s.mux.HandleFunc("/api/v1/series", s.handleSeries)
	s.mux.HandleFunc("/api/v1/labels", s.handleLabels)
	s.mux.HandleFunc("/api/v1/label/", s.handleLabelValues)
	s.mux.HandleFunc("/api/v1/stats", s.handleStats)
	s.mux.HandleFunc("/api/v1/cluster", s.handleCluster)
	s.mux.HandleFunc("/api/v1/blocks", s.handleBlocks)
	s.mux.HandleFunc("/api/v1/query_latency", s.handleQueryLatency)
	s.mux.HandleFunc("/metrics", s.handlePromMetrics)
	s.mux.HandleFunc("/ws/metrics", s.handleWSMetrics)

	// Serve dashboard static files
	dashboardDir := findDashboardDir()
	if dashboardDir != "" {
		fs := http.FileServer(http.Dir(dashboardDir))
		s.mux.Handle("/assets/", fs)
		s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" || !fileExists(filepath.Join(dashboardDir, r.URL.Path)) {
				http.ServeFile(w, r, filepath.Join(dashboardDir, "index.html"))
				return
			}
			fs.ServeHTTP(w, r)
		})
	}
}

func (s *HTTPServer) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *HTTPServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"status":  "ok",
		"version": "0.1.0",
		"uptime":  time.Since(s.startTime).String(),
		"node_id": s.nodeID,
	})
}

func (s *HTTPServer) handleQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "missing query parameter 'q'")
		return
	}

	start := parseTimestamp(r.URL.Query().Get("start"), time.Now().Add(-1*time.Hour).UnixMilli())
	end := parseTimestamp(r.URL.Query().Get("end"), time.Now().UnixMilli())
	stepStr := r.URL.Query().Get("step")
	step := 15 * time.Second
	if stepStr != "" {
		if d, err := query.ParseDuration(stepStr); err == nil {
			step = d
		}
	}

	startExec := time.Now()
	results, err := s.engine.Execute(r.Context(), q, start, end, step)
	execTime := time.Since(startExec)
	s.latency.record(execTime)

	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	data := make([]map[string]interface{}, len(results))
	for i, rs := range results {
		points := make([][]interface{}, len(rs.Points))
		for j, p := range rs.Points {
			points[j] = []interface{}{p.Timestamp, p.Value}
		}
		data[i] = map[string]interface{}{
			"name":   rs.Name,
			"labels": rs.Labels,
			"values": points,
		}
	}

	writeJSON(w, map[string]interface{}{
		"status":    "success",
		"exec_time": execTime.String(),
		"data": map[string]interface{}{
			"resultType": "matrix",
			"result":     data,
		},
	})
}

func (s *HTTPServer) handleSeries(w http.ResponseWriter, r *http.Request) {
	series := s.db.Series()
	data := make([]map[string]interface{}, len(series))
	for i, si := range series {
		data[i] = map[string]interface{}{
			"name":          si.Name,
			"labels":        si.Labels,
			"samples_count": si.SampleCount,
		}
	}
	writeJSON(w, map[string]interface{}{
		"data": data,
	})
}

func (s *HTTPServer) handleLabels(w http.ResponseWriter, r *http.Request) {
	names := s.db.LabelNames()
	writeJSON(w, map[string]interface{}{
		"data": names,
	})
}

func (s *HTTPServer) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	// Path: /api/v1/label/<name>/values
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/label/"), "/")
	if len(parts) < 2 || parts[1] != "values" {
		writeError(w, http.StatusBadRequest, "expected /api/v1/label/<name>/values")
		return
	}
	name := parts[0]
	values := s.db.LabelValues(name)
	writeJSON(w, map[string]interface{}{
		"data": values,
	})
}

func (s *HTTPServer) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := s.db.Stats()
	ratio := s.db.CompressionRatio()
	writeJSON(w, map[string]interface{}{
		"total_samples":            stats.TotalSamples,
		"total_series":             stats.TotalSeries,
		"blocks":                   stats.BlockCount,
		"compression_ratio":        fmt.Sprintf("%.1f", ratio),
		"storage_bytes_raw":        stats.StorageBytesRaw,
		"storage_bytes_compressed": stats.ChunkBytes,
		"storage_bytes_disk":       stats.StorageBytesDisk,
		"head_samples":             stats.HeadSamples,
		"head_series":              stats.HeadSeries,
		"wal_size":                 stats.WALSize,
		"ingestion_rate":           s.db.IngestionRate(),
		"uptime":                   time.Since(s.startTime).String(),
	})
}

func (s *HTTPServer) handleBlocks(w http.ResponseWriter, r *http.Request) {
	blocks := s.db.Blocks()
	type blockInfo struct {
		ULID       string `json:"ulid"`
		NodeID     string `json:"node_id"`
		MinTime    int64  `json:"min_time"`
		MaxTime    int64  `json:"max_time"`
		NumSamples int64  `json:"num_samples"`
		NumSeries  int    `json:"num_series"`
		Level      int    `json:"level"`
	}
	infos := make([]blockInfo, len(blocks))
	for i, b := range blocks {
		meta := b.Meta()
		infos[i] = blockInfo{
			ULID:       meta.ULID,
			NodeID:     s.nodeID,
			MinTime:    meta.MinTime,
			MaxTime:    meta.MaxTime,
			NumSamples: meta.Stats.NumSamples,
			NumSeries:  meta.Stats.NumSeries,
			Level:      meta.Compaction.Level,
		}
	}
	writeJSON(w, map[string]interface{}{"blocks": infos})
}

func (s *HTTPServer) handleQueryLatency(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.latency.buckets)
}

// handlePromMetrics exposes Meridian's internal stats in Prometheus text format
// (https://prometheus.io/docs/instrumenting/exposition_formats/). This lets the
// server be scraped by a Prometheus-compatible collector — useful for running
// Meridian alongside an existing metrics pipeline.
func (s *HTTPServer) handlePromMetrics(w http.ResponseWriter, r *http.Request) {
	stats := s.db.Stats()
	ratio := s.db.CompressionRatio()
	node := s.nodeID
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	fmt.Fprintf(w, "# HELP meridian_samples_ingested_total Total samples ingested since startup.\n")
	fmt.Fprintf(w, "# TYPE meridian_samples_ingested_total counter\n")
	fmt.Fprintf(w, "meridian_samples_ingested_total{node=%q} %d\n", node, s.db.IngestionRate())

	fmt.Fprintf(w, "# HELP meridian_head_samples Samples currently resident in the in-memory head block.\n")
	fmt.Fprintf(w, "# TYPE meridian_head_samples gauge\n")
	fmt.Fprintf(w, "meridian_head_samples{node=%q} %d\n", node, stats.HeadSamples)

	fmt.Fprintf(w, "# HELP meridian_active_series Distinct (name,labels) tuples currently tracked.\n")
	fmt.Fprintf(w, "# TYPE meridian_active_series gauge\n")
	fmt.Fprintf(w, "meridian_active_series{node=%q} %d\n", node, stats.TotalSeries)

	fmt.Fprintf(w, "# HELP meridian_blocks Number of flushed on-disk blocks.\n")
	fmt.Fprintf(w, "# TYPE meridian_blocks gauge\n")
	fmt.Fprintf(w, "meridian_blocks{node=%q} %d\n", node, stats.BlockCount)

	fmt.Fprintf(w, "# HELP meridian_storage_bytes Storage footprint by layer.\n")
	fmt.Fprintf(w, "# TYPE meridian_storage_bytes gauge\n")
	fmt.Fprintf(w, "meridian_storage_bytes{node=%q,layer=\"raw\"} %d\n", node, stats.StorageBytesRaw)
	fmt.Fprintf(w, "meridian_storage_bytes{node=%q,layer=\"compressed\"} %d\n", node, stats.ChunkBytes)
	fmt.Fprintf(w, "meridian_storage_bytes{node=%q,layer=\"disk\"} %d\n", node, stats.StorageBytesDisk)
	fmt.Fprintf(w, "meridian_storage_bytes{node=%q,layer=\"wal\"} %d\n", node, stats.WALSize)

	fmt.Fprintf(w, "# HELP meridian_compression_ratio Raw-to-compressed size ratio for Gorilla-encoded chunks.\n")
	fmt.Fprintf(w, "# TYPE meridian_compression_ratio gauge\n")
	fmt.Fprintf(w, "meridian_compression_ratio{node=%q} %.3f\n", node, ratio)

	fmt.Fprintf(w, "# HELP meridian_query_latency_seconds Query executor latency histogram.\n")
	fmt.Fprintf(w, "# TYPE meridian_query_latency_seconds histogram\n")
	var cumulative int64
	var sumSeconds float64
	for _, b := range s.latency.buckets {
		cumulative += b.Count
		le, secs := promBucketUpperBound(b.LE)
		fmt.Fprintf(w, "meridian_query_latency_seconds_bucket{node=%q,le=%q} %d\n", node, le, cumulative)
		sumSeconds += float64(b.Count) * secs
	}
	fmt.Fprintf(w, "meridian_query_latency_seconds_bucket{node=%q,le=\"+Inf\"} %d\n", node, cumulative)
	fmt.Fprintf(w, "meridian_query_latency_seconds_sum{node=%q} %f\n", node, sumSeconds)
	fmt.Fprintf(w, "meridian_query_latency_seconds_count{node=%q} %d\n", node, cumulative)

	fmt.Fprintf(w, "# HELP meridian_ws_clients Connected dashboard WebSocket clients.\n")
	fmt.Fprintf(w, "# TYPE meridian_ws_clients gauge\n")
	fmt.Fprintf(w, "meridian_ws_clients{node=%q} %d\n", node, s.wsHub.ClientCount())

	fmt.Fprintf(w, "# HELP meridian_uptime_seconds Seconds since this node started.\n")
	fmt.Fprintf(w, "# TYPE meridian_uptime_seconds counter\n")
	fmt.Fprintf(w, "meridian_uptime_seconds{node=%q} %d\n", node, int64(time.Since(s.startTime).Seconds()))
}

// promBucketUpperBound converts the internal bucket label (e.g. "5ms", "1s") into
// a Prometheus `le` value in seconds (both the string label and numeric bound used
// when computing the histogram sum).
func promBucketUpperBound(label string) (string, float64) {
	d, err := time.ParseDuration(label)
	if err != nil {
		return label, 0
	}
	secs := d.Seconds()
	return strconv.FormatFloat(secs, 'f', -1, 64), secs
}

func (s *HTTPServer) handleCluster(w http.ResponseWriter, r *http.Request) {
	stats := s.db.Stats()
	nodes := []map[string]interface{}{
		{
			"id":      s.nodeID,
			"addr":    "localhost",
			"state":   "active",
			"role":    "storage",
			"series":  stats.TotalSeries,
			"samples": stats.TotalSamples,
		},
	}

	// Probe configured peers for cluster-wide view
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for _, peer := range s.peerAddrs {
		resp, err := client.Get(fmt.Sprintf("http://%s/health", peer))
		if err != nil {
			nodes = append(nodes, map[string]interface{}{
				"id": peer, "addr": peer, "state": "dead", "role": "storage", "series": 0, "samples": 0,
			})
			continue
		}
		var health struct {
			NodeID string `json:"node_id"`
			Status string `json:"status"`
		}
		json.NewDecoder(resp.Body).Decode(&health)
		resp.Body.Close()

		peerState := "dead"
		if health.Status == "ok" {
			peerState = "active"
		}
		id := health.NodeID
		if id == "" {
			id = peer
		}
		nodes = append(nodes, map[string]interface{}{
			"id": id, "addr": peer, "state": peerState, "role": "storage", "series": 0, "samples": 0,
		})
	}

	writeJSON(w, map[string]interface{}{"nodes": nodes})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{
		"status": "error",
		"error":  msg,
	})
}

func parseTimestamp(s string, defaultVal int64) int64 {
	if s == "" {
		return defaultVal
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return defaultVal
	}
	return v
}

func findDashboardDir() string {
	candidates := []string{
		"dashboard/dist",
		"../dashboard/dist",
		"../../dashboard/dist",
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
