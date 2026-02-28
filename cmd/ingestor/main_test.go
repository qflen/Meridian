package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIngestorMetricsRouteWired(t *testing.T) {
	s := &ingestorServer{nodeID: "ingestor-test", startTime: time.Now()}
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `role="ingestor"`) || !strings.Contains(body, "meridian_up") {
		t.Errorf("ingestor /metrics body = %q", body)
	}
}
