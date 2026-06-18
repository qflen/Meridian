package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/meridiandb/meridian/internal/service"
)

func addrOf(s *httptest.Server) string {
	return strings.TrimPrefix(s.URL, "http://")
}

func sampleWrite() service.WriteRequest {
	return service.WriteRequest{
		TimeSeries: []service.TimeSeries{{
			Name: "cpu",
			Samples: []service.Sample{
				{TimestampMs: 1, Value: 1},
				{TimestampMs: 2, Value: 2},
				{TimestampMs: 3, Value: 3},
			},
		}},
	}
}

func TestWriteErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A storage node erroring out: previously decoded into a zero struct and
		// reported as success-with-0-ingested.
		http.Error(w, "internal boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := service.NewStorageClient([]string{addrOf(srv)})
	if _, err := c.Write(context.Background(), sampleWrite()); err == nil {
		t.Fatal("Write must return an error when the storage node returns 500, not silent success")
	}
}

func TestWriteSucceedsOn200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// SamplesIngested is the count of logical samples that met write quorum, computed
	// client-side — not the (replication-multiplied) sum of per-node responses. The
	// single-node client writes the one series' 3 samples at quorum.
	c := service.NewStorageClient([]string{addrOf(srv)})
	resp, err := c.Write(context.Background(), sampleWrite())
	if err != nil {
		t.Fatalf("Write should succeed on 200: %v", err)
	}
	if resp.SamplesIngested != 3 {
		t.Fatalf("expected 3 ingested, got %d", resp.SamplesIngested)
	}
}

func TestHealthCheckFalseOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, ok := service.HealthCheck(addrOf(srv)); ok {
		t.Fatal("HealthCheck should report not-ok for a non-200 response")
	}
}

func TestFetchStatsSkipsErroringNode(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(service.StatsResponse{TotalSamples: 42, TotalSeries: 2})
	}))
	defer good.Close()

	c := service.NewStorageClient([]string{addrOf(bad), addrOf(good)})
	agg, err := c.FetchStats(context.Background())
	if err != nil {
		t.Fatalf("FetchStats: %v", err)
	}
	// The 500 node must contribute nothing (no zero-struct merged); only the good
	// node's real counts appear.
	if agg.TotalSamples != 42 || agg.TotalSeries != 2 {
		t.Fatalf("expected only the healthy node's stats (42/2), got %d/%d", agg.TotalSamples, agg.TotalSeries)
	}
}
