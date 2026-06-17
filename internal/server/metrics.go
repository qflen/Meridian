package server

import (
	"fmt"
	"io"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// WriteStorageMetrics writes the storage-engine Prometheus metrics for db, labelled
// by node. meridian_samples_ingested_total is the cumulative counter (IngestedTotal),
// never the windowed IngestionRate. It is shared by the monolith /metrics handler and
// the storage microservice so every storage node exposes identical metrics.
func WriteStorageMetrics(w io.Writer, db *storage.TSDB, node string) {
	stats := db.Stats()
	ratio := db.CompressionRatio()

	fmt.Fprintf(w, "# HELP meridian_samples_ingested_total Total samples ingested since startup.\n")
	fmt.Fprintf(w, "# TYPE meridian_samples_ingested_total counter\n")
	fmt.Fprintf(w, "meridian_samples_ingested_total{node=%q} %d\n", node, db.IngestedTotal())

	fmt.Fprintf(w, "# HELP meridian_out_of_order_samples_total Samples rejected for arriving out of order.\n")
	fmt.Fprintf(w, "# TYPE meridian_out_of_order_samples_total counter\n")
	fmt.Fprintf(w, "meridian_out_of_order_samples_total{node=%q} %d\n", node, db.OutOfOrderTotal())

	fmt.Fprintf(w, "# HELP meridian_head_samples Samples currently resident in the in-memory head block.\n")
	fmt.Fprintf(w, "# TYPE meridian_head_samples gauge\n")
	fmt.Fprintf(w, "meridian_head_samples{node=%q} %d\n", node, stats.HeadSamples)

	fmt.Fprintf(w, "# HELP meridian_active_series Distinct (name,labels) tuples currently tracked.\n")
	fmt.Fprintf(w, "# TYPE meridian_active_series gauge\n")
	fmt.Fprintf(w, "meridian_active_series{node=%q} %d\n", node, stats.TotalSeries)

	fmt.Fprintf(w, "# HELP meridian_blocks Number of flushed on-disk blocks.\n")
	fmt.Fprintf(w, "# TYPE meridian_blocks gauge\n")
	fmt.Fprintf(w, "meridian_blocks{node=%q} %d\n", node, stats.BlockCount)

	fmt.Fprintf(w, "# HELP meridian_storage_bytes Storage footprint by layer.\n")
	fmt.Fprintf(w, "# TYPE meridian_storage_bytes gauge\n")
	fmt.Fprintf(w, "meridian_storage_bytes{node=%q,layer=\"raw\"} %d\n", node, stats.StorageBytesRaw)
	fmt.Fprintf(w, "meridian_storage_bytes{node=%q,layer=\"compressed\"} %d\n", node, stats.ChunkBytes)
	fmt.Fprintf(w, "meridian_storage_bytes{node=%q,layer=\"disk\"} %d\n", node, stats.StorageBytesDisk)
	fmt.Fprintf(w, "meridian_storage_bytes{node=%q,layer=\"wal\"} %d\n", node, stats.WALSize)

	fmt.Fprintf(w, "# HELP meridian_compression_ratio Raw-to-compressed size ratio for Gorilla-encoded chunks.\n")
	fmt.Fprintf(w, "# TYPE meridian_compression_ratio gauge\n")
	fmt.Fprintf(w, "meridian_compression_ratio{node=%q} %.3f\n", node, ratio)
}

// WriteServiceMetrics writes process-level metrics common to every Meridian service
// so that gateway/querier/ingestor/compactor all expose a valid scrape endpoint.
func WriteServiceMetrics(w io.Writer, node, role string, uptime time.Duration) {
	fmt.Fprintf(w, "# HELP meridian_up Whether the service is up (always 1 when scraped).\n")
	fmt.Fprintf(w, "# TYPE meridian_up gauge\n")
	fmt.Fprintf(w, "meridian_up{node=%q,role=%q} 1\n", node, role)

	fmt.Fprintf(w, "# HELP meridian_uptime_seconds Seconds since this service started.\n")
	fmt.Fprintf(w, "# TYPE meridian_uptime_seconds counter\n")
	fmt.Fprintf(w, "meridian_uptime_seconds{node=%q,role=%q} %d\n", node, role, int64(uptime.Seconds()))
}
