package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

func TestStorageMetricsRouteWired(t *testing.T) {
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

	s := &storageServer{db: db, nodeID: "storage-test", startTime: time.Now()}
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("/metrics content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"meridian_out_of_order_samples_total",
		"meridian_samples_ingested_total",
		`role="storage"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("storage /metrics missing %q", want)
		}
	}
}
