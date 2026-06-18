package ingestion

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/meridiandb/meridian/internal/ingestion/proto"
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

// TestServerWriteNACKsUnderOverload proves Server.Write surfaces shedding as a NACK:
// with the drain stalled and a tiny queue, a write of many samples reports a non-zero
// Shed and SamplesIngested excludes the shed samples.
func TestServerWriteNACKsUnderOverload(t *testing.T) {
	sink := newGateSink()
	bw := newBatchWriterSink(sink, 1, time.Hour, QueueOptions{Capacity: 2, HighWatermark: 2, BlockDeadline: 10 * time.Millisecond})
	srv := newServer(nil, bw)
	defer func() {
		sink.release()
		bw.Close()
	}()

	const offered = 20
	samples := make([]pb.Sample, offered)
	for i := range samples {
		samples[i] = pb.Sample{TimestampMs: int64(i+1) * 1000, Value: float64(i)}
	}
	resp, err := srv.Write(context.Background(), &pb.WriteRequest{
		TimeSeries: []pb.TimeSeries{{Name: "m", Samples: samples}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Shed == 0 {
		t.Fatal("expected the server to report shed samples (NACK) under overload")
	}
	if resp.SamplesIngested != offered-resp.Shed {
		t.Fatalf("SamplesIngested(%d) should equal offered(%d) - Shed(%d)", resp.SamplesIngested, offered, resp.Shed)
	}
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
