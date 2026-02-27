package server

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// maxClientDrops is how many consecutive full-buffer drops a client may accumulate
// before the hub force-disconnects it. A client that has not drained a single
// broadcast in this many ticks is treated as dead and removed, so it cannot linger
// (and leak its read/write goroutines) for the duration of the write deadline.
const maxClientDrops = 64

// WebSocketHub manages all WebSocket connections and broadcasts messages.
type WebSocketHub struct {
	mu       sync.RWMutex
	clients  map[*wsClient]bool
	register chan *wsClient
	remove   chan *wsClient
}

type wsClient struct {
	hub   *WebSocketHub
	conn  *websocket.Conn
	send  chan []byte
	drops atomic.Int32 // consecutive full-buffer drops; reset on a successful send
}

// NewWebSocketHub creates a new hub for managing WebSocket connections.
func NewWebSocketHub() *WebSocketHub {
	return &WebSocketHub{
		clients:  make(map[*wsClient]bool),
		register: make(chan *wsClient),
		remove:   make(chan *wsClient),
	}
}

// Run starts the hub event loop.
func (h *WebSocketHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
		case client := <-h.remove:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				// Closing the conn unblocks a writePump stuck mid-write so a
				// force-disconnected slow client exits immediately. Close is
				// goroutine-safe and idempotent across the pumps' own defers.
				if client.conn != nil {
					client.conn.Close()
				}
			}
			h.mu.Unlock()
		}
	}
}

// BroadcastMetrics sends a message to every connected /ws/metrics client. The
// payload is marshaled exactly once per call and the identical bytes are handed to
// every client. A client whose send buffer stays full for maxClientDrops broadcasts
// in a row is force-disconnected so a stalled reader cannot leak goroutines.
func (h *WebSocketHub) BroadcastMetrics(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	var slow []*wsClient
	h.mu.RLock()
	for client := range h.clients {
		select {
		case client.send <- data:
			client.drops.Store(0)
		default:
			if client.drops.Add(1) >= maxClientDrops {
				slow = append(slow, client)
			}
		}
	}
	h.mu.RUnlock()

	// Disconnect stalled clients outside the read lock. Run closes client.send under
	// the write lock and removes the client in the same critical section, so this can
	// never race with the buffered sends above.
	for _, c := range slow {
		h.remove <- c
	}
}

// ClientCount returns the number of connected WebSocket clients.
func (h *WebSocketHub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// HandleWSUpgrade upgrades an HTTP connection to WebSocket and registers with the hub.
// Exported so other binaries (e.g., the gateway) can reuse the hub implementation.
func HandleWSUpgrade(hub *WebSocketHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	client := &wsClient{
		hub:  hub,
		conn: conn,
		send: make(chan []byte, 256),
	}
	hub.register <- client
	go client.writePump()
	go client.readPump()
}

func (s *HTTPServer) handleWSMetrics(w http.ResponseWriter, r *http.Request) {
	HandleWSUpgrade(s.wsHub, w, r)
}

func (c *wsClient) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *wsClient) readPump() {
	defer func() {
		c.hub.remove <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(512)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
	}
}
