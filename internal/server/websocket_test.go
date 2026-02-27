package server

import (
	"sync/atomic"
	"testing"
	"time"
)

func waitForClientCount(t *testing.T, h *WebSocketHub, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.ClientCount() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("ClientCount did not reach %d (got %d)", want, h.ClientCount())
}

// registerTestClient adds a buffered, conn-less client to the hub for white-box
// broadcast tests.
func registerTestClient(t *testing.T, h *WebSocketHub, bufSize int) *wsClient {
	t.Helper()
	c := &wsClient{hub: h, send: make(chan []byte, bufSize)}
	h.register <- c
	return c
}

// countingMarshaler counts how many times it is serialized.
type countingMarshaler struct{ n *int32 }

func (c countingMarshaler) MarshalJSON() ([]byte, error) {
	atomic.AddInt32(c.n, 1)
	return []byte(`{"ok":1}`), nil
}

func TestBroadcastDisconnectsSlowClient(t *testing.T) {
	h := NewWebSocketHub()
	go h.Run()

	// A client that never drains its buffer.
	registerTestClient(t, h, 4)
	waitForClientCount(t, h, 1)

	// Fill the buffer, then keep broadcasting; once maxClientDrops full-buffer drops
	// accumulate the hub must disconnect and remove the client.
	payload := map[string]any{"type": "stats", "v": 1}
	for i := 0; i < 4+maxClientDrops+5; i++ {
		h.BroadcastMetrics(payload)
	}

	waitForClientCount(t, h, 0)
}

func TestBroadcastSurvivesActiveClient(t *testing.T) {
	h := NewWebSocketHub()
	go h.Run()

	c := registerTestClient(t, h, 4)
	waitForClientCount(t, h, 1)

	// A client that keeps draining must never be dropped, even across many broadcasts.
	done := make(chan struct{})
	go func() {
		for range c.send {
		}
		close(done)
	}()
	for i := 0; i < maxClientDrops*4; i++ {
		h.BroadcastMetrics(map[string]any{"type": "stats", "v": i})
		time.Sleep(50 * time.Microsecond)
	}
	if got := h.ClientCount(); got != 1 {
		t.Fatalf("active client was dropped: ClientCount=%d", got)
	}
}

func TestBroadcastMarshalsOncePerCall(t *testing.T) {
	h := NewWebSocketHub()
	go h.Run()

	// Three clients with room in their buffers.
	for i := 0; i < 3; i++ {
		registerTestClient(t, h, 8)
	}
	waitForClientCount(t, h, 3)

	var n int32
	h.BroadcastMetrics(countingMarshaler{&n})

	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("payload marshaled %d times for one broadcast; expected exactly 1 (no per-client re-marshal)", got)
	}
}
