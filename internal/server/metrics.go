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

// WriteRollupMetrics writes the downsampling metrics for db: per-resolution rollup
// block count, stored windows, compressed bytes, and raw samples summarised, plus the
// downsampling-savings figure (raw samples represented per stored rollup window — ~12x
// for 1m and ~720x for 1h over 5s data). Shared by the monolith and the storage
// microservice so every node that holds rollups reports them. See ADR-011.
func WriteRollupMetrics(w io.Writer, db *storage.TSDB, node string) {
	stats := db.RollupStats()

	fmt.Fprintf(w, "# HELP meridian_rollup_blocks Rollup blocks on disk, by resolution.\n")
	fmt.Fprintf(w, "# TYPE meridian_rollup_blocks gauge\n")
	for _, s := range stats {
		fmt.Fprintf(w, "meridian_rollup_blocks{node=%q,resolution=%q} %d\n", node, storage.ResolutionLabel(s.Resolution), s.BlockCount)
	}

	fmt.Fprintf(w, "# HELP meridian_rollup_windows Stored rollup windows (points), by resolution.\n")
	fmt.Fprintf(w, "# TYPE meridian_rollup_windows gauge\n")
	for _, s := range stats {
		fmt.Fprintf(w, "meridian_rollup_windows{node=%q,resolution=%q} %d\n", node, storage.ResolutionLabel(s.Resolution), s.NumWindows)
	}

	fmt.Fprintf(w, "# HELP meridian_rollup_bytes Compressed rollup column bytes on disk, by resolution.\n")
	fmt.Fprintf(w, "# TYPE meridian_rollup_bytes gauge\n")
	for _, s := range stats {
		fmt.Fprintf(w, "meridian_rollup_bytes{node=%q,resolution=%q} %d\n", node, storage.ResolutionLabel(s.Resolution), s.ChunkBytes)
	}

	fmt.Fprintf(w, "# HELP meridian_downsampling_point_reduction Raw samples represented per stored rollup window (the downsampling savings).\n")
	fmt.Fprintf(w, "# TYPE meridian_downsampling_point_reduction gauge\n")
	for _, s := range stats {
		var reduction float64
		if s.NumWindows > 0 {
			reduction = float64(s.RawSamples) / float64(s.NumWindows)
		}
		fmt.Fprintf(w, "meridian_downsampling_point_reduction{node=%q,resolution=%q} %.2f\n", node, storage.ResolutionLabel(s.Resolution), reduction)
	}
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

// WriteAdmissionMetrics writes the per-series fair-share / priority-class admission
// metrics for one node/role from a shaper snapshot (ADR-027): admitted and dropped
// samples by class (drops split by reason — priority band vs fair-share budget), and the
// shed distribution across series-hash buckets so a hot series bucket is visible without
// per-series cardinality. The counters are cumulative (Prometheus-correct); a scraper
// derives a per-class shed rate with rate(). Nothing is emitted when the admission layer
// is disabled (an empty snapshot), so the uniform-shedding default scrape is unchanged.
func WriteAdmissionMetrics(w io.Writer, node, role string, st backpressure.ShaperStats) {
	if len(st.Classes) == 0 {
		return
	}
	fmt.Fprintf(w, "# HELP meridian_admission_admitted_samples_total Samples admitted by the priority/fair-share shaper, by class.\n")
	fmt.Fprintf(w, "# TYPE meridian_admission_admitted_samples_total counter\n")
	for _, c := range st.Classes {
		fmt.Fprintf(w, "meridian_admission_admitted_samples_total{node=%q,role=%q,class=%q} %d\n", node, role, c.Name, c.Admitted)
	}

	fmt.Fprintf(w, "# HELP meridian_admission_dropped_samples_total Samples shed by the priority/fair-share shaper, by class and reason.\n")
	fmt.Fprintf(w, "# TYPE meridian_admission_dropped_samples_total counter\n")
	for _, c := range st.Classes {
		fmt.Fprintf(w, "meridian_admission_dropped_samples_total{node=%q,role=%q,class=%q,reason=\"priority\"} %d\n", node, role, c.Name, c.DroppedPriority)
		fmt.Fprintf(w, "meridian_admission_dropped_samples_total{node=%q,role=%q,class=%q,reason=\"fairshare\"} %d\n", node, role, c.Name, c.DroppedFairShare)
	}

	fmt.Fprintf(w, "# HELP meridian_admission_series_bucket_dropped_total Shed samples by series-hash bucket (bounded cardinality) — a hot bucket flags an unfair series.\n")
	fmt.Fprintf(w, "# TYPE meridian_admission_series_bucket_dropped_total counter\n")
	for i, d := range st.BucketDrops {
		fmt.Fprintf(w, "meridian_admission_series_bucket_dropped_total{node=%q,role=%q,bucket=\"%d\"} %d\n", node, role, i, d)
	}
}

// HandoffStats is a snapshot of the hinted-handoff hint store for metrics (ADR-029).
// The ingestor builds it from its service.HintStore.
type HandoffStats struct {
	// PendingSamples is the samples currently buffered as hints for unreachable replicas.
	PendingSamples int64
	// PendingHints is the buffered hint records (each is one missed write batch).
	PendingHints int64
	// ReplayedSamples is the cumulative samples replayed to recovered replicas.
	ReplayedSamples int64
	// DroppedSamples is the cumulative samples dropped because a target hit its buffer cap.
	DroppedSamples int64
}

// WriteHandoffMetrics writes the hinted-handoff metrics for one node/role (ADR-029): the
// pending-hint gauges (buffered samples and records awaiting a recovered replica) and the
// cumulative replayed/dropped sample counters. It is emitted by the write path (the
// ingestor) that owns the hint store; a scraper derives a replay or drop rate with
// rate(). The counters are cumulative (Prometheus-correct).
func WriteHandoffMetrics(w io.Writer, node, role string, st HandoffStats) {
	fmt.Fprintf(w, "# HELP meridian_handoff_pending_samples Samples currently buffered as hints for unreachable replicas.\n")
	fmt.Fprintf(w, "# TYPE meridian_handoff_pending_samples gauge\n")
	fmt.Fprintf(w, "meridian_handoff_pending_samples{node=%q,role=%q} %d\n", node, role, st.PendingSamples)

	fmt.Fprintf(w, "# HELP meridian_handoff_pending_hints Buffered hint records awaiting replay to a recovered replica.\n")
	fmt.Fprintf(w, "# TYPE meridian_handoff_pending_hints gauge\n")
	fmt.Fprintf(w, "meridian_handoff_pending_hints{node=%q,role=%q} %d\n", node, role, st.PendingHints)

	fmt.Fprintf(w, "# HELP meridian_handoff_replayed_samples_total Samples replayed to recovered replicas via hinted handoff.\n")
	fmt.Fprintf(w, "# TYPE meridian_handoff_replayed_samples_total counter\n")
	fmt.Fprintf(w, "meridian_handoff_replayed_samples_total{node=%q,role=%q} %d\n", node, role, st.ReplayedSamples)

	fmt.Fprintf(w, "# HELP meridian_handoff_dropped_samples_total Hint samples dropped because a target's buffer hit its cap.\n")
	fmt.Fprintf(w, "# TYPE meridian_handoff_dropped_samples_total counter\n")
	fmt.Fprintf(w, "meridian_handoff_dropped_samples_total{node=%q,role=%q} %d\n", node, role, st.DroppedSamples)
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
