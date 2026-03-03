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
	"github.com/meridiandb/meridian/internal/storage"
)

// stalledWriter is a service.Writer whose Write blocks until released, used to
// saturate the storage accept pool so it sheds.
type stalledWriter struct {
	open chan struct{}
	once sync.Once
}

func (s *stalledWriter) Write(ctx context.Context, req service.WriteRequest) (*service.WriteResponse, error) {
	<-s.open
	return &service.WriteResponse{SamplesIngested: 1}, nil
}

func (s *stalledWriter) release() { s.once.Do(func() { close(s.open) }) }

func TestStorageMetricsRouteWired(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(dir, storage.TSDBOptions{
		WALDir:             dir + "/wal",
		BlockDir:           dir + "/blocks",
		FlushInterval:      time.Hour,
		RateSampleInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := &storageServer{db: db, nodeID: "storage-test", startTime: time.Now()}
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("/metrics content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"meridian_out_of_order_samples_total",
		"meridian_samples_ingested_total",
		`role="storage"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("storage /metrics missing %q", want)
		}
	}
}

// TestStorageWriteShedReturns429 proves the storage accept queue NACKs with 429
// when its bounded pool sheds under a stalled downstream (a stand-in for a slow
// WAL fsync), pushing backpressure upstream to the quorum write.
func TestStorageWriteShedReturns429(t *testing.T) {
	w := &stalledWriter{open: make(chan struct{})}
	pool := service.NewWritePool(w, service.PoolOptions{Capacity: 1, HighWatermark: 1, BlockDeadline: 20 * time.Millisecond, Workers: 1})
	defer func() {
		w.release()
		pool.Close()
	}()
	s := &storageServer{nodeID: "storage-test", pool: pool, startTime: time.Now()}

	body := func() []byte {
		req := service.WriteRequest{TimeSeries: []service.TimeSeries{{Name: "m", Samples: []service.Sample{{TimestampMs: 1000, Value: 1}}}}}
		b, _ := json.Marshal(req)
		return b
	}

	const burst = 16
	codes := make(chan int, burst)
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			s.handleWrite(rec, httptest.NewRequest("POST", "/api/internal/write", bytes.NewReader(body())))
			codes <- rec.Code
		}()
	}
	time.Sleep(150 * time.Millisecond)
	w.release()
	wg.Wait()
	close(codes)

	var n429 int
	for c := range codes {
		if c == http.StatusTooManyRequests {
			n429++
		}
	}
	if n429 == 0 {
		t.Fatal("expected at least one 429 from the storage accept queue under overload")
	}
	if pool.Stats().DroppedSamples == 0 {
		t.Error("expected the storage pool to record dropped samples")
	}
}
