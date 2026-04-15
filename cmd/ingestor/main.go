// Ingestor service — receives metric writes via TCP (simulator) and HTTP,
// shards by metric name hash, and forwards to storage nodes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pb "github.com/meridiandb/meridian/internal/ingestion/proto"
	"github.com/meridiandb/meridian/internal/service"
)

func main() {
	httpAddr := envOrDefault("INGESTOR_HTTP_ADDR", ":8080")
	tcpAddr := envOrDefault("INGESTOR_TCP_ADDR", ":9090")
	storageAddrs := strings.Split(envOrDefault("STORAGE_ADDRS", "localhost:8081"), ",")
	nodeID := envOrDefault("INGESTOR_NODE_ID", "ingestor-1")

	sc := service.NewStorageClient(storageAddrs)

	srv := &ingestorServer{
		nodeID:    nodeID,
		storage:   sc,
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
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/api/internal/ingest", srv.handleHTTPIngest)

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
	startTime time.Time
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

	resp, err := s.storage.Write(r.Context(), req)
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

		resp, err := s.storage.Write(context.Background(), svcReq)
		if err != nil {
			log.Printf("Storage write error: %v", err)
			encoder.Encode(pb.WriteResponse{SamplesIngested: 0})
			continue
		}

		encoder.Encode(pb.WriteResponse{SamplesIngested: resp.SamplesIngested})
	}
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

func init() {
	// suppress unused import
	_ = fmt.Sprintf
}
