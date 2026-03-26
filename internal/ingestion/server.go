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

// Server is the gRPC-style ingestion server.
// Since we avoid full protobuf code generation, this uses a simple JSON-over-TCP
// protocol that mirrors the protobuf message structure for the demo.
type Server struct {
	db       *storage.TSDB
	batch    *BatchWriter
	listener net.Listener
	done     chan struct{}
}

// NewServer creates a new ingestion server.
func NewServer(db *storage.TSDB, batchSize int, flushInterval time.Duration) *Server {
	return &Server{
		db:    db,
		batch: NewBatchWriter(db, batchSize, flushInterval),
		done:  make(chan struct{}),
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

// Write handles a single write request (used for direct invocation).
func (s *Server) Write(_ context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
	var count int64
	for _, ts := range req.TimeSeries {
		if err := ValidateMetricName(ts.Name); err != nil {
			continue
		}
		labels := make(map[string]string, len(ts.Labels))
		for _, l := range ts.Labels {
			labels[l.Name] = l.Value
		}
		for _, sample := range ts.Samples {
			s.batch.Add(ts.Name, labels, sample.TimestampMs, sample.Value)
			count++
		}
	}
	return &pb.WriteResponse{SamplesIngested: count}, nil
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
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		var req pb.WriteRequest
		if err := decoder.Decode(&req); err != nil {
			if err != io.EOF {
				log.Printf("Ingestion decode error: %v", err)
			}
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
