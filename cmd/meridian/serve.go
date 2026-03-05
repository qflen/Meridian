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

	"github.com/meridiandb/meridian/internal/anomaly"
	"github.com/meridiandb/meridian/internal/config"
	"github.com/meridiandb/meridian/internal/ingestion"
	"github.com/meridiandb/meridian/internal/retention"
	"github.com/meridiandb/meridian/internal/server"
	"github.com/meridiandb/meridian/internal/storage"
	"github.com/spf13/cobra"
)

// Cadence/retention for the streaming anomaly detector fed by the broadcast loop.
// The detector is evicted every anomalyEvictEvery ticks of series not seen within
// anomalyTTL, so its memory follows live cardinality (series leave the head on
// flush/retention) rather than every series ever observed.
const (
	anomalyEvictEvery = 30
	anomalyTTL        = 5 * time.Minute
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
		WALGroupCommit:  cfg.Storage.WALGroupCommit,
		WALCommitLinger: cfg.Storage.WALCommitLinger.Std(),
	}

	db, err := storage.Open(cfg.Storage.DataDir, opts)
	if err != nil {
		return fmt.Errorf("open TSDB: %w", err)
	}

	// Start ingestion server with a bounded, sheddable queue sized from config.
	ingServer := ingestion.NewServerWithQueue(db, cfg.Ingestion.BatchSize, cfg.Ingestion.FlushInterval.Std(), ingestion.QueueOptions{
		Capacity:      cfg.Ingestion.QueueCapacity,
		HighWatermark: cfg.Ingestion.QueueHighWatermark,
		BlockDeadline: cfg.Ingestion.BlockDeadline.Std(),
		// Per-series fair-share / priority-class shedding (ADR-027); nil when disabled,
		// leaving the queue's uniform block-then-shed as the only policy.
		Admission: cfg.Ingestion.Admission.Shaper(),
	})
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
	httpServer.SetQueryTimeout(cfg.Server.QueryTimeout.Std())
	httpServer.SetAllowedOrigins(cfg.Server.AllowedOrigins)
	// Surface the ingest queue depth / capacity / drops on /metrics and /api/v1/stats.
	httpServer.SetIngestStatsSource(ingServer.BatchWriter().QueueStats)
	// Surface the per-series/priority admission counters too (ADR-027); a no-op for the
	// scrape output when admission is disabled.
	httpServer.SetAdmissionStatsSource(ingServer.BatchWriter().AdmissionStats)
	if err := httpServer.Start(cfg.Server.HTTPAddr); err != nil {
		return fmt.Errorf("start HTTP server: %w", err)
	}

	// Start the downsampling cascade (ADR-011) and per-resolution retention. When
	// downsampling is enabled, raw blocks are rolled up to 1m/1h tiers in the
	// background and each tier expires on its own TTL (raw shortest); otherwise only
	// raw retention runs.
	var downsampler *retention.Downsampler
	var enforcer *retention.Enforcer
	if cfg.Downsampling.Enabled {
		downsampler = retention.NewDownsampler(db, cfg.Downsampling.DownsampleRules(), cfg.Downsampling.Interval.Std())
		downsampler.Start()
		enforcer = retention.NewEnforcerWithTiers(db, cfg.Storage.Retention.Std(), cfg.Downsampling.RollupRetentions(), retention.DefaultEnforceInterval)
	} else {
		enforcer = retention.NewEnforcer(db, cfg.Storage.Retention.Std())
	}
	enforcer.Start()

	// Streaming anomaly detector over the live telemetry path (ADR-024). It is fed
	// from the same per-series stream the broadcaster emits each tick and shares the
	// dashboard WebSocket hub; the HTTP server surfaces its recent buffer/counters.
	var det *anomaly.Detector
	if cfg.Anomaly.Enabled {
		det = anomaly.New(cfg.Anomaly.Detector())
		httpServer.SetAnomalyDetector(det)
	}

	// Start internal metrics broadcaster
	go broadcastInternalMetrics(httpServer.Hub(), db, ingServer, det)

	fmt.Printf("Meridian node started (HTTP %s, gRPC %s, node=%s)\n", cfg.Server.HTTPAddr, cfg.Server.GRPCAddr, nodeID)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\nShutting down...")
	httpServer.Stop()
	ingServer.Stop()
	if downsampler != nil {
		downsampler.Stop()
	}
	enforcer.Stop()
	db.Close()
	fmt.Println("Shutdown complete.")
	return nil
}

func broadcastInternalMetrics(hub *server.WebSocketHub, db *storage.TSDB, ingServer *ingestion.Server, det *anomaly.Detector) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var tick uint64
	for range ticker.C {
		tick++
		stats := db.Stats()
		q := ingServer.BatchWriter().QueueStats()

		hub.BroadcastMetrics(map[string]interface{}{
			"type":            "stats",
			"ingestionRate":   db.IngestionRate(),
			"activeSeries":    stats.TotalSeries,
			"memoryBytes":     stats.HeadSamples * 16, // approximate
			"compressedBytes": stats.ChunkBytes,
			"rawBytes":        stats.StorageBytesRaw,
			"walBytes":        stats.WALSize,
			"blockCount":      stats.BlockCount,
			"uptimeSeconds":   int(time.Since(db.StartTime()).Seconds()),
			// Write-path backpressure (ADR-023): bounded queue depth/capacity and the
			// cumulative drop counter. The dashboard derives a drop rate from successive
			// samples, like the ingestion rate/total split.
			"ingestQueueDepth":         q.Depth,
			"ingestQueueCapacity":      q.Capacity,
			"ingestQueueHighWatermark": q.HighWatermark,
			"droppedSamples":           q.DroppedSamples,
		})

		// Per-series stream. The anomaly detector (ADR-024) is fed from *every* live
		// series — the same stream the broadcaster emits — while only a capped slice
		// is pushed to the live-stream display. The detector dedups on LastTS, so a
		// 1 Hz re-read of a slower series is not counted twice.
		seriesInfos := db.Head().SeriesInfos()
		var samples []anomaly.Sample
		if det != nil {
			samples = make([]anomaly.Sample, 0, len(seriesInfos))
		}
		count := 0
		for _, si := range seriesInfos {
			if si.SampleCount == 0 {
				continue
			}
			key := server.SeriesKey(si.Name, si.Labels)
			if det != nil {
				samples = append(samples, anomaly.Sample{
					Series:      key,
					Metric:      si.Name,
					Labels:      si.Labels,
					Value:       si.LastValue,
					TimestampMs: si.LastTS,
				})
			}
			if count < 20 {
				hub.BroadcastMetrics(map[string]interface{}{
					"type":      "metric",
					"series":    key,
					"labels":    si.Labels,
					"timestamp": time.Now().UnixMilli(),
					"value":     si.LastValue,
				})
				count++
			} else if det == nil {
				break // nothing left to do once the display cap is hit
			}
		}

		if det != nil {
			server.BroadcastAnomalies(hub, det.ObserveBatch(samples))
			if tick%anomalyEvictEvery == 0 {
				det.Evict(time.Now().UnixMilli() - anomalyTTL.Milliseconds())
			}
		}
	}
}
