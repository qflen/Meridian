package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/service"
)

// stalledWriter is a service.Writer whose Write blocks until released, used to
// stall the ingestor's pool so the bounded queue saturates and sheds.
type stalledWriter struct {
	open chan struct{}
	once sync.Once
}

func (s *stalledWriter) Write(ctx context.Context, req service.WriteRequest) (*service.WriteResponse, error) {
	<-s.open
	return &service.WriteResponse{SamplesIngested: 1}, nil
}

func (s *stalledWriter) release() { s.once.Do(func() { close(s.open) }) }

func TestIngestorMetricsRouteWired(t *testing.T) {
	s := &ingestorServer{nodeID: "ingestor-test", startTime: time.Now()}
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `role="ingestor"`) || !strings.Contains(body, "meridian_up") {
		t.Errorf("ingestor /metrics body = %q", body)
	}
}

// TestIngestorHTTPShedReturns429 proves the HTTP ingest handler NACKs with 429 +
// Retry-After when the bounded queue sheds under a stalled downstream, and exposes
// the drop counter on /metrics.
func TestIngestorHTTPShedReturns429(t *testing.T) {
	w := &stalledWriter{open: make(chan struct{})}
	pool := service.NewWritePool(w, service.PoolOptions{Capacity: 1, HighWatermark: 1, BlockDeadline: 20 * time.Millisecond, Workers: 1})
	defer func() {
		w.release()
		pool.Close()
	}()
	s := &ingestorServer{nodeID: "ingestor-test", pool: pool, startTime: time.Now()}

	body := func() []byte {
		req := service.WriteRequest{TimeSeries: []service.TimeSeries{{Name: "m", Samples: []service.Sample{{TimestampMs: 1000, Value: 1}}}}}
		b, _ := json.Marshal(req)
		return b
	}

	// Fire a burst concurrently; the stalled worker means most are shed → 429.
	const burst = 16
	codes := make(chan int, burst)
	retryAfter := make(chan string, burst)
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			s.handleHTTPIngest(rec, httptest.NewRequest("POST", "/api/internal/ingest", bytes.NewReader(body())))
			codes <- rec.Code
			retryAfter <- rec.Header().Get("Retry-After")
		}()
	}
	// Let shedding happen, then release so the accepted requests finish.
	time.Sleep(150 * time.Millisecond)
	w.release()
	wg.Wait()
	close(codes)
	close(retryAfter)

	var n429 int
	for c := range codes {
		if c == http.StatusTooManyRequests {
			n429++
		}
	}
	if n429 == 0 {
		t.Fatal("expected at least one 429 under overload")
	}
	var sawRetryAfter bool
	for ra := range retryAfter {
		if ra != "" {
			sawRetryAfter = true
		}
	}
	if !sawRetryAfter {
		t.Error("429 responses must carry a Retry-After header")
	}
	if pool.Stats().DroppedSamples == 0 {
		t.Error("expected the pool to record dropped samples")
	}

	// /metrics surfaces the drop counter.
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `meridian_dropped_samples_total{node="ingestor-test",role="ingestor"}`) {
		t.Error("ingestor /metrics missing meridian_dropped_samples_total")
	}
}
