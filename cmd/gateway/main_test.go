package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/server"
)

func TestGatewayMetricsRouteWired(t *testing.T) {
	gw := &gatewayServer{nodeID: "gateway-test", startTime: time.Now(), wsHub: server.NewWebSocketHub()}
	mux := http.NewServeMux()
	gw.registerRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`role="gateway"`, "meridian_ws_clients", "meridian_up"} {
		if !strings.Contains(body, want) {
			t.Errorf("gateway /metrics missing %q", want)
		}
	}
}
