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

func newAEServer(t *testing.T) (*storageServer, *http.ServeMux) {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(dir, storage.TSDBOptions{
		WALDir: dir + "/wal", BlockDir: dir + "/blocks", FlushInterval: time.Hour, RateSampleInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &storageServer{db: db, nodeID: "storage-test", startTime: time.Now()}
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	return s, mux
}

func postAE(t *testing.T, mux *http.ServeMux, method, path string, reqBody, respOut any) int {
	t.Helper()
	b, _ := json.Marshal(reqBody)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewReader(b)))
	if rec.Code == http.StatusOK && respOut != nil {
		if err := json.NewDecoder(rec.Body).Decode(respOut); err != nil {
			t.Fatalf("decode %s response: %v", path, err)
		}
	}
	return rec.Code
}

// TestAntiEntropyEndpointsRoundTrip drives the real storage routes (ADR-030): digest the
// source, export its range, backfill that into an empty node through the actual backfill
// route, and confirm the destination's digest now equals the source's — the full transfer
// path the coordinator uses, exercised end-to-end against the binary's handlers.
func TestAntiEntropyEndpointsRoundTrip(t *testing.T) {
	src, srcMux := newAEServer(t)
	_, dstMux := newAEServer(t)

	if _, err := src.db.Backfill([]storage.IngestSample{
		{Name: "cpu", Labels: map[string]string{"h": "a"}, Timestamp: 100, Value: 1},
		{Name: "cpu", Labels: map[string]string{"h": "a"}, Timestamp: 1500, Value: 2},
		{Name: "mem", Timestamp: 200, Value: 7},
	}); err != nil {
		t.Fatal(err)
	}

	whole := [][2]uint64{{0, 0}} // Lo == Hi ⇒ the whole ring
	dreq := service.DigestRequest{Ranges: whole, Start: 0, End: 1 << 62, Window: 1000}

	var srcDigest storage.MerkleDigest
	if code := postAE(t, srcMux, "POST", "/api/internal/antientropy/digest", dreq, &srcDigest); code != http.StatusOK {
		t.Fatalf("digest status = %d", code)
	}
	if srcDigest.Root == "" || len(srcDigest.Leaves) == 0 {
		t.Fatalf("expected a non-empty digest, got %+v", srcDigest)
	}

	var export service.WriteRequest
	if code := postAE(t, srcMux, "POST", "/api/internal/antientropy/range",
		service.RangeRequest{Ranges: whole, Start: 0, End: 1 << 62}, &export); code != http.StatusOK {
		t.Fatalf("range status = %d", code)
	}
	if len(export.TimeSeries) != 2 {
		t.Fatalf("expected 2 series exported, got %d", len(export.TimeSeries))
	}
	for _, ts := range export.TimeSeries {
		for _, l := range ts.Labels {
			if l.Name == "__name__" {
				t.Fatalf("export must strip the synthetic __name__ label, got it on %q", ts.Name)
			}
		}
	}

	if code := postAE(t, dstMux, "POST", "/api/internal/backfill", export, nil); code != http.StatusOK {
		t.Fatalf("backfill status = %d", code)
	}

	var dstDigest storage.MerkleDigest
	postAE(t, dstMux, "POST", "/api/internal/antientropy/digest", dreq, &dstDigest)
	if dstDigest.Root != srcDigest.Root {
		t.Fatalf("destination root %s != source root %s after transfer", dstDigest.Root, srcDigest.Root)
	}

	// Guard rails: wrong method and a zero window are rejected.
	if code := postAE(t, srcMux, "GET", "/api/internal/antientropy/digest", dreq, nil); code != http.StatusMethodNotAllowed {
		t.Errorf("GET digest = %d, want 405", code)
	}
	bad := service.DigestRequest{Ranges: whole, Start: 0, End: 1 << 62, Window: 0}
	if code := postAE(t, srcMux, "POST", "/api/internal/antientropy/digest", bad, nil); code != http.StatusBadRequest {
		t.Errorf("zero-window digest = %d, want 400", code)
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
