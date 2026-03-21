package storage

import (
	"fmt"
	"sync"
	"testing"
)

func TestHeadBlockIngestAndQuery(t *testing.T) {
	h := NewHeadBlock()

	s1, created := h.GetOrCreateSeries("cpu_usage", map[string]string{"host": "web-01"})
	if !created {
		t.Fatal("expected new series")
	}
	s2, created := h.GetOrCreateSeries("cpu_usage", map[string]string{"host": "web-02"})
	if !created {
		t.Fatal("expected new series")
	}
	s3, created := h.GetOrCreateSeries("mem_bytes", map[string]string{"host": "web-01"})
	if !created {
		t.Fatal("expected new series")
	}

	// Ingest samples
	for i := 0; i < 100; i++ {
		ts := int64(i) * 5000
		h.Ingest(s1.ID, ts, float64(45+i%10))
		h.Ingest(s2.ID, ts, float64(60+i%5))
		h.Ingest(s3.ID, ts, float64(8*1024*1024*1024))
	}

	if h.SampleCount() != 300 {
		t.Fatalf("sample count: got %d, want 300", h.SampleCount())
	}
	if h.SeriesCount() != 3 {
		t.Fatalf("series count: got %d, want 3", h.SeriesCount())
	}

	// Query by name
	results := h.Query([]LabelMatcher{
		{Name: "__name__", Value: "cpu_usage", Type: MatchEqual},
	}, 0, 500000)
	if len(results) != 2 {
		t.Fatalf("query cpu_usage: got %d series, want 2", len(results))
	}

	// Query by name + host
	results = h.Query([]LabelMatcher{
		{Name: "__name__", Value: "cpu_usage", Type: MatchEqual},
		{Name: "host", Value: "web-01", Type: MatchEqual},
	}, 0, 500000)
	if len(results) != 1 {
		t.Fatalf("query cpu_usage{host=web-01}: got %d, want 1", len(results))
	}
	if results[0].Name != "cpu_usage" {
		t.Fatalf("wrong series: %s", results[0].Name)
	}

	// Query with not-equal
	results = h.Query([]LabelMatcher{
		{Name: "__name__", Value: "cpu_usage", Type: MatchEqual},
		{Name: "host", Value: "web-01", Type: MatchNotEqual},
	}, 0, 500000)
	if len(results) != 1 {
		t.Fatalf("query cpu_usage{host!=web-01}: got %d, want 1", len(results))
	}
	if results[0].Labels["host"] != "web-02" {
		t.Fatalf("wrong host: %s", results[0].Labels["host"])
	}
}

func TestHeadBlockRegexQuery(t *testing.T) {
	h := NewHeadBlock()

	hosts := []string{"web-01", "web-02", "web-03", "db-01", "cache-01"}
	for _, host := range hosts {
		s, _ := h.GetOrCreateSeries("cpu_usage", map[string]string{"host": host})
		h.Ingest(s.ID, 1000, 50.0)
	}

	// Regex match: web-.*
	results := h.Query([]LabelMatcher{
		{Name: "host", Value: "web-.*", Type: MatchRegexp},
	}, 0, 2000)
	if len(results) != 3 {
		t.Fatalf("regex web-.*: got %d, want 3", len(results))
	}

	// Negative regex: !~ web-.*
	results = h.Query([]LabelMatcher{
		{Name: "host", Value: "web-.*", Type: MatchNotRegexp},
	}, 0, 2000)
	if len(results) != 2 {
		t.Fatalf("not regex web-.*: got %d, want 2", len(results))
	}
}

func TestHeadBlockDuplicateSeries(t *testing.T) {
	h := NewHeadBlock()

	s1, created1 := h.GetOrCreateSeries("metric", map[string]string{"a": "1"})
	s2, created2 := h.GetOrCreateSeries("metric", map[string]string{"a": "1"})

	if !created1 {
		t.Fatal("first creation should be new")
	}
	if created2 {
		t.Fatal("second creation should return existing")
	}
	if s1.ID != s2.ID {
		t.Fatalf("IDs should match: %d != %d", s1.ID, s2.ID)
	}
	if h.SeriesCount() != 1 {
		t.Fatalf("series count: %d", h.SeriesCount())
	}
}

func TestHeadBlockTimeRange(t *testing.T) {
	h := NewHeadBlock()
	s, _ := h.GetOrCreateSeries("metric", nil)

	h.Ingest(s.ID, 5000, 1.0)
	h.Ingest(s.ID, 10000, 2.0)
	h.Ingest(s.ID, 15000, 3.0)

	if h.MinTime() != 5000 {
		t.Fatalf("min time: %d", h.MinTime())
	}
	if h.MaxTime() != 15000 {
		t.Fatalf("max time: %d", h.MaxTime())
	}

	// Query that misses the time range
	results := h.Query([]LabelMatcher{
		{Name: "__name__", Value: "metric", Type: MatchEqual},
	}, 20000, 30000)
	if len(results) != 0 {
		t.Fatalf("expected 0 results for out-of-range query, got %d", len(results))
	}
}

func TestHeadBlockReset(t *testing.T) {
	h := NewHeadBlock()
	s, _ := h.GetOrCreateSeries("metric", nil)
	h.Ingest(s.ID, 1000, 1.0)

	h.Reset()

	if h.SeriesCount() != 0 {
		t.Fatalf("series count after reset: %d", h.SeriesCount())
	}
	if h.SampleCount() != 0 {
		t.Fatalf("sample count after reset: %d", h.SampleCount())
	}
}

func TestHeadBlockConcurrentIngest(t *testing.T) {
	h := NewHeadBlock()
	nSeries := 10
	samplesPerSeries := 1000

	var wg sync.WaitGroup
	for i := 0; i < nSeries; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			s, _ := h.GetOrCreateSeries(
				fmt.Sprintf("metric_%d", idx),
				map[string]string{"idx": fmt.Sprintf("%d", idx)},
			)
			for j := 0; j < samplesPerSeries; j++ {
				h.Ingest(s.ID, int64(j)*5000, float64(j))
			}
		}(i)
	}
	wg.Wait()

	if h.SeriesCount() != nSeries {
		t.Fatalf("series: %d", h.SeriesCount())
	}
	if h.SampleCount() != int64(nSeries*samplesPerSeries) {
		t.Fatalf("samples: %d", h.SampleCount())
	}
}

func TestInvertedIndexLabelNames(t *testing.T) {
	h := NewHeadBlock()
	h.GetOrCreateSeries("cpu", map[string]string{"host": "a", "region": "us"})
	h.GetOrCreateSeries("mem", map[string]string{"host": "b", "dc": "dc1"})

	names := h.index.LabelNames()
	expected := []string{"__name__", "dc", "host", "region"}
	if len(names) != len(expected) {
		t.Fatalf("label names: %v", names)
	}
	for i, n := range names {
		if n != expected[i] {
			t.Fatalf("label %d: got %s, want %s", i, n, expected[i])
		}
	}
}

func TestInvertedIndexLabelValues(t *testing.T) {
	h := NewHeadBlock()
	h.GetOrCreateSeries("cpu", map[string]string{"host": "web-01"})
	h.GetOrCreateSeries("cpu", map[string]string{"host": "web-02"})
	h.GetOrCreateSeries("cpu", map[string]string{"host": "db-01"})

	values := h.index.LabelValues("host")
	if len(values) != 3 {
		t.Fatalf("expected 3 host values, got %d: %v", len(values), values)
	}
}

func TestSeriesInfos(t *testing.T) {
	h := NewHeadBlock()
	s1, _ := h.GetOrCreateSeries("cpu", map[string]string{"host": "a"})
	s2, _ := h.GetOrCreateSeries("mem", map[string]string{"host": "a"})

	h.Ingest(s1.ID, 1000, 1.0)
	h.Ingest(s1.ID, 2000, 2.0)
	h.Ingest(s2.ID, 1000, 8.0)

	infos := h.SeriesInfos()
	if len(infos) != 2 {
		t.Fatalf("expected 2 series infos, got %d", len(infos))
	}

	var cpuInfo, memInfo SeriesInfo
	for _, info := range infos {
		if info.Name == "cpu" {
			cpuInfo = info
		} else {
			memInfo = info
		}
	}
	if cpuInfo.SampleCount != 2 {
		t.Fatalf("cpu samples: %d", cpuInfo.SampleCount)
	}
	if memInfo.SampleCount != 1 {
		t.Fatalf("mem samples: %d", memInfo.SampleCount)
	}
}
