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

	"github.com/meridiandb/meridian/internal/server"
	"github.com/meridiandb/meridian/internal/service"
)

func main() {
	httpAddr := envOrDefault("GATEWAY_HTTP_ADDR", ":8080")
	querierAddr := envOrDefault("QUERIER_ADDR", "localhost:8082")
	ingestorAddrs := strings.Split(envOrDefault("INGESTOR_ADDRS", "localhost:8083"), ",")
	storageAddrs := strings.Split(envOrDefault("STORAGE_ADDRS", "localhost:8081"), ",")
	nodeID := envOrDefault("GATEWAY_NODE_ID", "gateway-1")

	sc := service.NewStorageClient(storageAddrs)

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
	}

	go gw.wsHub.Run()

	mux := http.NewServeMux()
	gw.registerRoutes(mux)

	httpServer := &http.Server{Addr: httpAddr, Handler: corsMiddleware(mux)}

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
	mux.HandleFunc("/api/v1/query_latency", gw.handleLatency)
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
	writeJSON(w, map[string]interface{}{
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
		"uptime":                   time.Since(gw.startTime).String(),
	})
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

// broadcastLoop periodically polls storage nodes for stats and series data,
// then broadcasts to WebSocket clients.
func (gw *gatewayServer) broadcastLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastIngested int64

	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

		agg, err := gw.storageCli.FetchStats(ctx)
		if err != nil {
			cancel()
			continue
		}

		ingestionRate := agg.IngestionRate - lastIngested
		lastIngested = agg.IngestionRate

		gw.wsHub.BroadcastMetrics(map[string]interface{}{
			"type":            "stats",
			"ingestionRate":   ingestionRate,
			"activeSeries":    agg.TotalSeries,
			"memoryBytes":     agg.HeadSamples * 16,
			"compressedBytes": agg.StorageBytesDisk,
			"rawBytes":        agg.StorageBytesRaw,
			"walSegments":     agg.WALSize,
			"blockCount":      agg.BlockCount,
			"uptimeSeconds":   int(time.Since(gw.startTime).Seconds()),
		})

		// Broadcast live metric stream from storage nodes
		series, _ := gw.storageCli.FetchSeries(ctx)
		count := 0
		for _, si := range series {
			if si.SampleCount > 0 && count < 20 {
				seriesKey := si.Name
				if len(si.Labels) > 0 {
					pairs := ""
					for k, v := range si.Labels {
						if k == "__name__" {
							continue
						}
						if pairs != "" {
							pairs += ","
						}
						pairs += k + `="` + v + `"`
					}
					if pairs != "" {
						seriesKey = si.Name + "{" + pairs + "}"
					}
				}
				gw.wsHub.BroadcastMetrics(map[string]interface{}{
					"type":      "metric",
					"series":    seriesKey,
					"labels":    si.Labels,
					"timestamp": time.Now().UnixMilli(),
					"value":     si.LastValue,
				})
				count++
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
