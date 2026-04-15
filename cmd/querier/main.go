// Querier service — parses PromQL queries, fans out to storage nodes, aggregates results.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/meridiandb/meridian/internal/query"
	"github.com/meridiandb/meridian/internal/service"
)

func main() {
	httpAddr := envOrDefault("QUERIER_HTTP_ADDR", ":8080")
	storageAddrs := strings.Split(envOrDefault("STORAGE_ADDRS", "localhost:8081"), ",")
	nodeID := envOrDefault("QUERIER_NODE_ID", "querier-1")

	sc := service.NewStorageClient(storageAddrs)
	engine := query.NewEngine(sc) // StorageClient implements query.DataSource

	srv := &querierServer{
		nodeID:    nodeID,
		engine:    engine,
		storage:   sc,
		latency:   service.NewLatencyTracker(),
		startTime: time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/api/internal/query", srv.handleQuery)
	mux.HandleFunc("/api/internal/series", srv.handleSeries)
	mux.HandleFunc("/api/internal/labels", srv.handleLabels)
	mux.HandleFunc("/api/internal/label/", srv.handleLabelValues)
	mux.HandleFunc("/api/internal/latency", srv.handleLatency)

	httpServer := &http.Server{Addr: httpAddr, Handler: corsMiddleware(mux)}
	go func() {
		log.Printf("Querier %s listening on %s → storage %v", nodeID, httpAddr, storageAddrs)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down querier...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)
	log.Println("Querier stopped.")
}

type querierServer struct {
	nodeID    string
	engine    *query.Engine
	storage   *service.StorageClient
	latency   *service.LatencyTracker
	startTime time.Time
}

func (s *querierServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"status":  "ok",
		"node_id": s.nodeID,
		"role":    "querier",
		"uptime":  time.Since(s.startTime).String(),
	})
}

func (s *querierServer) handleQuery(w http.ResponseWriter, r *http.Request) {
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
	s.latency.Record(execTime)

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

func (s *querierServer) handleSeries(w http.ResponseWriter, r *http.Request) {
	series, err := s.storage.FetchSeries(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"data": series})
}

func (s *querierServer) handleLabels(w http.ResponseWriter, r *http.Request) {
	labels, err := s.storage.FetchLabels(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"data": labels})
}

func (s *querierServer) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/internal/label/"), "/")
	if len(parts) < 2 || parts[1] != "values" {
		writeError(w, http.StatusBadRequest, "expected /api/internal/label/<name>/values")
		return
	}
	values, err := s.storage.FetchLabelValues(r.Context(), parts[0])
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"data": values})
}

func (s *querierServer) handleLatency(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.latency.Buckets())
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": msg})
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

func corsMiddleware(next http.Handler) http.Handler {
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

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func init() {
	_ = fmt.Sprintf
}
