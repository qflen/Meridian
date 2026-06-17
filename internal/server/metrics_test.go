package server

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
