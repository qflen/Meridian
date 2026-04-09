package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
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
	configPath string
	dataDir    string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start a Meridian node",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().StringVar(&configPath, "config", "meridian.yaml", "Path to config file")
	serveCmd.Flags().StringVar(&dataDir, "data-dir", "", "Data directory (overrides config)")
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

	// Open TSDB
	opts := storage.TSDBOptions{
		WALDir:          cfg.Storage.WALDir,
		BlockDir:        cfg.Storage.DataDir + "/blocks",
		BlockDuration:   cfg.Storage.BlockDuration,
		RetentionPeriod: cfg.Storage.Retention,
		FlushInterval:   cfg.Storage.FlushInterval,
	}

	db, err := storage.Open(cfg.Storage.DataDir, opts)
	if err != nil {
		return fmt.Errorf("open TSDB: %w", err)
	}

	// Start ingestion server
	ingServer := ingestion.NewServer(db, cfg.Ingestion.BatchSize, cfg.Ingestion.FlushInterval)
	if err := ingServer.Start(cfg.Server.GRPCAddr); err != nil {
		return fmt.Errorf("start ingestion server: %w", err)
	}

	// Start HTTP server
	nodeID := cfg.Cluster.NodeID
	if nodeID == "" {
		nodeID = fmt.Sprintf("node-%d", os.Getpid())
	}
	httpServer := server.NewHTTPServer(db, nodeID)
	if err := httpServer.Start(cfg.Server.HTTPAddr); err != nil {
		return fmt.Errorf("start HTTP server: %w", err)
	}

	// Start retention enforcer
	enforcer := retention.NewEnforcer(db, cfg.Storage.Retention)
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
			"compressedBytes": stats.StorageBytesDisk,
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

