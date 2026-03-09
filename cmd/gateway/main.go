// Gateway service — public HTTP API, serves dashboard, WebSocket hub,
// proxies queries to querier and writes to ingestors.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/meridiandb/meridian/internal/anomaly"
	"github.com/meridiandb/meridian/internal/server"
	"github.com/meridiandb/meridian/internal/service"
)

// Cadence/retention for the gateway's streaming anomaly detector, matching the
// monolith: evict series not seen within anomalyTTL every anomalyEvictEvery ticks.
const (
	anomalyEvictEvery = 30
	anomalyTTL        = 5 * time.Minute
)

func main() {
	httpAddr := envOrDefault("GATEWAY_HTTP_ADDR", ":8080")
	querierAddr := envOrDefault("QUERIER_ADDR", "localhost:8082")
	ingestorAddrs := strings.Split(envOrDefault("INGESTOR_ADDRS", "localhost:8083"), ",")
	storageAddrs := strings.Split(envOrDefault("STORAGE_ADDRS", "localhost:8081"), ",")
	nodeID := envOrDefault("GATEWAY_NODE_ID", "gateway-1")
	allowedOrigins := splitOrigins(envOrDefault("GATEWAY_ALLOWED_ORIGINS", ""))

	sc := service.NewStorageClient(storageAddrs)

	// Streaming anomaly detector over the cluster broadcast (ADR-024), enabled by
	// default and toggled with GATEWAY_ANOMALY_ENABLED. Uses the detector's standard
	// tunables; the monolith reads the same defaults from config.
	var det *anomaly.Detector
	if envBool("GATEWAY_ANOMALY_ENABLED", true) {
		acfg := anomaly.DefaultConfig()
		acfg.Enabled = true
		// Optional seasonal model (ADR-028); GATEWAY_ANOMALY_MODE=holt_winters selects
		// it. Season tunables take the detector's defaults (48 buckets over 24h); an
		// unrecognised value falls back to EWMA via withDefaults.
		if mode := os.Getenv("GATEWAY_ANOMALY_MODE"); mode != "" {
			acfg.Mode = anomaly.Mode(mode)
		}
		det = anomaly.New(acfg)
	}

	gw := &gatewayServer{
		nodeID:        nodeID,
		querierAddr:   querierAddr,
		ingestorAddrs: ingestorAddrs,
		storageAddrs:  storageAddrs,
		storageCli:    sc,
		wsHub:         server.NewWebSocketHub(),
		latency:       service.NewLatencyTracker(),
		startTime:     time.Now(),
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		anomalyDet:    det,
	}

	go gw.wsHub.Run()

	mux := http.NewServeMux()
	gw.registerRoutes(mux)

	httpServer := &http.Server{Addr: httpAddr, Handler: server.CORSMiddleware(allowedOrigins, server.GuardTraversal(mux))}

	// Background: broadcast stats to WebSocket clients
	go gw.broadcastLoop()

	go func() {
		log.Printf("Gateway %s listening on %s (querier=%s, ingestors=%v, storage=%v)",
			nodeID, httpAddr, querierAddr, ingestorAddrs, storageAddrs)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down gateway...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)
	log.Println("Gateway stopped.")
}

type gatewayServer struct {
	nodeID        string
	querierAddr   string
	ingestorAddrs []string
	storageAddrs  []string
	storageCli    *service.StorageClient
	wsHub         *server.WebSocketHub
	latency       *service.LatencyTracker
	startTime     time.Time
	httpClient    *http.Client
	anomalyDet    *anomaly.Detector // nil when anomaly detection is disabled
}

func (gw *gatewayServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", gw.handleHealth)
	mux.HandleFunc("/api/v1/query", gw.handleQuery)
	mux.HandleFunc("/api/v1/series", gw.handleSeries)
	mux.HandleFunc("/api/v1/labels", gw.handleLabels)
	mux.HandleFunc("/api/v1/label/", gw.handleLabelValues)
	mux.HandleFunc("/api/v1/stats", gw.handleStats)
	mux.HandleFunc("/api/v1/cluster", gw.handleCluster)
	mux.HandleFunc("/api/v1/blocks", gw.handleBlocks)
	mux.HandleFunc("/api/v1/anomalies", gw.handleAnomalies)
	mux.HandleFunc("/api/v1/query_latency", gw.handleLatency)
	mux.HandleFunc("/metrics", gw.handleMetrics)
	mux.HandleFunc("/ws/metrics", gw.handleWSMetrics)

	// Serve dashboard static files
	dashboardDir := findDashboardDir()
	if dashboardDir != "" {
		fs := http.FileServer(http.Dir(dashboardDir))
		mux.Handle("/assets/", fs)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" || !fileExists(filepath.Join(dashboardDir, r.URL.Path)) {
				http.ServeFile(w, r, filepath.Join(dashboardDir, "index.html"))
				return
			}
			fs.ServeHTTP(w, r)
		})
	}
}

func (gw *gatewayServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"status":  "ok",
		"node_id": gw.nodeID,
		"role":    "gateway",
		"uptime":  time.Since(gw.startTime).String(),
	})
}

// handleQuery proxies to the querier service.
func (gw *gatewayServer) handleQuery(w http.ResponseWriter, r *http.Request) {
	startExec := time.Now()

	url := fmt.Sprintf("http://%s/api/internal/query?%s", gw.querierAddr, r.URL.RawQuery)
	resp, err := gw.httpClient.Get(url)
	if err != nil {
		writeError(w, http.StatusBadGateway, "querier unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	gw.latency.Record(time.Since(startExec))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleSeries proxies to querier.
func (gw *gatewayServer) handleSeries(w http.ResponseWriter, r *http.Request) {
	proxyGET(gw.httpClient, w, fmt.Sprintf("http://%s/api/internal/series", gw.querierAddr))
}

// handleLabels proxies to querier.
func (gw *gatewayServer) handleLabels(w http.ResponseWriter, r *http.Request) {
	proxyGET(gw.httpClient, w, fmt.Sprintf("http://%s/api/internal/labels", gw.querierAddr))
}

// handleLabelValues proxies to querier.
func (gw *gatewayServer) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/api/v1/label/")
	proxyGET(gw.httpClient, w, fmt.Sprintf("http://%s/api/internal/label/%s", gw.querierAddr, suffix))
}

// handleStats aggregates stats from all storage nodes.
func (gw *gatewayServer) handleStats(w http.ResponseWriter, r *http.Request) {
	agg, err := gw.storageCli.FetchStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ratio := float64(0)
	if agg.StorageBytesDisk > 0 {
		ratio = float64(agg.StorageBytesRaw) / float64(agg.StorageBytesDisk)
	}
	iq := gw.fetchIngestStats(r.Context())
	out := map[string]interface{}{
		"total_samples":            agg.TotalSamples,
		"total_series":             agg.TotalSeries,
		"blocks":                   agg.BlockCount,
		"compression_ratio":        fmt.Sprintf("%.1f", ratio),
		"storage_bytes_raw":        agg.StorageBytesRaw,
		"storage_bytes_compressed": agg.StorageBytesDisk,
		"head_samples":             agg.HeadSamples,
		"head_series":              agg.HeadSeries,
		"wal_size":                 agg.WALSize,
		"ingestion_rate":           agg.IngestionRate,
		"ingest_queue_depth":       iq.Depth,
		"ingest_queue_capacity":    iq.Capacity,
		"dropped_samples":          iq.DroppedSamples,
		"uptime":                   time.Since(gw.startTime).String(),
	}
	if gw.anomalyDet != nil {
		out["anomalies_total"] = gw.anomalyDet.Total()
		out["active_anomalies"] = gw.anomalyDet.Active()
		out["anomaly_model"] = string(gw.anomalyDet.Mode())
	}
	writeJSON(w, out)
}

// ingestQueueStats is the ingest-queue load aggregated across ingestors.
type ingestQueueStats struct {
	Depth          int64
	Capacity       int64
	HighWatermark  int64
	DroppedSamples int64
}

// fetchIngestStats sums each ingestor's bounded-queue snapshot so the dashboard can
// show cluster-wide ingest-queue depth and drops. Unreachable ingestors are skipped.
func (gw *gatewayServer) fetchIngestStats(ctx context.Context) ingestQueueStats {
	var agg ingestQueueStats
	for _, addr := range gw.ingestorAddrs {
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%s/api/internal/ingest_stats", addr), nil)
		if err != nil {
			continue
		}
		resp, err := gw.httpClient.Do(req)
		if err != nil {
			continue
		}
		var s struct {
			Depth          int64 `json:"depth"`
			Capacity       int64 `json:"capacity"`
			HighWatermark  int64 `json:"high_watermark"`
			DroppedSamples int64 `json:"dropped_samples"`
		}
		if resp.StatusCode == http.StatusOK {
			json.NewDecoder(resp.Body).Decode(&s)
		}
		resp.Body.Close()
		agg.Depth += s.Depth
		agg.Capacity += s.Capacity
		agg.HighWatermark += s.HighWatermark
		agg.DroppedSamples += s.DroppedSamples
	}
	return agg
}

// handleCluster returns the full microservice topology.
func (gw *gatewayServer) handleCluster(w http.ResponseWriter, r *http.Request) {
	var nodes []service.NodeInfo

	// Self (gateway)
	nodes = append(nodes, service.NodeInfo{
		ID: gw.nodeID, Addr: "gateway", State: "active", Role: "gateway",
	})

	// Probe ingestors
	for _, addr := range gw.ingestorAddrs {
		id, ok := service.HealthCheck(addr)
		state := "dead"
		if ok {
			state = "active"
		}
		if id == "" {
			id = addr
		}
		nodes = append(nodes, service.NodeInfo{
			ID: id, Addr: addr, State: state, Role: "ingestor",
		})
	}

	// Probe storage nodes
	for _, addr := range gw.storageAddrs {
		id, ok := service.HealthCheck(addr)
		state := "dead"
		if ok {
			state = "active"
		}
		if id == "" {
			id = addr
		}
		nodes = append(nodes, service.NodeInfo{
			ID: id, Addr: addr, State: state, Role: "storage",
		})
	}

	// Probe querier
	{
		id, ok := service.HealthCheck(gw.querierAddr)
		state := "dead"
		if ok {
			state = "active"
		}
		if id == "" {
			id = gw.querierAddr
		}
		nodes = append(nodes, service.NodeInfo{
			ID: id, Addr: gw.querierAddr, State: state, Role: "querier",
		})
	}

	writeJSON(w, map[string]interface{}{"nodes": nodes})
}

// handleBlocks aggregates block metadata from all storage nodes.
func (gw *gatewayServer) handleBlocks(w http.ResponseWriter, r *http.Request) {
	blocks, err := gw.storageCli.FetchBlocks(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"blocks": blocks})
}

// handleLatency returns the query latency histogram.
func (gw *gatewayServer) handleLatency(w http.ResponseWriter, r *http.Request) {
	// Fetch from querier
	resp, err := gw.httpClient.Get(fmt.Sprintf("http://%s/api/internal/latency", gw.querierAddr))
	if err != nil {
		// Fall back to gateway's own latency tracker
		writeJSON(w, gw.latency.Buckets())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}

func (gw *gatewayServer) handleWSMetrics(w http.ResponseWriter, r *http.Request) {
	server.HandleWSUpgrade(gw.wsHub, w, r)
}

// handleAnomalies returns the gateway's bounded recent-anomalies buffer so a
// late-joining dashboard can seed its alerts strip.
func (gw *gatewayServer) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, server.RecentAnomaliesPayload(gw.anomalyDet))
}

func (gw *gatewayServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	server.WriteServiceMetrics(w, gw.nodeID, "gateway", time.Since(gw.startTime))
	fmt.Fprintf(w, "# HELP meridian_ws_clients Connected dashboard WebSocket clients.\n")
	fmt.Fprintf(w, "# TYPE meridian_ws_clients gauge\n")
	fmt.Fprintf(w, "meridian_ws_clients{node=%q,role=\"gateway\"} %d\n", gw.nodeID, gw.wsHub.ClientCount())
	if gw.anomalyDet != nil {
		server.WriteAnomalyMetrics(w, gw.nodeID, "gateway", string(gw.anomalyDet.Mode()), gw.anomalyDet.Total(), gw.anomalyDet.Active())
	}
}

// broadcastLoop periodically polls storage nodes for stats and series data,
// then broadcasts to WebSocket clients.
func (gw *gatewayServer) broadcastLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var tick uint64
	for range ticker.C {
		tick++
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

		agg, err := gw.storageCli.FetchStats(ctx)
		if err != nil {
			cancel()
			continue
		}

		// Aggregate ingest-queue load across ingestors for the dashboard load view.
		iq := gw.fetchIngestStats(ctx)

		// Each storage node already reports a windowed samples/sec rate; the cluster
		// rate is their sum (FetchStats aggregates it).
		gw.wsHub.BroadcastMetrics(map[string]interface{}{
			"type":            "stats",
			"ingestionRate":   agg.IngestionRate,
			"activeSeries":    agg.TotalSeries,
			"memoryBytes":     agg.HeadSamples * 16,
			"compressedBytes": agg.StorageBytesDisk,
			"rawBytes":        agg.StorageBytesRaw,
			"walBytes":        agg.WALSize,
			"blockCount":      agg.BlockCount,
			"uptimeSeconds":   int(time.Since(gw.startTime).Seconds()),
			// Write-path backpressure (ADR-023), summed across ingestors.
			"ingestQueueDepth":         iq.Depth,
			"ingestQueueCapacity":      iq.Capacity,
			"ingestQueueHighWatermark": iq.HighWatermark,
			"droppedSamples":           iq.DroppedSamples,
		})

		// Broadcast live metric stream from storage nodes, and feed the anomaly
		// detector (ADR-024) from every live series — the same uniform per-series
		// stream the monolith uses. The detector dedups on LastTS, so the same series
		// fetched from multiple replicas at one timestamp is observed once.
		series, _ := gw.storageCli.FetchSeries(ctx)
		var samples []anomaly.Sample
		if gw.anomalyDet != nil {
			samples = make([]anomaly.Sample, 0, len(series))
		}
		count := 0
		for _, si := range series {
			if si.SampleCount == 0 {
				continue
			}
			key := server.SeriesKey(si.Name, si.Labels)
			if gw.anomalyDet != nil {
				samples = append(samples, anomaly.Sample{
					Series:      key,
					Metric:      si.Name,
					Labels:      si.Labels,
					Value:       si.LastValue,
					TimestampMs: si.LastTS,
				})
			}
			if count < 20 {
				gw.wsHub.BroadcastMetrics(map[string]interface{}{
					"type":      "metric",
					"series":    key,
					"labels":    si.Labels,
					"timestamp": time.Now().UnixMilli(),
					"value":     si.LastValue,
				})
				count++
			} else if gw.anomalyDet == nil {
				break
			}
		}

		if gw.anomalyDet != nil {
			server.BroadcastAnomalies(gw.wsHub, gw.anomalyDet.ObserveBatch(samples))
			if tick%anomalyEvictEvery == 0 {
				gw.anomalyDet.Evict(time.Now().UnixMilli() - anomalyTTL.Milliseconds())
			}
		}

		cancel()
	}
}

func proxyGET(client *http.Client, w http.ResponseWriter, url string) {
	resp, err := client.Get(url)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
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

func findDashboardDir() string {
	candidates := []string{
		"dashboard/dist",
		"../dashboard/dist",
		"../../dashboard/dist",
		"/app/dashboard/dist",
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

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool reads a boolean env var, returning def when unset or unparseable. Accepts
// the usual 1/0, true/false, yes/no, on/off forms (case-insensitive).
func envBool(key string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// splitOrigins parses a comma-separated origin list, dropping blanks. An empty input
// yields nil so the CORS policy falls back to its localhost-only default.
func splitOrigins(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
