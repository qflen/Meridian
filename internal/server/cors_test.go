package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func corsResp(allowed []string, method, origin string) *httptest.ResponseRecorder {
	h := CORSMiddleware(allowed, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, "http://server.local/api/v1/query", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCORSDefaultAllowsLocalhostOnly(t *testing.T) {
	// A localhost dev origin (e.g. the Vite dev server) is echoed, never "*".
	rec := corsResp(nil, "POST", "http://localhost:5173")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("localhost origin: ACAO = %q, want it echoed", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Errorf("policy must never emit a wildcard ACAO")
	}
	// A foreign origin gets no CORS header at all (POST included).
	rec = corsResp(nil, "POST", "http://evil.example.com")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("foreign origin: ACAO = %q, want none", got)
	}
}

func TestCORSExplicitAllowlist(t *testing.T) {
	allowed := []string{"https://app.example.com"}
	if got := corsResp(allowed, "GET", "https://app.example.com").Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("configured origin: ACAO = %q, want echoed", got)
	}
	// With an explicit list, even localhost is denied unless listed.
	if got := corsResp(allowed, "GET", "http://localhost:5173").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unlisted origin: ACAO = %q, want none", got)
	}
}

func TestCORSWildcardOptIn(t *testing.T) {
	if got := corsResp([]string{"*"}, "GET", "http://anywhere.example").Header().Get("Access-Control-Allow-Origin"); got != "http://anywhere.example" {
		t.Errorf("wildcard: ACAO = %q, want origin echoed", got)
	}
}

func TestCORSPreflightAllowed(t *testing.T) {
	rec := corsResp(nil, "OPTIONS", "http://localhost:5173")
	if rec.Code != http.StatusOK {
		t.Errorf("preflight status = %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("preflight ACAO = %q", got)
	}
}
