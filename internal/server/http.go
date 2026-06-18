// Package server provides the HTTP API and WebSocket endpoints for Meridian.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/meridiandb/meridian/internal/anomaly"
	"github.com/meridiandb/meridian/internal/backpressure"
	"github.com/meridiandb/meridian/internal/query"
	"github.com/meridiandb/meridian/internal/storage"
)

// defaultQueryTimeout bounds how long a single /api/v1/query may run before the
// engine context is cancelled. It is configurable via SetQueryTimeout.
const defaultQueryTimeout = 30 * time.Second

// clusterProbeTimeout is the overall deadline for probing all peers in one
// /api/v1/cluster request. Peers are probed concurrently, so this bounds the whole
// fan-out rather than each peer serially.
const clusterProbeTimeout = 2 * time.Second

// HTTPServer serves the REST API, dashboard, and WebSocket endpoints.
type HTTPServer struct {
	db         *storage.TSDB
	engine     *query.Engine
	wsHub      *WebSocketHub
	mux        *http.ServeMux
	httpServer *http.Server
	startTime      time.Time
	nodeID         string
	peerAddrs      []string // HTTP addresses of cluster peers
	latency        *latencyTracker
	queryTimeout   time.Duration
	allowedOrigins []string // CORS allow-list; empty = localhost only
	// ingestStats, when set, supplies the bounded ingest queue snapshot for the
	// /metrics and /api/v1/stats handlers. The ingestion server owns the queue, so
	// the serve command wires this after constructing both.
	ingestStats func() backpressure.Stats
	// anomalyDet, when set, is the streaming anomaly detector fed by the broadcast
	// loop. It backs the /api/v1/anomalies endpoint and the anomaly metrics/stats.
	anomalyDet *anomaly.Detector
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
		nodeID:       nodeID,
		peerAddrs:    peerAddrs,
		latency:      newLatencyTracker(),
		queryTimeout: defaultQueryTimeout,
	}

	s.registerRoutes()
	return s
}

// SetQueryTimeout overrides the per-query execution deadline. A value <= 0 leaves
// the current timeout unchanged.
func (s *HTTPServer) SetQueryTimeout(d time.Duration) {
	if d > 0 {
		s.queryTimeout = d
	}
}

// SetAllowedOrigins sets the CORS allow-list. Empty (the default) permits only
// localhost origins; a single "*" entry permits all. See CORSMiddleware.
func (s *HTTPServer) SetAllowedOrigins(origins []string) {
	s.allowedOrigins = origins
}

// SetIngestStatsSource wires the bounded ingest queue snapshot into the /metrics
// and /api/v1/stats handlers so the monolith exposes write-path flow control.
func (s *HTTPServer) SetIngestStatsSource(fn func() backpressure.Stats) {
	s.ingestStats = fn
}

// SetAnomalyDetector wires the streaming anomaly detector (fed by the broadcast
// loop) so /api/v1/anomalies, /api/v1/stats, and /metrics can surface recent
// anomalies and their counters.
func (s *HTTPServer) SetAnomalyDetector(d *anomaly.Detector) {
	s.anomalyDet = d
}

// handler composes the full middleware chain in front of the mux: CORS, then a
// traversal guard that rejects "../" paths before http.ServeMux can path-clean and
// 301-redirect them.
func (s *HTTPServer) handler() http.Handler {
	return CORSMiddleware(s.allowedOrigins, GuardTraversal(s.mux))
}

// Start begins serving HTTP requests.
func (s *HTTPServer) Start(addr string) error {
	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: s.handler(),
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

// Stop gracefully shuts down the HTTP server. It is a no-op if Start was never
// called, so a startup that fails before Start does not panic on cleanup.
func (s *HTTPServer) Stop() {
	if s.httpServer == nil {
		return
	}
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
	s.mux.HandleFunc("/api/v1/anomalies", s.handleAnomalies)
	s.mux.HandleFunc("/api/v1/query_latency", s.handleQueryLatency)
	s.mux.HandleFunc("/metrics", s.handlePromMetrics)
	s.mux.HandleFunc("/ws/metrics", s.handleWSMetrics)

	// Serve dashboard static files. Hashed bundle assets are served straight from
	// the traversal-safe http.Dir file server; every other non-API path goes through
	// newStaticHandler, which rejects directory traversal and falls back to the SPA
	// shell for client-side routes.
	dashboardDir := findDashboardDir()
	if dashboardDir != "" {
		s.mux.Handle("/assets/", http.FileServer(http.Dir(dashboardDir)))
		s.mux.Handle("/", newStaticHandler(dashboardDir))
	}
}

// newStaticHandler serves the dashboard's static files, falling back to the SPA
// shell (index.html) for client-side routes. It is hardened against directory
// traversal: any request whose (already percent-decoded) path contains a ".."
// element is rejected with 400, and every file read is confined to dashboardDir via
// http.Dir, so "/../../../../etc/passwd" or its "%2e%2e%2f" encoding can never escape
// the dashboard directory.
func newStaticHandler(dashboardDir string) http.Handler {
	fileServer := http.FileServer(http.Dir(dashboardDir))
	index := filepath.Join(dashboardDir, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if containsDotDot(r.URL.Path) {
			http.Error(w, "invalid URL path", http.StatusBadRequest)
			return
		}
		// Serve an existing file from inside the dashboard dir; otherwise return the
		// SPA shell so client-side routes resolve. The ".." guard above plus http.Dir
		// keep both branches confined to dashboardDir.
		if r.URL.Path != "/" && fileExists(filepath.Join(dashboardDir, filepath.FromSlash(r.URL.Path))) {
			fileServer.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, index)
	})
}

// GuardTraversal rejects any request whose path contains a ".." element with 400,
// before it reaches http.ServeMux (which would otherwise path-clean and 301-redirect
// it). Applied at the server boundary so directory-traversal attempts get an explicit
// 4xx and never resolve against the filesystem.
func GuardTraversal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if containsDotDot(r.URL.Path) {
			http.Error(w, "invalid URL path", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// containsDotDot reports whether v contains a ".." path element. net/http
// percent-decodes r.URL.Path before handlers run, so this also catches encoded forms
// such as "%2e%2e%2f".
func containsDotDot(v string) bool {
	if !strings.Contains(v, "..") {
		return false
	}
	for _, ent := range strings.FieldsFunc(v, func(r rune) bool { return r == '/' || r == '\\' }) {
		if ent == ".." {
			return true
		}
	}
	return false
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
	// A panic anywhere in parse/plan/execute must not drop the connection; turn it
	// into a 500 so a single pathological query cannot take down the server.
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("query handler panic: %v", rec)
			writeError(w, http.StatusInternalServerError, "internal error during query execution")
		}
	}()

	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "missing query parameter 'q'")
		return
	}

	now := time.Now()
	start, err := parseTimestampParam(r.URL.Query().Get("start"), now.Add(-1*time.Hour).UnixMilli())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'start' parameter: "+err.Error())
		return
	}
	end, err := parseTimestampParam(r.URL.Query().Get("end"), now.UnixMilli())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'end' parameter: "+err.Error())
		return
	}
	if end < start {
		writeError(w, http.StatusBadRequest, "invalid range: 'end' must be greater than or equal to 'start'")
		return
	}
	// An absent step is left at 0 so the engine auto-sizes one from [start,end]
	// (~250 points). A present-but-unparseable step is a client error, not a silent
	// default. The engine caps the resulting step count internally.
	var step time.Duration
	if stepStr := r.URL.Query().Get("step"); stepStr != "" {
		d, perr := query.ParseDuration(stepStr)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "invalid 'step' parameter: "+perr.Error())
			return
		}
		step = d
	}

	// Bound execution time at the HTTP boundary; the engine honours ctx cancellation
	// per evaluation step and per storage fetch.
	ctx, cancel := context.WithTimeout(r.Context(), s.queryTimeout)
	defer cancel()

	startExec := time.Now()
	results, meta, err := s.engine.ExecuteWithMeta(ctx, q, start, end, step)
	execTime := time.Since(startExec)
	s.latency.record(execTime)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			writeError(w, http.StatusGatewayTimeout, "query exceeded the time limit")
		case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
			writeError(w, http.StatusServiceUnavailable, "query canceled")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
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
		// Resolution selection is transparent, but reported so callers can see a wide
		// span was served from a coarse rollup tier (resolution_ms>0) reading far fewer
		// points. resolution_ms is 0 when the query read raw. See ADR-011.
		"resolution_ms": meta.ResolutionMs,
		"points_read":   meta.PointsRead,
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
	out := map[string]interface{}{
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
	}
	// Write-path backpressure: the queue depth/capacity gauges and the cumulative
	// drop counter (the dashboard derives a drop rate from successive samples, like
	// the rate/total split for ingestion). See ADR-023.
	if s.ingestStats != nil {
		q := s.ingestStats()
		out["ingest_queue_depth"] = q.Depth
		out["ingest_queue_capacity"] = q.Capacity
		out["ingest_queue_high_watermark"] = q.HighWatermark
		out["dropped_samples"] = q.DroppedSamples
	}
	// Streaming anomaly detection (ADR-024): cumulative alerts raised and the number
	// of series currently firing.
	if s.anomalyDet != nil {
		out["anomalies_total"] = s.anomalyDet.Total()
		out["active_anomalies"] = s.anomalyDet.Active()
	}
	writeJSON(w, out)
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

// handleAnomalies returns the bounded recent-anomalies buffer, most-recent-first,
// so a late-joining dashboard can seed its alerts strip before live frames arrive.
func (s *HTTPServer) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, RecentAnomaliesPayload(s.anomalyDet))
}

// RecentAnomaliesPayload snapshots a detector's recent buffer into the wire shape
// shared by the monolith and the gateway: an `anomalies` list (most-recent-first)
// plus the `total`/`active` counters. A nil detector yields an empty list and zero
// counters so the endpoint is always well-formed.
func RecentAnomaliesPayload(d *anomaly.Detector) map[string]interface{} {
	if d == nil {
		return map[string]interface{}{"anomalies": []anomaly.Event{}, "total": 0, "active": 0}
	}
	events := d.Recent()
	anomaly.SortEventsRecentFirst(events)
	return map[string]interface{}{
		"anomalies": events,
		"total":     d.Total(),
		"active":    d.Active(),
	}
}

// handlePromMetrics exposes Meridian's internal stats in Prometheus text format
// (https://prometheus.io/docs/instrumenting/exposition_formats/). This lets the
// server be scraped by a Prometheus-compatible collector — useful for running
// Meridian alongside an existing metrics pipeline.
func (s *HTTPServer) handlePromMetrics(w http.ResponseWriter, r *http.Request) {
	node := s.nodeID
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// Storage-engine metrics, shared verbatim with the storage microservice.
	WriteStorageMetrics(w, s.db, node)

	// Write-path flow-control metrics from the bounded ingest queue, when wired.
	if s.ingestStats != nil {
		WriteQueueMetrics(w, node, "monolith", s.ingestStats())
	}

	// Streaming anomaly detection metrics, when wired.
	if s.anomalyDet != nil {
		WriteAnomalyMetrics(w, node, "monolith", s.anomalyDet.Total(), s.anomalyDet.Active())
	}

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
	self := map[string]interface{}{
		"id":      s.nodeID,
		"addr":    "localhost",
		"state":   "active",
		"role":    "storage",
		"series":  stats.TotalSeries,
		"samples": stats.TotalSamples,
	}

	// Single-node serve mode has no peers: report exactly the one real node rather
	// than fabricating a cluster with hardcoded zero-stat peers.
	if len(s.peerAddrs) == 0 {
		writeJSON(w, map[string]interface{}{"nodes": []map[string]interface{}{self}})
		return
	}

	// Probe peers concurrently under one overall deadline tied to the request,
	// instead of serially blocking up to 500ms per peer.
	ctx, cancel := context.WithTimeout(r.Context(), clusterProbeTimeout)
	defer cancel()

	peers := make([]map[string]interface{}, len(s.peerAddrs))
	var wg sync.WaitGroup
	for i, peer := range s.peerAddrs {
		wg.Add(1)
		go func(i int, peer string) {
			defer wg.Done()
			peers[i] = s.probePeer(ctx, peer)
		}(i, peer)
	}
	wg.Wait()

	nodes := make([]map[string]interface{}, 0, len(peers)+1)
	nodes = append(nodes, self)
	nodes = append(nodes, peers...)
	writeJSON(w, map[string]interface{}{"nodes": nodes})
}

// probePeer fetches a peer's liveness and real stats. A reachable peer reports its
// actual series/samples rather than fabricated zeros; an unreachable one is marked
// dead.
func (s *HTTPServer) probePeer(ctx context.Context, peer string) map[string]interface{} {
	node := map[string]interface{}{
		"id": peer, "addr": peer, "state": "dead", "role": "storage", "series": 0, "samples": 0,
	}
	id, ok := s.peerHealth(ctx, peer)
	if !ok {
		return node
	}
	node["state"] = "active"
	if id != "" {
		node["id"] = id
	}
	if series, samples, ok := s.peerStats(ctx, peer); ok {
		node["series"] = series
		node["samples"] = samples
	}
	return node
}

func (s *HTTPServer) peerHealth(ctx context.Context, peer string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%s/health", peer), nil)
	if err != nil {
		return "", false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", false
	}
	var h struct {
		NodeID string `json:"node_id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return "", false
	}
	return h.NodeID, h.Status == "ok"
}

func (s *HTTPServer) peerStats(ctx context.Context, peer string) (int, int64, bool) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%s/api/v1/stats", peer), nil)
	if err != nil {
		return 0, 0, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return 0, 0, false
	}
	var st struct {
		TotalSeries  int   `json:"total_series"`
		TotalSamples int64 `json:"total_samples"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return 0, 0, false
	}
	return st.TotalSeries, st.TotalSamples, true
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

// parseTimestampParam parses a millisecond Unix timestamp from a query parameter.
// An empty string yields defaultVal (the parameter was absent); a non-empty but
// unparseable string returns an error so malformed input is rejected with 400
// rather than silently falling back to a default.
func parseTimestampParam(s string, defaultVal int64) (int64, error) {
	if s == "" {
		return defaultVal, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a millisecond timestamp", s)
	}
	return v, nil
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
