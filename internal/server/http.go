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
}

// NewHTTPServer creates a new HTTP server.
func NewHTTPServer(db *storage.TSDB, nodeID string) *HTTPServer {
	s := &HTTPServer{
		db:        db,
		engine:    query.NewEngine(db),
		wsHub:     NewWebSocketHub(),
		mux:       http.NewServeMux(),
		startTime: time.Now(),
		nodeID:    nodeID,
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
	s.mux.HandleFunc("/ws/live", s.handleWSLive)
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
		"total_samples":           stats.TotalSamples,
		"total_series":            stats.TotalSeries,
		"blocks":                  stats.BlockCount,
		"compression_ratio":       fmt.Sprintf("%.1f", ratio),
		"storage_bytes_raw":       stats.StorageBytesRaw,
		"storage_bytes_compressed": stats.StorageBytesDisk,
		"head_samples":            stats.HeadSamples,
		"head_series":             stats.HeadSeries,
		"wal_size":                stats.WALSize,
		"ingestion_rate":          s.db.IngestionRate(),
		"uptime":                  time.Since(s.startTime).String(),
	})
}

func (s *HTTPServer) handleCluster(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"nodes": []map[string]interface{}{
			{
				"id":     s.nodeID,
				"addr":   "localhost",
				"state":  "active",
				"shards": 256,
			},
		},
	})
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
