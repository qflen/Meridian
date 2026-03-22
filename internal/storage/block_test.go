package storage

import (
	"testing"
)

func TestWriteAndReadBlock(t *testing.T) {
	dir := t.TempDir()
	h := NewHeadBlock()

	// Create series and ingest data
	s1, _ := h.GetOrCreateSeries("cpu_usage", map[string]string{"host": "web-01"})
	s2, _ := h.GetOrCreateSeries("cpu_usage", map[string]string{"host": "web-02"})
	s3, _ := h.GetOrCreateSeries("mem_bytes", map[string]string{"host": "web-01"})

	for i := 0; i < 100; i++ {
		ts := int64(i) * 5000
		h.Ingest(s1.ID, ts, float64(45+i%10))
		h.Ingest(s2.ID, ts, float64(60+i%5))
		h.Ingest(s3.ID, ts, float64(8e9)+float64(i)*1e6)
	}

	// Write block
	block, err := WriteBlock(dir, h)
	if err != nil {
		t.Fatal(err)
	}

	meta := block.Meta()
	if meta.Stats.NumSeries != 3 {
		t.Fatalf("series: %d", meta.Stats.NumSeries)
	}
	if meta.Stats.NumSamples != 300 {
		t.Fatalf("samples: %d", meta.Stats.NumSamples)
	}
	if meta.MinTime != 0 {
		t.Fatalf("min time: %d", meta.MinTime)
	}
	if meta.MaxTime != 99*5000 {
		t.Fatalf("max time: %d", meta.MaxTime)
	}

	// Query all cpu_usage
	results := block.Query([]LabelMatcher{
		{Name: "__name__", Value: "cpu_usage", Type: MatchEqual},
	}, 0, 500000)
	if len(results) != 2 {
		t.Fatalf("query cpu_usage: got %d results", len(results))
	}
	for _, r := range results {
		if len(r.Points) != 100 {
			t.Fatalf("series %s: got %d points, want 100", r.Labels["host"], len(r.Points))
		}
	}

	// Query with specific host
	results = block.Query([]LabelMatcher{
		{Name: "__name__", Value: "cpu_usage", Type: MatchEqual},
		{Name: "host", Value: "web-01", Type: MatchEqual},
	}, 0, 500000)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Points[0].Value != 45.0 {
		t.Fatalf("first value: %f", results[0].Points[0].Value)
	}
}

func TestBlockPredicatePushdown(t *testing.T) {
	dir := t.TempDir()
	h := NewHeadBlock()

	s, _ := h.GetOrCreateSeries("metric", map[string]string{"host": "a"})
	for i := 0; i < 50; i++ {
		h.Ingest(s.ID, int64(1000+i*5000), float64(i))
	}

	block, err := WriteBlock(dir, h)
	if err != nil {
		t.Fatal(err)
	}

	// Query time range that doesn't overlap block at all
	results := block.Query([]LabelMatcher{
		{Name: "__name__", Value: "metric", Type: MatchEqual},
	}, 999999, 9999999)
	if len(results) != 0 {
		t.Fatalf("expected 0 results for non-overlapping range, got %d", len(results))
	}

	// Query time range that partially overlaps
	results = block.Query([]LabelMatcher{
		{Name: "__name__", Value: "metric", Type: MatchEqual},
	}, 50000, 100000)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	// Should only get points in [50000, 100000]
	for _, p := range results[0].Points {
		if p.Timestamp < 50000 || p.Timestamp > 100000 {
			t.Fatalf("point outside range: ts=%d", p.Timestamp)
		}
	}
}

func TestBlockReopenFromDisk(t *testing.T) {
	dir := t.TempDir()
	h := NewHeadBlock()

	s, _ := h.GetOrCreateSeries("test_metric", map[string]string{"env": "prod", "region": "us"})
	for i := 0; i < 50; i++ {
		h.Ingest(s.ID, int64(i)*1000, float64(i)*1.5)
	}

	block, err := WriteBlock(dir, h)
	if err != nil {
		t.Fatal(err)
	}
	blockDir := block.Dir()

	// Re-open from disk (simulating process restart)
	block2, err := OpenBlock(blockDir)
	if err != nil {
		t.Fatal(err)
	}

	results := block2.Query([]LabelMatcher{
		{Name: "__name__", Value: "test_metric", Type: MatchEqual},
	}, 0, 50000)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0].Points) != 50 {
		t.Fatalf("expected 50 points, got %d", len(results[0].Points))
	}
	if results[0].Points[25].Value != 37.5 {
		t.Fatalf("point 25 value: %f", results[0].Points[25].Value)
	}
}

func TestBlockIndexLookup(t *testing.T) {
	dir := t.TempDir()
	h := NewHeadBlock()

	for _, host := range []string{"web-01", "web-02", "db-01"} {
		for _, metric := range []string{"cpu", "mem"} {
			s, _ := h.GetOrCreateSeries(metric, map[string]string{"host": host})
			h.Ingest(s.ID, 1000, 1.0)
		}
	}

	block, err := WriteBlock(dir, h)
	if err != nil {
		t.Fatal(err)
	}

	// Regex match
	results := block.Query([]LabelMatcher{
		{Name: "host", Value: "web-.*", Type: MatchRegexp},
	}, 0, 2000)
	if len(results) != 4 { // 2 hosts × 2 metrics
		t.Fatalf("regex web-.*: %d results", len(results))
	}

	// Not-equal
	results = block.Query([]LabelMatcher{
		{Name: "__name__", Value: "cpu", Type: MatchEqual},
		{Name: "host", Value: "db-01", Type: MatchNotEqual},
	}, 0, 2000)
	if len(results) != 2 {
		t.Fatalf("not-equal: %d results", len(results))
	}
}

func TestBlockEmptyHead(t *testing.T) {
	dir := t.TempDir()
	h := NewHeadBlock()

	_, err := WriteBlock(dir, h)
	if err == nil {
		t.Fatal("expected error for empty head")
	}
}
