package query

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestBucketQuantile checks the interpolation against hand-computed buckets:
// le=1→1, le=2→3, le=4→6, +Inf→6 (cumulative counts, total 6).
func TestBucketQuantile(t *testing.T) {
	buckets := []bucket{
		{upperBound: 1, count: 1},
		{upperBound: 2, count: 3},
		{upperBound: 4, count: 6},
		{upperBound: math.Inf(1), count: 6},
	}
	cases := []struct {
		phi  float64
		want float64
	}{
		{0.1, 0.6}, // rank 0.6 in first bucket: 1*(0.6/1)
		{0.5, 2.0}, // rank 3 at the le=2 boundary: 1 + (2-1)*(3-1)/(3-1)
		{1.0, 4.0}, // rank 6 reached at le=4
	}
	for _, c := range cases {
		got := bucketQuantile(c.phi, buckets)
		if math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("phi=%v: got %v, want %v", c.phi, got, c.want)
		}
	}
}

// TestHistogramQuantileEndToEnd drives histogram_quantile through the engine over
// real bucket series distinguished by le, and verifies the le/__name__ labels are
// dropped from the result.
func TestHistogramQuantileEndToEnd(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	const bkt = "http_request_duration_seconds_bucket"
	const ts = int64(1000)
	db.Ingest(bkt, map[string]string{"le": "1"}, ts, 1)
	db.Ingest(bkt, map[string]string{"le": "2"}, ts, 3)
	db.Ingest(bkt, map[string]string{"le": "4"}, ts, 6)
	db.Ingest(bkt, map[string]string{"le": "+Inf"}, ts, 6)
	engine := NewEngine(db)

	res, err := engine.Execute(context.Background(), "histogram_quantile(0.5, "+bkt+")", 0, 2000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 grouped series, got %d: %+v", len(res), res)
	}
	if len(res[0].Points) != 1 {
		t.Fatalf("expected 1 point, got %d", len(res[0].Points))
	}
	if math.Abs(res[0].Points[0].Value-2.0) > 1e-9 {
		t.Fatalf("p50: got %v, want 2.0", res[0].Points[0].Value)
	}
	if _, ok := res[0].Labels["le"]; ok {
		t.Fatalf("result must drop the le label: %+v", res[0].Labels)
	}
	if _, ok := res[0].Labels["__name__"]; ok {
		t.Fatalf("result must drop __name__: %+v", res[0].Labels)
	}
}
