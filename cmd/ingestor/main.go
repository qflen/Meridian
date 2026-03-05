// Ingestor service — receives metric writes via TCP (simulator) and HTTP,
// shards by metric name hash, and forwards to storage nodes.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/meridiandb/meridian/internal/config"
	pb "github.com/meridiandb/meridian/internal/ingestion/proto"
	"github.com/meridiandb/meridian/internal/server"
	"github.com/meridiandb/meridian/internal/service"
)

func main() {
	httpAddr := envOrDefault("INGESTOR_HTTP_ADDR", ":8080")
	tcpAddr := envOrDefault("INGESTOR_TCP_ADDR", ":9090")
	storageAddrs := strings.Split(envOrDefault("STORAGE_ADDRS", "localhost:8081"), ",")
	nodeID := envOrDefault("INGESTOR_NODE_ID", "ingestor-1")

	sc := newStorageClient(storageAddrs)

	// Drive ring node-state from /health so replicated writes route around dead nodes.
	monitorCtx, stopMonitor := context.WithCancel(context.Background())
	defer stopMonitor()
	sc.StartHealthMonitor(monitorCtx, 2*time.Second)

	// Bound in-flight writes: a worker pool drains a bounded queue to the quorum
	// Write, so a stalled replica backpressures (then sheds → 429) the producer
	// instead of piling up unbounded concurrent writes. See ADR-023.
	// Optional per-series fair-share / priority-class shedding (ADR-027), configured by
	// env; nil (the default) leaves the queue's uniform block-then-shed in place.
	adm := config.AdmissionFromEnv("INGEST")
	if err := adm.Validate(); err != nil {
		log.Fatalf("invalid admission config: %v", err)
	}
	pool := service.NewWritePool(sc, service.PoolOptions{
		Capacity:      envInt("INGEST_QUEUE_CAPACITY", 50000),
		HighWatermark: envInt("INGEST_QUEUE_HIGH_WATERMARK", 40000),
		BlockDeadline: envDuration("INGEST_BLOCK_DEADLINE", 250*time.Millisecond),
		Workers:       envInt("MAX_CONCURRENT_WRITES", 64),
		Admission:     adm.Shaper(),
	})
	defer pool.Close()

	srv := &ingestorServer{
		nodeID:    nodeID,
		storage:   sc,
		pool:      pool,
		startTime: time.Now(),
	}

	// Start TCP listener for simulator
	tcpListener, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		log.Fatalf("listen TCP %s: %v", tcpAddr, err)
	}
	go srv.acceptTCP(tcpListener)
	log.Printf("Ingestor %s TCP listening on %s", nodeID, tcpAddr)

	// Start HTTP server
	mux := http.NewServeMux()
	srv.registerRoutes(mux)

	httpServer := &http.Server{Addr: httpAddr, Handler: corsMiddleware(mux)}
	go func() {
		log.Printf("Ingestor %s HTTP listening on %s → storage %v", nodeID, httpAddr, storageAddrs)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down ingestor...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tcpListener.Close()
	httpServer.Shutdown(ctx)
	log.Println("Ingestor stopped.")
}

type ingestorServer struct {
	nodeID    string
	storage   *service.StorageClient
	pool      *service.WritePool
	startTime time.Time
}

func (s *ingestorServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/internal/ingest", s.handleHTTPIngest)
	mux.HandleFunc("/api/internal/ingest_stats", s.handleIngestStats)
	mux.HandleFunc("/metrics", s.handleMetrics)
}

// handleIngestStats reports the bounded ingest queue snapshot as JSON so the
// gateway can aggregate ingest-queue load across ingestors for the dashboard.
func (s *ingestorServer) handleIngestStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.pool == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{})
		return
	}
	st := s.pool.Stats()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"depth":               st.Depth,
		"capacity":            st.Capacity,
		"high_watermark":      st.HighWatermark,
		"dropped_samples":     st.DroppedSamples,
		"shed_events":         st.ShedEvents,
		"backpressure_events": st.BackpressureEvents,
	})
}

func (s *ingestorServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	server.WriteServiceMetrics(w, s.nodeID, "ingestor", time.Since(s.startTime))
	if s.pool != nil {
		server.WriteQueueMetrics(w, s.nodeID, "ingestor", s.pool.Stats())
		server.WriteAdmissionMetrics(w, s.nodeID, "ingestor", s.pool.AdmissionStats())
	}
}

func (s *ingestorServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"node_id": s.nodeID,
		"role":    "ingestor",
		"uptime":  time.Since(s.startTime).String(),
	})
}

func (s *ingestorServer) handleHTTPIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req service.WriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	resp, _, err := s.pool.Submit(r.Context(), req)
	if errors.Is(err, service.ErrShed) {
		// Overloaded: the bounded queue was full past the block deadline. Shed the
		// request and tell the producer to back off — backpressure as a 429.
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": "ingest overloaded, retry after backing off"})
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// acceptTCP handles the TCP ingestion protocol (same JSON-over-TCP as the monolith).
func (s *ingestorServer) acceptTCP(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleTCPConn(conn)
	}
}

func (s *ingestorServer) handleTCPConn(conn net.Conn) {
	defer conn.Close()
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		var req pb.WriteRequest
		if err := decoder.Decode(&req); err != nil {
			if err != io.EOF {
				log.Printf("TCP decode error: %v", err)
			}
			return
		}

		// Convert proto types to service types
		svcReq := protoToServiceRequest(req)

		resp, _, err := s.pool.Submit(context.Background(), svcReq)
		if errors.Is(err, service.ErrShed) {
			// Overloaded: NACK the producer with the shed sample count so it eases off.
			encoder.Encode(pb.WriteResponse{Shed: requestSamples(svcReq), Throttled: true})
			continue
		}
		if err != nil {
			log.Printf("Storage write error: %v", err)
			encoder.Encode(pb.WriteResponse{SamplesIngested: 0})
			continue
		}

		encoder.Encode(pb.WriteResponse{SamplesIngested: resp.SamplesIngested})
	}
}

// requestSamples totals the samples in a request, used to report the shed count
// when the producer is NACKed.
func requestSamples(req service.WriteRequest) int64 {
	var n int64
	for _, ts := range req.TimeSeries {
		n += int64(len(ts.Samples))
	}
	return n
}

func protoToServiceRequest(req pb.WriteRequest) service.WriteRequest {
	svcReq := service.WriteRequest{
		TimeSeries: make([]service.TimeSeries, len(req.TimeSeries)),
	}
	for i, ts := range req.TimeSeries {
		labels := make([]service.Label, len(ts.Labels))
		for j, l := range ts.Labels {
			labels[j] = service.Label{Name: l.Name, Value: l.Value}
		}
		samples := make([]service.Sample, len(ts.Samples))
		for j, s := range ts.Samples {
			samples[j] = service.Sample{TimestampMs: s.TimestampMs, Value: s.Value}
		}
		svcReq.TimeSeries[i] = service.TimeSeries{
			Name:    ts.Name,
			Labels:  labels,
			Samples: samples,
		}
	}
	return svcReq
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

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("invalid %s=%q, using default %d", key, v, def)
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

// newStorageClient builds the replicated storage client from REPLICATION_FACTOR,
// WRITE_QUORUM, READ_QUORUM and VIRTUAL_NODES (defaults 3/2/2/256), validating the
// quorum relationship (W+R>N) before constructing it.
func newStorageClient(storageAddrs []string) *service.StorageClient {
	cc := config.ClusterConfig{
		ReplicationFactor: envInt("REPLICATION_FACTOR", 3),
		WriteQuorum:       envInt("WRITE_QUORUM", 2),
		ReadQuorum:        envInt("READ_QUORUM", 2),
		VirtualNodes:      envInt("VIRTUAL_NODES", 256),
	}
	if err := cc.Validate(); err != nil {
		log.Fatalf("invalid replication config: %v", err)
	}
	log.Printf("replication: N=%d W=%d R=%d across %d storage node(s)",
		cc.ReplicationFactor, cc.WriteQuorum, cc.ReadQuorum, len(storageAddrs))
	return service.NewReplicatedStorageClient(storageAddrs, service.ReplicationOptions{
		ReplicationFactor: cc.ReplicationFactor,
		WriteQuorum:       cc.WriteQuorum,
		ReadQuorum:        cc.ReadQuorum,
		VirtualNodes:      cc.VirtualNodes,
	})
}

func init() {
	// suppress unused import
	_ = fmt.Sprintf
}
