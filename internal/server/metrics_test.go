package server

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/backpressure"
	"github.com/meridiandb/meridian/internal/storage"
)

func TestWriteStorageMetricsIncludesStorageFamilies(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(dir, storage.TSDBOptions{
		WALDir:             dir + "/wal",
		BlockDir:           dir + "/blocks",
		FlushInterval:      time.Hour,
		RateSampleInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.Ingest("cpu", map[string]string{"host": "a"}, 1000, 1)

	var buf bytes.Buffer
	WriteStorageMetrics(&buf, db, "node-1")
	body := buf.String()
	for _, want := range []string{
		`meridian_samples_ingested_total{node="node-1"}`,
		`meridian_out_of_order_samples_total{node="node-1"}`,
		`meridian_active_series{node="node-1"}`,
		`meridian_storage_bytes{node="node-1",layer="wal"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("storage metrics missing %q\n%s", want, body)
		}
	}
}

func TestWriteServiceMetricsLabelsRole(t *testing.T) {
	var buf bytes.Buffer
	WriteServiceMetrics(&buf, "gw-1", "gateway", 5*time.Second)
	body := buf.String()
	for _, want := range []string{
		`meridian_up{node="gw-1",role="gateway"} 1`,
		`meridian_uptime_seconds{node="gw-1",role="gateway"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("service metrics missing %q\n%s", want, body)
		}
	}
}

func TestWriteQueueMetricsFamilies(t *testing.T) {
	var buf bytes.Buffer
	WriteQueueMetrics(&buf, "node-1", "monolith", backpressure.Stats{
		Depth: 12, Capacity: 100, HighWatermark: 80, DroppedSamples: 7, ShedEvents: 2, BackpressureEvents: 5,
	})
	body := buf.String()
	for _, want := range []string{
		`meridian_dropped_samples_total{node="node-1",role="monolith"} 7`,
		`meridian_ingest_shed_events_total{node="node-1",role="monolith"} 2`,
		`meridian_ingest_backpressure_events_total{node="node-1",role="monolith"} 5`,
		`meridian_ingest_queue_depth{node="node-1",role="monolith"} 12`,
		`meridian_ingest_queue_capacity{node="node-1",role="monolith"} 100`,
		`meridian_ingest_queue_high_watermark{node="node-1",role="monolith"} 80`,
		"# TYPE meridian_dropped_samples_total counter",
		"# TYPE meridian_ingest_queue_depth gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue metrics missing %q\n%s", want, body)
		}
	}
}

func TestWriteAdmissionMetricsFamilies(t *testing.T) {
	var buf bytes.Buffer
	WriteAdmissionMetrics(&buf, "node-1", "monolith", backpressure.ShaperStats{
		TotalAdmitted: 30, TotalDropped: 12,
		Classes: []backpressure.ClassStat{
			{Name: "high", Admitted: 20, DroppedPriority: 0, DroppedFairShare: 1},
			{Name: "default", Admitted: 10, DroppedPriority: 8, DroppedFairShare: 3},
		},
		BucketDrops: []int64{5, 0, 7},
	})
	body := buf.String()
	for _, want := range []string{
		`meridian_admission_admitted_samples_total{node="node-1",role="monolith",class="high"} 20`,
		`meridian_admission_dropped_samples_total{node="node-1",role="monolith",class="default",reason="priority"} 8`,
		`meridian_admission_dropped_samples_total{node="node-1",role="monolith",class="default",reason="fairshare"} 3`,
		`meridian_admission_series_bucket_dropped_total{node="node-1",role="monolith",bucket="2"} 7`,
		"# TYPE meridian_admission_admitted_samples_total counter",
		"# TYPE meridian_admission_dropped_samples_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("admission metrics missing %q\n%s", want, body)
		}
	}
}

// TestWriteAdmissionMetricsDisabledEmpty confirms a disabled (empty) snapshot emits
// nothing, so the default uniform-shedding scrape is byte-for-byte unchanged.
func TestWriteAdmissionMetricsDisabledEmpty(t *testing.T) {
	var buf bytes.Buffer
	WriteAdmissionMetrics(&buf, "node-1", "monolith", backpressure.ShaperStats{})
	if buf.Len() != 0 {
		t.Fatalf("disabled admission emitted %d bytes, want 0:\n%s", buf.Len(), buf.String())
	}
}

func TestWriteHandoffMetricsFamilies(t *testing.T) {
	var buf bytes.Buffer
	WriteHandoffMetrics(&buf, "ingestor-1", "ingestor", HandoffStats{
		PendingSamples: 1200, PendingHints: 3, ReplayedSamples: 9000, DroppedSamples: 42,
	})
	body := buf.String()
	for _, want := range []string{
		`meridian_handoff_pending_samples{node="ingestor-1",role="ingestor"} 1200`,
		`meridian_handoff_pending_hints{node="ingestor-1",role="ingestor"} 3`,
		`meridian_handoff_replayed_samples_total{node="ingestor-1",role="ingestor"} 9000`,
		`meridian_handoff_dropped_samples_total{node="ingestor-1",role="ingestor"} 42`,
		"# TYPE meridian_handoff_pending_samples gauge",
		"# TYPE meridian_handoff_replayed_samples_total counter",
		"# TYPE meridian_handoff_dropped_samples_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("handoff metrics missing %q\n%s", want, body)
		}
	}
}

// TestMonolithMetricsAndStatsIncludeQueue proves the wired ingest stats source
// surfaces the bounded-queue families on /metrics and the fields on /api/v1/stats.
func TestMonolithMetricsAndStatsIncludeQueue(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(dir, storage.TSDBOptions{
		WALDir: dir + "/wal", BlockDir: dir + "/blocks", FlushInterval: time.Hour, RateSampleInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := NewHTTPServer(db, "mono-1", nil)
	s.SetIngestStatsSource(func() backpressure.Stats {
		return backpressure.Stats{Depth: 3, Capacity: 50, HighWatermark: 40, DroppedSamples: 9}
	})

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{
		`meridian_dropped_samples_total{node="mono-1",role="monolith"} 9`,
		`meridian_ingest_queue_depth{node="mono-1",role="monolith"} 3`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("monolith /metrics missing %q", want)
		}
	}

	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/stats", nil))
	for _, want := range []string{
		`"ingest_queue_depth":3`,
		`"ingest_queue_capacity":50`,
		`"dropped_samples":9`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("/api/v1/stats missing %q\n%s", want, rec.Body.String())
		}
	}
}

func TestMonolithPromMetricsHandler(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(dir, storage.TSDBOptions{
		WALDir:             dir + "/wal",
		BlockDir:           dir + "/blocks",
		FlushInterval:      time.Hour,
		RateSampleInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := NewHTTPServer(db, "mono-1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != 200 {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	// The refactored handler still emits storage, latency and ws families.
	for _, want := range []string{
		"meridian_samples_ingested_total",
		"meridian_query_latency_seconds",
		"meridian_ws_clients",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("monolith /metrics missing %q", want)
		}
	}
}
