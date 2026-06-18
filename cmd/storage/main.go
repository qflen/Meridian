// Storage service — owns TSDB storage, WAL, compression, block management.
// Exposes internal HTTP API for reads and writes.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/meridiandb/meridian/internal/config"
	"github.com/meridiandb/meridian/internal/retention"
	"github.com/meridiandb/meridian/internal/server"
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

	// Bound the accept queue: a worker pool drains writes into the local TSDB, so a
	// node whose WAL fsync is the bottleneck backpressures (then sheds → 429) and
	// pushes flow control upstream to the ingestor/quorum layer. See ADR-023.
	pool := service.NewWritePool(localIngest{db: db}, service.PoolOptions{
		Capacity:      envInt("STORAGE_QUEUE_CAPACITY", 50000),
		HighWatermark: envInt("STORAGE_QUEUE_HIGH_WATERMARK", 40000),
		BlockDeadline: envDuration("STORAGE_BLOCK_DEADLINE", 250*time.Millisecond),
		Workers:       envInt("STORAGE_MAX_CONCURRENT_WRITES", 8),
	})
	defer pool.Close()

	// Run the downsampling cascade locally: rollups are generated where the raw data
	// lives (ADR-011). Cluster-wide per-resolution retention is enforced by the
	// compactor, which now sees rollup blocks tagged by resolution.
	if envBool("STORAGE_DOWNSAMPLING_ENABLED", true) {
		dc := config.DefaultConfig().Downsampling
		ds := retention.NewDownsampler(db, dc.DownsampleRules(), envDuration("STORAGE_DOWNSAMPLE_INTERVAL", dc.Interval.Std()))
		ds.Start()
		defer ds.Stop()
		log.Printf("Storage service %s: downsampling cascade enabled", nodeID)
	}

	mux := http.NewServeMux()
	s := &storageServer{db: db, nodeID: nodeID, pool: pool, startTime: time.Now()}
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
	pool      *service.WritePool
	startTime time.Time
}

// localIngest adapts the local TSDB to service.Writer so it can sit behind a
// bounded WritePool. A stalled WAL fsync backpressures the pool and, past the
// deadline, sheds — surfaced to the caller as 429 so the ingestor's quorum write
// observes the overload and backpressure propagates upstream.
type localIngest struct {
	db *storage.TSDB
}

func (l localIngest) Write(_ context.Context, req service.WriteRequest) (*service.WriteResponse, error) {
	var count int64
	for _, ts := range req.TimeSeries {
		labels := make(map[string]string, len(ts.Labels))
		for _, lb := range ts.Labels {
			labels[lb.Name] = lb.Value
		}
		for _, sample := range ts.Samples {
			if err := l.db.Ingest(ts.Name, labels, sample.TimestampMs, sample.Value); err != nil {
				log.Printf("Ingest error: %v", err)
				continue
			}
			count++
		}
	}
	return &service.WriteResponse{SamplesIngested: count}, nil
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
	mux.HandleFunc("/metrics", s.handleMetrics)
	// DELETE for specific block: /api/internal/blocks/{ulid}
}

func (s *storageServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	server.WriteStorageMetrics(w, s.db, s.nodeID)
	server.WriteRollupMetrics(w, s.db, s.nodeID)
	server.WriteServiceMetrics(w, s.nodeID, "storage", time.Since(s.startTime))
	if s.pool != nil {
		server.WriteQueueMetrics(w, s.nodeID, "storage", s.pool.Stats())
	}
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

	// Submit through the bounded accept queue: a stalled WAL fsync backpressures and,
	// past the deadline, sheds with 429 so the caller's quorum write sees the overload.
	resp, _, err := s.pool.Submit(r.Context(), req)
	if errors.Is(err, service.ErrShed) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "storage overloaded, retry after backing off")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, resp)
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
			LastTS:      si.LastTS,
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
	// Handle DELETE /api/internal/blocks/{ulid} across the raw and rollup tiers.
	path := strings.TrimPrefix(r.URL.Path, "/api/internal/blocks")
	if r.Method == "DELETE" && len(path) > 1 {
		ulid := strings.TrimPrefix(path, "/")
		if err := s.db.DeleteAnyBlock(ulid); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}

	// GET: list raw blocks plus every rollup tier, tagged with its resolution so the
	// compactor can apply a per-resolution retention TTL.
	var infos []service.BlockInfo
	for _, b := range s.db.Blocks() {
		meta := b.Meta()
		infos = append(infos, service.BlockInfo{
			ULID:       meta.ULID,
			NodeID:     s.nodeID,
			MinTime:    meta.MinTime,
			MaxTime:    meta.MaxTime,
			NumSamples: meta.Stats.NumSamples,
			NumSeries:  meta.Stats.NumSeries,
			Level:      meta.Compaction.Level,
			Resolution: 0,
		})
	}
	for _, res := range s.db.RollupResolutions() {
		for _, b := range s.db.RollupBlocks(res) {
			meta := b.Meta()
			infos = append(infos, service.BlockInfo{
				ULID:       meta.ULID,
				NodeID:     s.nodeID,
				MinTime:    meta.MinTime,
				MaxTime:    meta.MaxTime,
				NumSamples: meta.Stats.NumWindows,
				NumSeries:  meta.Stats.NumSeries,
				Resolution: meta.Resolution,
			})
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

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("invalid %s=%q, using default %d", key, v, def)
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		log.Printf("invalid %s=%q, using default %v", key, v, def)
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := config.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("invalid %s=%q, using default %s", key, v, def)
	}
	return def
}
