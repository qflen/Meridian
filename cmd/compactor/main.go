// Compactor service — background service that enforces retention policies
// by checking storage nodes for expired blocks and deleting them.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/meridiandb/meridian/internal/service"
)

func main() {
	httpAddr := envOrDefault("COMPACTOR_HTTP_ADDR", ":8080")
	storageAddrs := strings.Split(envOrDefault("STORAGE_ADDRS", "localhost:8081"), ",")
	nodeID := envOrDefault("COMPACTOR_NODE_ID", "compactor-1")
	retentionStr := envOrDefault("RETENTION", "360h") // 15 days default
	retention, err := time.ParseDuration(retentionStr)
	if err != nil {
		retention = 15 * 24 * time.Hour
	}

	sc := service.NewStorageClient(storageAddrs)

	comp := &compactorServer{
		nodeID:    nodeID,
		storage:   sc,
		retention: retention,
		startTime: time.Now(),
	}

	// Start health HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/health", comp.handleHealth)

	httpServer := &http.Server{Addr: httpAddr, Handler: mux}
	go func() {
		log.Printf("Compactor %s listening on %s (retention=%s, storage=%v)",
			nodeID, httpAddr, retention, storageAddrs)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Start retention enforcement loop
	go comp.retentionLoop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down compactor...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)
	log.Println("Compactor stopped.")
}

type compactorServer struct {
	nodeID    string
	storage   *service.StorageClient
	retention time.Duration
	startTime time.Time
}

func (c *compactorServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"node_id": c.nodeID,
		"role":    "compactor",
		"uptime":  time.Since(c.startTime).String(),
	})
}

func (c *compactorServer) retentionLoop() {
	// Initial run after 30 seconds
	time.Sleep(30 * time.Second)
	c.enforceRetention()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		c.enforceRetention()
	}
}

func (c *compactorServer) enforceRetention() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	blocks, err := c.storage.FetchBlocks(ctx)
	if err != nil {
		log.Printf("Compactor: error fetching blocks: %v", err)
		return
	}

	cutoff := time.Now().UnixMilli() - c.retention.Milliseconds()
	deleted := 0

	for _, block := range blocks {
		if block.MaxTime < cutoff {
			// Find the storage node address for this block
			addr := c.findNodeAddr(block.NodeID)
			if addr == "" {
				continue
			}
			log.Printf("Compactor: deleting block %s from %s (max_time=%d < cutoff=%d)",
				block.ULID, block.NodeID, block.MaxTime, cutoff)
			if err := c.storage.DeleteBlock(ctx, addr, block.ULID); err != nil {
				log.Printf("Compactor: error deleting block %s: %v", block.ULID, err)
				continue
			}
			deleted++
		}
	}

	if deleted > 0 {
		log.Printf("Compactor: deleted %d expired blocks", deleted)
	}
}

func (c *compactorServer) findNodeAddr(nodeID string) string {
	// Try to match nodeID to a storage address
	for _, addr := range c.storage.Addrs() {
		id, ok := service.HealthCheck(addr)
		if ok && id == nodeID {
			return addr
		}
	}
	// Fallback: try all addrs
	if len(c.storage.Addrs()) > 0 {
		return c.storage.Addrs()[0]
	}
	return ""
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
