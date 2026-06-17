package ingestion

import (
	"bytes"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	opts := storage.DefaultTSDBOptions()
	opts.WALDir = filepath.Join(dir, "wal")
	opts.BlockDir = filepath.Join(dir, "blocks")
	opts.FlushInterval = time.Hour
	db, err := storage.Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	srv := NewServer(db, 100, time.Hour)
	// Tight bounds for the test.
	srv.readTimeout = 200 * time.Millisecond
	srv.maxMessageBytes = 1024
	if err := srv.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	return srv, srv.listener.Addr().String()
}

// TestIngestionStalledConnectionClosed verifies a client that opens a connection and
// then stalls is disconnected by the read deadline rather than pinning a goroutine
// for the process lifetime.
func TestIngestionStalledConnectionClosed(t *testing.T) {
	_, addr := newTestServer(t)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Send nothing; the server should close us out after its read deadline.
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = conn.Read(make([]byte, 1))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the server to close the stalled connection")
	}
	if elapsed > time.Second {
		t.Fatalf("connection was not closed promptly (%v); the handler likely hung", elapsed)
	}
}

// TestIngestionOversizedMessageClosed verifies that a message exceeding the size cap
// closes the connection instead of being buffered to exhaustion.
func TestIngestionOversizedMessageClosed(t *testing.T) {
	_, addr := newTestServer(t)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// An incomplete JSON document far larger than maxMessageBytes (1 KiB).
	payload := append([]byte(`{"TimeSeries":[`), bytes.Repeat([]byte(" "), 8192)...)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = conn.Read(make([]byte, 1))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the server to close the oversized connection")
	}
	if elapsed > time.Second {
		t.Fatalf("oversized connection was not closed promptly (%v)", elapsed)
	}
}
