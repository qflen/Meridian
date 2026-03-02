package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestDashboard lays out a dashboard dir under a fresh temp root and drops a
// sentinel file in the parent that must never be reachable through the handler.
func newTestDashboard(t *testing.T) (dashboardDir, secretBody string) {
	t.Helper()
	root := t.TempDir()
	dashboardDir = filepath.Join(root, "dist")
	if err := os.MkdirAll(filepath.Join(dashboardDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dashboardDir, "index.html"), []byte("<!doctype html><title>app</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dashboardDir, "assets", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	secretBody = "TOP-SECRET-OUTSIDE-ROOT"
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte(secretBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return dashboardDir, secretBody
}

func serveStatic(h http.Handler, rawpath string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.URL.Path = rawpath // decoded path, exactly as net/http hands it to a handler
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStaticHandlerServesShellAndAssets(t *testing.T) {
	dir, _ := newTestDashboard(t)
	h := newStaticHandler(dir)

	if rec := serveStatic(h, "/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>app</title>") {
		t.Fatalf("GET /: code=%d body=%q", rec.Code, rec.Body.String())
	}
	// Unknown client-side route falls back to the SPA shell.
	if rec := serveStatic(h, "/dashboard/some/route"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>app</title>") {
		t.Fatalf("GET /dashboard/some/route: code=%d body=%q", rec.Code, rec.Body.String())
	}
	// A real file inside the dir is served as itself.
	if rec := serveStatic(h, "/assets/app.js"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "console.log(1)") {
		t.Fatalf("GET /assets/app.js: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestStaticHandlerRejectsTraversal(t *testing.T) {
	dir, secret := newTestDashboard(t)
	h := newStaticHandler(dir)

	// Literal "../" traversal, including the deep form a client sends with
	// curl --path-as-is. Must be a 4xx and must never leak the sentinel file.
	for _, p := range []string{
		"/../secret.txt",
		"/../../../../etc/passwd",
		"/assets/../../secret.txt",
		"/foo/../../secret.txt",
	} {
		rec := serveStatic(h, p)
		if rec.Code < 400 || rec.Code >= 500 {
			t.Errorf("GET %s: expected 4xx, got %d", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("GET %s: leaked sentinel file content", p)
		}
	}
}

func TestStaticHandlerRejectsEncodedTraversal(t *testing.T) {
	dir, secret := newTestDashboard(t)
	h := newStaticHandler(dir)

	// net/http percent-decodes the path before the handler runs; emulate that by
	// parsing the encoded target and handing the decoded URL to the handler.
	u, err := url.Parse("http://example.com/%2e%2e/%2e%2e/secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.Path, "..") {
		t.Fatalf("precondition: decoded path %q should contain '..'", u.Path)
	}
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.URL = u
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("encoded traversal: expected 4xx, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("encoded traversal leaked sentinel file content")
	}
}

// TestServerHandlerRejectsTraversalAtBoundary exercises the full server handler
// chain (CORS → GuardTraversal → mux). The boundary guard must return 400 before
// http.ServeMux can path-clean and 301-redirect the traversal.
func TestServerHandlerRejectsTraversalAtBoundary(t *testing.T) {
	s := NewHTTPServer(nil, "node-1", nil)
	h := s.handler()
	for _, p := range []string{"/../../../../etc/passwd", "/assets/../../secret", "/api/v1/../../etc/passwd"} {
		req := httptest.NewRequest("GET", "http://example.com/", nil)
		req.URL.Path = p
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s through full handler: status=%d, want 400", p, rec.Code)
		}
	}
}
