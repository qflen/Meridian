package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompactorMetricsRouteWired(t *testing.T) {
	c := &compactorServer{nodeID: "compactor-test", startTime: time.Now()}
	mux := http.NewServeMux()
	c.registerRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `role="compactor"`) || !strings.Contains(body, "meridian_up") {
		t.Errorf("compactor /metrics body = %q", body)
	}
}
