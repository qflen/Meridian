// Storage service — owns TSDB storage, WAL, compression, block management.
// Exposes internal HTTP API for reads and writes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/meridiandb/meridian/internal/service"
	"github.com/meridiandb/meridian/internal/storage"
)

func main() {
	httpAddr := envOrDefault("STORAGE_HTTP_ADDR", ":8080")
	dataDir := envOrDefault("STORAGE_DATA_DIR", "./data")
	nodeID := envOrDefault("STORAGE_NODE_ID", "storage-1")

	opts := storage.TSDBOptions{
		WALDir:          dataDir + "/wal",
		BlockDir:        dataDir + "/blocks",
		BlockDuration:   2 * time.Hour,
		RetentionPeriod: 15 * 24 * time.Hour,
		FlushInterval:   1 * time.Minute,
	}

	db, err := storage.Open(dataDir, opts)
	if err != nil {
		log.Fatalf("open TSDB: %v", err)
	}

	mux := http.NewServeMux()
	s := &storageServer{db: db, nodeID: nodeID, startTime: time.Now()}
	s.registerRoutes(mux)

	httpServer := &http.Server{Addr: httpAddr, Handler: corsMiddleware(mux)}
	go func() {
		log.Printf("Storage service %s listening on %s (data: %s)", nodeID, httpAddr, dataDir)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down storage service...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)
	db.Close()
	log.Println("Storage service stopped.")
}

type storageServer struct {
	db        *storage.TSDB
	nodeID    string
	startTime time.Time
}

func (s *storageServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/internal/write", s.handleWrite)
	mux.HandleFunc("/api/internal/query", s.handleQuery)
	mux.HandleFunc("/api/internal/series", s.handleSeries)
	mux.HandleFunc("/api/internal/labels", s.handleLabels)
	mux.HandleFunc("/api/internal/label/", s.handleLabelValues)
	mux.HandleFunc("/api/internal/stats", s.handleStats)
	mux.HandleFunc("/api/internal/blocks", s.handleBlocks)
	// DELETE for specific block: /api/internal/blocks/{ulid}
}

func (s *storageServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"status":  "ok",
		"node_id": s.nodeID,
		"role":    "storage",
		"uptime":  time.Since(s.startTime).String(),
	})
}

func (s *storageServer) handleWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req service.WriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	var count int64
	for _, ts := range req.TimeSeries {
		labels := make(map[string]string, len(ts.Labels))
		for _, l := range ts.Labels {
			labels[l.Name] = l.Value
		}
		for _, sample := range ts.Samples {
			if err := s.db.Ingest(ts.Name, labels, sample.TimestampMs, sample.Value); err != nil {
				log.Printf("Ingest error: %v", err)
				continue
			}
			count++
		}
	}

	writeJSON(w, service.WriteResponse{SamplesIngested: count})
}

func (s *storageServer) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req service.QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	matchers := make([]storage.LabelMatcher, len(req.Matchers))
	for i, m := range req.Matchers {
		matchers[i] = service.MatcherToStorage(m)
	}

	ss, err := s.db.Query(r.Context(), matchers, req.Start, req.End)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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

	writeJSON(w, service.QueryResponse{Status: "success", Data: data})
}

func (s *storageServer) handleSeries(w http.ResponseWriter, r *http.Request) {
	series := s.db.Series()
	data := make([]service.SeriesInfo, len(series))
	for i, si := range series {
		data[i] = service.SeriesInfo{
			Name:        si.Name,
			Labels:      si.Labels,
			SampleCount: si.SampleCount,
			LastValue:   si.LastValue,
		}
	}
	writeJSON(w, map[string]interface{}{"data": data})
}

func (s *storageServer) handleLabels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{"data": s.db.LabelNames()})
}

func (s *storageServer) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/internal/label/"), "/")
	if len(parts) < 2 || parts[1] != "values" {
		writeError(w, http.StatusBadRequest, "expected /api/internal/label/<name>/values")
		return
	}
	writeJSON(w, map[string]interface{}{"data": s.db.LabelValues(parts[0])})
}

func (s *storageServer) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := s.db.Stats()
	ratio := s.db.CompressionRatio()
	writeJSON(w, service.StatsResponse{
		TotalSamples:     stats.TotalSamples,
		TotalSeries:      stats.TotalSeries,
		BlockCount:       stats.BlockCount,
		CompressionRatio: fmt.Sprintf("%.1f", ratio),
		StorageBytesRaw:  stats.StorageBytesRaw,
		StorageBytesDisk: stats.StorageBytesDisk,
		HeadSamples:      stats.HeadSamples,
		HeadSeries:       stats.HeadSeries,
		WALSize:          stats.WALSize,
		IngestionRate:    s.db.IngestionRate(),
		Uptime:           time.Since(s.startTime).String(),
	})
}

func (s *storageServer) handleBlocks(w http.ResponseWriter, r *http.Request) {
	// Handle DELETE /api/internal/blocks/{ulid}
	path := strings.TrimPrefix(r.URL.Path, "/api/internal/blocks")
	if r.Method == "DELETE" && len(path) > 1 {
		ulid := strings.TrimPrefix(path, "/")
		if err := s.db.DeleteBlock(ulid); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}

	// GET: list all blocks
	blocks := s.db.Blocks()
	infos := make([]service.BlockInfo, len(blocks))
	for i, b := range blocks {
		meta := b.Meta()
		infos[i] = service.BlockInfo{
			ULID:       meta.ULID,
			NodeID:     s.nodeID,
			MinTime:    meta.MinTime,
			MaxTime:    meta.MaxTime,
			NumSamples: meta.Stats.NumSamples,
			NumSeries:  meta.Stats.NumSeries,
			Level:      meta.Compaction.Level,
		}
	}
	writeJSON(w, infos)
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

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
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
