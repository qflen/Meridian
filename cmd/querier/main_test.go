package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQuerierMetricsRouteWired(t *testing.T) {
	s := &querierServer{nodeID: "querier-test", startTime: time.Now()}
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `role="querier"`) || !strings.Contains(body, "meridian_up") {
		t.Errorf("querier /metrics body = %q", body)
	}
}
