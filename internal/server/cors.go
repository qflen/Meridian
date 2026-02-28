package server

import (
	"net/http"
	"net/url"
)

// CORSMiddleware wraps next with an origin-checked CORS policy instead of a blanket
// "Access-Control-Allow-Origin: *". The allowed set is matched against the request's
// Origin and, when permitted, that exact origin is echoed back (so the policy also
// works for credentialed requests). Disallowed cross-origin requests receive no CORS
// headers and are blocked by the browser.
//
// allowed semantics:
//   - empty   → secure default: any localhost / 127.0.0.1 / [::1] origin (the local
//     dashboard and its dev server), nothing else.
//   - ["*"]   → allow all origins (opt-in; intended only for trusted networks).
//   - else    → exact-match against the configured origins.
func CORSMiddleware(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if originAllowed(allowed, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func originAllowed(allowed []string, origin string) bool {
	if origin == "" {
		return false // same-origin request; no CORS headers needed
	}
	if len(allowed) == 0 {
		return isLocalhostOrigin(origin)
	}
	for _, a := range allowed {
		if a == "*" || a == origin {
			return true
		}
	}
	return false
}

func isLocalhostOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}
