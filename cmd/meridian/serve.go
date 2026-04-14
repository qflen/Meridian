package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/meridiandb/meridian/internal/config"
	"github.com/meridiandb/meridian/internal/ingestion"
	"github.com/meridiandb/meridian/internal/retention"
	"github.com/meridiandb/meridian/internal/server"
	"github.com/meridiandb/meridian/internal/storage"
	"github.com/spf13/cobra"
)

var (
	configPath    string
	dataDir       string
	httpListen    string
	ingListen     string
	clusterListen string
	clusterPeers  string
	flagNodeID    string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start a Meridian node",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().StringVar(&configPath, "config", "meridian.yaml", "Path to config file")
	serveCmd.Flags().StringVar(&dataDir, "data-dir", "", "Data directory (overrides config)")
	serveCmd.Flags().StringVar(&httpListen, "http-listen", "", "HTTP listen address (overrides config)")
	serveCmd.Flags().StringVar(&ingListen, "ingestion-listen", "", "Ingestion/gRPC listen address (overrides config)")
	serveCmd.Flags().StringVar(&clusterListen, "cluster-listen", "", "Cluster gossip listen address (overrides config)")
	serveCmd.Flags().StringVar(&clusterPeers, "cluster-peers", "", "Comma-separated cluster peer addresses")
	serveCmd.Flags().StringVar(&flagNodeID, "node-id", "", "Node ID (overrides config and env)")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("Warning: could not load config file %s: %v, using defaults", configPath, err)
		cfg = config.DefaultConfig()
	}

	if dataDir != "" {
		cfg.Storage.DataDir = dataDir
		cfg.Storage.WALDir = dataDir + "/wal"
	}
	if httpListen != "" {
		cfg.Server.HTTPAddr = httpListen
	}
	if ingListen != "" {
		cfg.Server.GRPCAddr = ingListen
	}
	if clusterListen != "" {
		cfg.Cluster.BindAddr = clusterListen
	}
	if clusterPeers != "" {
		cfg.Cluster.Join = strings.Split(clusterPeers, ",")
	}
	if flagNodeID != "" {
		cfg.Cluster.NodeID = flagNodeID
	} else if envID := os.Getenv("MERIDIAN_NODE_ID"); envID != "" && cfg.Cluster.NodeID == "" {
		cfg.Cluster.NodeID = envID
	}

	// Open TSDB
	opts := storage.TSDBOptions{
		WALDir:          cfg.Storage.WALDir,
		BlockDir:        cfg.Storage.DataDir + "/blocks",
		BlockDuration:   cfg.Storage.BlockDuration.Std(),
		RetentionPeriod: cfg.Storage.Retention.Std(),
		FlushInterval:   cfg.Storage.FlushInterval.Std(),
	}

	db, err := storage.Open(cfg.Storage.DataDir, opts)
	if err != nil {
		return fmt.Errorf("open TSDB: %w", err)
	}

	// Start ingestion server
	ingServer := ingestion.NewServer(db, cfg.Ingestion.BatchSize, cfg.Ingestion.FlushInterval.Std())
	if err := ingServer.Start(cfg.Server.GRPCAddr); err != nil {
		return fmt.Errorf("start ingestion server: %w", err)
	}

	// Start HTTP server
	nodeID := cfg.Cluster.NodeID
	if nodeID == "" {
		nodeID = fmt.Sprintf("node-%d", os.Getpid())
	}

	// Derive peer HTTP addresses from cluster peers
	var peerHTTPAddrs []string
	if len(cfg.Cluster.Join) > 0 {
		_, httpPort, _ := net.SplitHostPort(cfg.Server.HTTPAddr)
		if httpPort == "" {
			httpPort = "8080"
		}
		for _, peer := range cfg.Cluster.Join {
			host, _, err := net.SplitHostPort(peer)
			if err != nil {
				host = peer
			}
			peerHTTPAddrs = append(peerHTTPAddrs, net.JoinHostPort(host, httpPort))
		}
	}
	httpServer := server.NewHTTPServer(db, nodeID, peerHTTPAddrs)
	if err := httpServer.Start(cfg.Server.HTTPAddr); err != nil {
		return fmt.Errorf("start HTTP server: %w", err)
	}

	// Start retention enforcer
	enforcer := retention.NewEnforcer(db, cfg.Storage.Retention.Std())
	enforcer.Start()

	// Start internal metrics broadcaster
	go broadcastInternalMetrics(httpServer.Hub(), db, ingServer)

	fmt.Printf("Meridian node started (HTTP %s, gRPC %s, node=%s)\n", cfg.Server.HTTPAddr, cfg.Server.GRPCAddr, nodeID)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\nShutting down...")
	httpServer.Stop()
	ingServer.Stop()
	enforcer.Stop()
	db.Close()
	fmt.Println("Shutdown complete.")
	return nil
}

func broadcastInternalMetrics(hub *server.WebSocketHub, db *storage.TSDB, ingServer *ingestion.Server) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastIngested int64

	for range ticker.C {
		stats := db.Stats()
		currentIngested := db.IngestionRate()
		ingestionRate := currentIngested - lastIngested
		lastIngested = currentIngested

		metrics := map[string]interface{}{
			"type":            "stats",
			"ingestionRate":   ingestionRate,
			"activeSeries":    stats.TotalSeries,
			"memoryBytes":     stats.HeadSamples * 16, // approximate
			"compressedBytes": stats.ChunkBytes,
			"rawBytes":        stats.StorageBytesRaw,
			"walSegments":     stats.WALSize,
			"blockCount":      stats.BlockCount,
			"uptimeSeconds":   int(time.Since(db.StartTime()).Seconds()),
		}

		hub.BroadcastMetrics(metrics)

		// Also broadcast individual metric messages for live stream
		head := db.Head()
		seriesInfos := head.SeriesInfos()
		if len(seriesInfos) > 0 {
			count := 0
			for _, si := range seriesInfos {
				if si.SampleCount > 0 {
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
					hub.BroadcastMetrics(map[string]interface{}{
						"type":      "metric",
						"series":    seriesKey,
						"labels":    si.Labels,
						"timestamp": time.Now().UnixMilli(),
						"value":     si.LastValue,
					})
					count++
					if count >= 20 {
						break
					}
				}
			}
		}
	}
}

