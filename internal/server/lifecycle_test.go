package server

import "testing"

func TestStopWithoutStartDoesNotPanic(t *testing.T) {
	s := NewHTTPServer(nil, "node-1", nil)
	// Start was never called, so the underlying *http.Server is nil. Stop must be a
	// safe no-op rather than a nil dereference.
	s.Stop()
}
