package server

import (
	"fmt"
	"io"
	"time"

	"github.com/meridiandb/meridian/internal/backpressure"
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

// WriteQueueMetrics writes the write-path flow-control metrics for one node/role
// from a bounded-queue snapshot (ADR-023): the cumulative drop/shed/backpressure
// counters and the depth/capacity/high-water gauges. It is shared by the monolith
// and the ingestor/storage services so every node that bounds ingest reports the
// same flow-control metrics. The counters are cumulative (Prometheus-correct); a
// scraper derives a drop rate with rate().
func WriteQueueMetrics(w io.Writer, node, role string, st backpressure.Stats) {
	fmt.Fprintf(w, "# HELP meridian_dropped_samples_total Samples shed because the ingest queue was full past the block deadline.\n")
	fmt.Fprintf(w, "# TYPE meridian_dropped_samples_total counter\n")
	fmt.Fprintf(w, "meridian_dropped_samples_total{node=%q,role=%q} %d\n", node, role, st.DroppedSamples)

	fmt.Fprintf(w, "# HELP meridian_ingest_shed_events_total Enqueue attempts that shed a batch under overload.\n")
	fmt.Fprintf(w, "# TYPE meridian_ingest_shed_events_total counter\n")
	fmt.Fprintf(w, "meridian_ingest_shed_events_total{node=%q,role=%q} %d\n", node, role, st.ShedEvents)

	fmt.Fprintf(w, "# HELP meridian_ingest_backpressure_events_total Enqueue attempts that blocked because the queue was full.\n")
	fmt.Fprintf(w, "# TYPE meridian_ingest_backpressure_events_total counter\n")
	fmt.Fprintf(w, "meridian_ingest_backpressure_events_total{node=%q,role=%q} %d\n", node, role, st.BackpressureEvents)

	fmt.Fprintf(w, "# HELP meridian_ingest_queue_depth Samples currently resident in the bounded ingest queue.\n")
	fmt.Fprintf(w, "# TYPE meridian_ingest_queue_depth gauge\n")
	fmt.Fprintf(w, "meridian_ingest_queue_depth{node=%q,role=%q} %d\n", node, role, st.Depth)

	fmt.Fprintf(w, "# HELP meridian_ingest_queue_capacity Maximum samples the bounded ingest queue may hold.\n")
	fmt.Fprintf(w, "# TYPE meridian_ingest_queue_capacity gauge\n")
	fmt.Fprintf(w, "meridian_ingest_queue_capacity{node=%q,role=%q} %d\n", node, role, st.Capacity)

	fmt.Fprintf(w, "# HELP meridian_ingest_queue_high_watermark Queue depth at which producers are flagged to throttle.\n")
	fmt.Fprintf(w, "# TYPE meridian_ingest_queue_high_watermark gauge\n")
	fmt.Fprintf(w, "meridian_ingest_queue_high_watermark{node=%q,role=%q} %d\n", node, role, st.HighWatermark)
}

// WriteAnomalyMetrics writes the streaming-anomaly-detector metrics for one
// node/role: the cumulative count of alerts raised (a counter) and the number of
// series currently firing (a gauge). Shared by the monolith and the gateway so
// both expose identical anomaly metrics. See ADR-024.
func WriteAnomalyMetrics(w io.Writer, node, role string, total uint64, active int) {
	fmt.Fprintf(w, "# HELP meridian_anomalies_total Anomaly alerts raised since startup.\n")
	fmt.Fprintf(w, "# TYPE meridian_anomalies_total counter\n")
	fmt.Fprintf(w, "meridian_anomalies_total{node=%q,role=%q} %d\n", node, role, total)

	fmt.Fprintf(w, "# HELP meridian_active_anomalies Series currently in an out-of-band (firing) state.\n")
	fmt.Fprintf(w, "# TYPE meridian_active_anomalies gauge\n")
	fmt.Fprintf(w, "meridian_active_anomalies{node=%q,role=%q} %d\n", node, role, active)
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
