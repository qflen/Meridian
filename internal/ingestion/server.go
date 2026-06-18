package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	pb "github.com/meridiandb/meridian/internal/ingestion/proto"
	"github.com/meridiandb/meridian/internal/storage"
)

const (
	// defaultMaxMessageBytes bounds a single decoded WriteRequest so a huge or
	// never-ending message cannot drive the server to OOM.
	defaultMaxMessageBytes = 16 << 20 // 16 MiB
	// defaultReadTimeout closes a connection that stalls mid-message, so a slow or
	// dead client cannot leak a goroutine for the process lifetime.
	defaultReadTimeout = 60 * time.Second
	// defaultMaxConns bounds concurrent connections; past it, accept applies
	// backpressure (the kernel backlog queues new connections) until a slot frees.
	defaultMaxConns = 4096
)

// Server is the gRPC-style ingestion server.
// Since we avoid full protobuf code generation, this uses a simple JSON-over-TCP
// protocol that mirrors the protobuf message structure for the demo.
type Server struct {
	db       *storage.TSDB
	batch    *BatchWriter
	listener net.Listener
	done     chan struct{}

	maxMessageBytes int64
	readTimeout     time.Duration
	connSem         chan struct{}
}

// NewServer creates a new ingestion server with a default-sized bounded queue.
func NewServer(db *storage.TSDB, batchSize int, flushInterval time.Duration) *Server {
	return newServer(db, NewBatchWriter(db, batchSize, flushInterval))
}

// NewServerWithQueue creates an ingestion server whose bounded queue is sized from
// opts (wired from ingestion config by the serve command).
func NewServerWithQueue(db *storage.TSDB, batchSize int, flushInterval time.Duration, opts QueueOptions) *Server {
	return newServer(db, NewBatchWriterWithQueue(db, batchSize, flushInterval, opts))
}

func newServer(db *storage.TSDB, batch *BatchWriter) *Server {
	return &Server{
		db:              db,
		batch:           batch,
		done:            make(chan struct{}),
		maxMessageBytes: defaultMaxMessageBytes,
		readTimeout:     defaultReadTimeout,
		connSem:         make(chan struct{}, defaultMaxConns),
	}
}

// Start begins listening for ingestion connections.
func (s *Server) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.listener = ln
	log.Printf("Ingestion server listening on %s", addr)

	go s.acceptLoop()
	return nil
}

// Stop gracefully shuts down the ingestion server.
func (s *Server) Stop() {
	close(s.done)
	if s.listener != nil {
		s.listener.Close()
	}
	s.batch.Close()
}

// BatchWriter returns the underlying batch writer for direct access.
func (s *Server) BatchWriter() *BatchWriter {
	return s.batch
}

// Write handles a single write request (used for direct invocation). Series with an
// invalid name or label (including oversized names/labels) are skipped. Valid
// samples are offered to the bounded ingest queue; any shed under overload are
// reported back so the producer is NACKed (SamplesIngested excludes them).
func (s *Server) Write(_ context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
	var samples []storage.IngestSample
	for _, ts := range req.TimeSeries {
		if err := ValidateMetricName(ts.Name); err != nil {
			continue
		}
		labels := make(map[string]string, len(ts.Labels))
		valid := true
		for _, l := range ts.Labels {
			if err := ValidateLabel(l.Name, l.Value); err != nil {
				valid = false
				break
			}
			labels[l.Name] = l.Value
		}
		if !valid {
			continue
		}
		for _, sample := range ts.Samples {
			samples = append(samples, storage.IngestSample{
				Name:      ts.Name,
				Labels:    labels,
				Timestamp: sample.TimestampMs,
				Value:     sample.Value,
			})
		}
	}

	res := s.batch.AddBatch(samples)
	accepted := int64(len(samples)) - res.Shed
	if accepted < 0 {
		accepted = 0
	}
	return &pb.WriteResponse{
		SamplesIngested: accepted,
		Shed:            res.Shed,
		Throttled:       res.Throttled,
	}, nil
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				log.Printf("Ingestion accept error: %v", err)
				continue
			}
		}
		// Bound concurrent connections: block until a slot frees (backpressure) or
		// the server is shutting down, rather than spawning unbounded goroutines.
		select {
		case s.connSem <- struct{}{}:
		case <-s.done:
			conn.Close()
			return
		}
		go func(c net.Conn) {
			defer func() { <-s.connSem }()
			s.handleConn(c)
		}(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	// Cap the bytes any single Decode can pull so an oversized/never-ending message
	// can't exhaust memory. N is reset before each message.
	limited := &io.LimitedReader{R: conn, N: s.maxMessageBytes}
	decoder := json.NewDecoder(limited)
	encoder := json.NewEncoder(conn)

	for {
		// A fresh read deadline per message: a client that stalls mid-message is
		// disconnected instead of pinning a goroutine forever.
		if err := conn.SetReadDeadline(time.Now().Add(s.readTimeout)); err != nil {
			return
		}
		limited.N = s.maxMessageBytes

		var req pb.WriteRequest
		if err := decoder.Decode(&req); err != nil {
			if err != io.EOF {
				log.Printf("Ingestion decode error: %v", err)
			}
			return
		}
		if limited.N <= 0 {
			log.Printf("Ingestion: message exceeded %d bytes, closing connection", s.maxMessageBytes)
			return
		}

		resp, err := s.Write(context.Background(), &req)
		if err != nil {
			log.Printf("Ingestion write error: %v", err)
			continue
		}

		if err := encoder.Encode(resp); err != nil {
			return
		}
	}
}
