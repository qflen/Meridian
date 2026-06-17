package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/query"
	"github.com/meridiandb/meridian/internal/storage"
)

// panicDataSource panics on every query, exercising the handler's recover guard.
type panicDataSource struct{}

func (panicDataSource) Query(context.Context, []storage.LabelMatcher, int64, int64) (storage.SeriesSet, error) {
	panic("boom in data source")
}

// blockingDataSource blocks until the context is cancelled, then reports why.
type blockingDataSource struct{}

func (blockingDataSource) Query(ctx context.Context, _ []storage.LabelMatcher, _, _ int64) (storage.SeriesSet, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// emptyDataSource returns no data and no error.
type emptyDataSource struct{}

func (emptyDataSource) Query(context.Context, []storage.LabelMatcher, int64, int64) (storage.SeriesSet, error) {
	return nil, nil
}

func newQueryTestServer(ds query.DataSource) *HTTPServer {
	s := NewHTTPServer(nil, "test-node", nil)
	s.engine = query.NewEngine(ds)
	return s
}

func runQuery(s *HTTPServer, rawQuery string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/v1/query?"+rawQuery, nil)
	rec := httptest.NewRecorder()
	s.handleQuery(rec, req)
	return rec
}

func TestQueryHandlerRecoversPanic(t *testing.T) {
	s := newQueryTestServer(panicDataSource{})
	rec := runQuery(s, "q=up")
	if rec.Code != 500 {
		t.Fatalf("panic in query path: expected 500, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestQueryHandlerEnforcesTimeout(t *testing.T) {
	s := newQueryTestServer(blockingDataSource{})
	s.SetQueryTimeout(20 * time.Millisecond)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- runQuery(s, "q=up") }()

	select {
	case rec := <-done:
		if rec.Code != 504 {
			t.Fatalf("deadline-exceeding query: expected 504, got %d (body=%q)", rec.Code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("query handler did not return after its deadline; request was not cancelled")
	}
}

func TestQueryHandlerRejectsInvalidRange(t *testing.T) {
	s := newQueryTestServer(emptyDataSource{})
	rec := runQuery(s, "q=up&start=2000&end=1000")
	if rec.Code != 400 {
		t.Fatalf("end<start: expected 400, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestQueryHandlerRejectsMalformedParams(t *testing.T) {
	s := newQueryTestServer(emptyDataSource{})
	cases := []struct {
		name  string
		query string
	}{
		{"missing q", "q="},
		{"garbage start", "q=up&start=abc"},
		{"garbage end", "q=up&end=xyz"},
		{"garbage step", "q=up&step=notaduration"},
		{"unitless step", "q=up&step=5"},
	}
	for _, tc := range cases {
		if rec := runQuery(s, tc.query); rec.Code != 400 {
			t.Errorf("%s: expected 400, got %d (body=%q)", tc.name, rec.Code, rec.Body.String())
		}
	}
}

func TestQueryHandlerAcceptsValidRequest(t *testing.T) {
	s := newQueryTestServer(emptyDataSource{})
	// Absent step must be honored (engine auto-sizes); valid timestamps accepted.
	rec := runQuery(s, "q=up&start=1000&end=2000")
	if rec.Code != 200 {
		t.Fatalf("valid query: expected 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}
