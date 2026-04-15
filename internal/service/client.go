package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// StorageClient communicates with storage service nodes over HTTP.
type StorageClient struct {
	addrs  []string
	client *http.Client
}

// NewStorageClient creates a client for the given storage node addresses.
func NewStorageClient(addrs []string) *StorageClient {
	return &StorageClient{
		addrs: addrs,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Addrs returns the configured storage node addresses.
func (c *StorageClient) Addrs() []string {
	return c.addrs
}

// Write sends a write request to the appropriate storage node based on metric name hash.
func (c *StorageClient) Write(ctx context.Context, req WriteRequest) (*WriteResponse, error) {
	// Group time series by target node (hash-based sharding by metric name)
	shards := make(map[int][]TimeSeries)
	for _, ts := range req.TimeSeries {
		idx := c.shardFor(ts.Name)
		shards[idx] = append(shards[idx], ts)
	}

	var totalIngested int64
	for idx, series := range shards {
		shardReq := WriteRequest{TimeSeries: series}
		body, err := json.Marshal(shardReq)
		if err != nil {
			return nil, fmt.Errorf("marshal write request: %w", err)
		}

		url := fmt.Sprintf("http://%s/api/internal/write", c.addrs[idx])
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := c.client.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("write to storage %s: %w", c.addrs[idx], err)
		}
		var wr WriteResponse
		json.NewDecoder(resp.Body).Decode(&wr)
		resp.Body.Close()
		totalIngested += wr.SamplesIngested
	}

	return &WriteResponse{SamplesIngested: totalIngested}, nil
}

// Query fans out a query to all storage nodes and merges results.
func (c *StorageClient) Query(ctx context.Context, matchers []storage.LabelMatcher, start, end int64) (storage.SeriesSet, error) {
	// Build request
	matcherJSON := make([]MatcherJSON, len(matchers))
	for i, m := range matchers {
		matcherJSON[i] = StorageToMatcher(m)
	}
	qr := QueryRequest{Matchers: matcherJSON, Start: start, End: end}
	body, err := json.Marshal(qr)
	if err != nil {
		return nil, err
	}

	type result struct {
		data []SeriesResult
		err  error
	}

	// Fan out to all storage nodes in parallel
	results := make(chan result, len(c.addrs))
	for _, addr := range c.addrs {
		go func(addr string) {
			url := fmt.Sprintf("http://%s/api/internal/query", addr)
			req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
			if err != nil {
				results <- result{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := c.client.Do(req)
			if err != nil {
				results <- result{err: fmt.Errorf("query storage %s: %w", addr, err)}
				return
			}
			defer resp.Body.Close()

			var qr QueryResponse
			if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
				results <- result{err: fmt.Errorf("decode response from %s: %w", addr, err)}
				return
			}
			results <- result{data: qr.Data}
		}(addr)
	}

	// Merge results from all nodes
	merged := make(map[string]*storage.ResultSeries)
	for range c.addrs {
		r := <-results
		if r.err != nil {
			continue // Skip failed nodes, return partial results
		}
		for _, sr := range r.data {
			key := seriesKey(sr.Name, sr.Labels)
			points := make([]storage.Point, len(sr.Points))
			for i, p := range sr.Points {
				points[i] = storage.Point{Timestamp: p.Timestamp, Value: p.Value}
			}
			if existing, ok := merged[key]; ok {
				existing.Points = mergePoints(existing.Points, points)
			} else {
				merged[key] = &storage.ResultSeries{
					Name:   sr.Name,
					Labels: sr.Labels,
					Points: points,
				}
			}
		}
	}

	ss := make(storage.SeriesSet, 0, len(merged))
	for _, rs := range merged {
		sort.Slice(rs.Points, func(i, j int) bool {
			return rs.Points[i].Timestamp < rs.Points[j].Timestamp
		})
		ss = append(ss, *rs)
	}
	return ss, nil
}

// FetchBlocks retrieves block metadata from all storage nodes.
func (c *StorageClient) FetchBlocks(ctx context.Context) ([]BlockInfo, error) {
	type result struct {
		blocks []BlockInfo
		err    error
	}
	results := make(chan result, len(c.addrs))
	for _, addr := range c.addrs {
		go func(addr string) {
			url := fmt.Sprintf("http://%s/api/internal/blocks", addr)
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			resp, err := c.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			var blocks []BlockInfo
			json.NewDecoder(resp.Body).Decode(&blocks)
			results <- result{blocks: blocks}
		}(addr)
	}

	var all []BlockInfo
	for range c.addrs {
		r := <-results
		if r.err == nil {
			all = append(all, r.blocks...)
		}
	}
	return all, nil
}

// FetchStats retrieves stats from all storage nodes and aggregates them.
func (c *StorageClient) FetchStats(ctx context.Context) (*AggregatedStats, error) {
	type result struct {
		stats StatsResponse
		err   error
	}
	results := make(chan result, len(c.addrs))
	for _, addr := range c.addrs {
		go func(addr string) {
			url := fmt.Sprintf("http://%s/api/internal/stats", addr)
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			resp, err := c.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			var s StatsResponse
			json.NewDecoder(resp.Body).Decode(&s)
			results <- result{stats: s}
		}(addr)
	}

	agg := &AggregatedStats{}
	for range c.addrs {
		r := <-results
		if r.err != nil {
			continue
		}
		agg.TotalSamples += r.stats.TotalSamples
		agg.TotalSeries += r.stats.TotalSeries
		agg.BlockCount += r.stats.BlockCount
		agg.StorageBytesRaw += r.stats.StorageBytesRaw
		agg.StorageBytesDisk += r.stats.StorageBytesDisk
		agg.HeadSamples += r.stats.HeadSamples
		agg.HeadSeries += r.stats.HeadSeries
		agg.WALSize += r.stats.WALSize
		agg.IngestionRate += r.stats.IngestionRate
	}
	return agg, nil
}

// AggregatedStats holds stats merged from all storage nodes.
type AggregatedStats struct {
	TotalSamples     int64
	TotalSeries      int
	BlockCount       int
	StorageBytesRaw  int64
	StorageBytesDisk int64
	HeadSamples      int64
	HeadSeries       int
	WALSize          int64
	IngestionRate    int64
}

// HealthCheck probes a service's /health endpoint.
func HealthCheck(addr string) (string, bool) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://%s/health", addr))
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var h struct {
		NodeID string `json:"node_id"`
		Status string `json:"status"`
		Role   string `json:"role"`
	}
	json.NewDecoder(resp.Body).Decode(&h)
	return h.NodeID, h.Status == "ok"
}

// FetchSeries retrieves series metadata from all storage nodes.
func (c *StorageClient) FetchSeries(ctx context.Context) ([]SeriesInfo, error) {
	type result struct {
		series []SeriesInfo
		err    error
	}
	results := make(chan result, len(c.addrs))
	for _, addr := range c.addrs {
		go func(addr string) {
			url := fmt.Sprintf("http://%s/api/internal/series", addr)
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			resp, err := c.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			var wrapper struct {
				Data []SeriesInfo `json:"data"`
			}
			json.Unmarshal(body, &wrapper)
			results <- result{series: wrapper.Data}
		}(addr)
	}

	var all []SeriesInfo
	for range c.addrs {
		r := <-results
		if r.err == nil {
			all = append(all, r.series...)
		}
	}
	return all, nil
}

// SeriesInfo mirrors storage.SeriesInfo for JSON transport.
type SeriesInfo struct {
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels"`
	SampleCount int               `json:"samples_count"`
	LastValue   float64           `json:"last_value"`
}

// FetchLabels retrieves label names from all storage nodes and deduplicates.
func (c *StorageClient) FetchLabels(ctx context.Context) ([]string, error) {
	type result struct {
		labels []string
		err    error
	}
	results := make(chan result, len(c.addrs))
	for _, addr := range c.addrs {
		go func(addr string) {
			url := fmt.Sprintf("http://%s/api/internal/labels", addr)
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			resp, err := c.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			var wrapper struct {
				Data []string `json:"data"`
			}
			json.NewDecoder(resp.Body).Decode(&wrapper)
			results <- result{labels: wrapper.Data}
		}(addr)
	}

	seen := make(map[string]bool)
	for range c.addrs {
		r := <-results
		if r.err == nil {
			for _, l := range r.labels {
				seen[l] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// FetchLabelValues retrieves values for a label from all storage nodes.
func (c *StorageClient) FetchLabelValues(ctx context.Context, name string) ([]string, error) {
	type result struct {
		values []string
		err    error
	}
	results := make(chan result, len(c.addrs))
	for _, addr := range c.addrs {
		go func(addr string) {
			url := fmt.Sprintf("http://%s/api/internal/label/%s/values", addr, name)
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			resp, err := c.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			var wrapper struct {
				Data []string `json:"data"`
			}
			json.NewDecoder(resp.Body).Decode(&wrapper)
			results <- result{values: wrapper.Data}
		}(addr)
	}

	seen := make(map[string]bool)
	for range c.addrs {
		r := <-results
		if r.err == nil {
			for _, v := range r.values {
				seen[v] = true
			}
		}
	}
	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}
	sort.Strings(values)
	return values, nil
}

// DeleteBlock tells a specific storage node to delete a block.
func (c *StorageClient) DeleteBlock(ctx context.Context, addr, ulid string) error {
	url := fmt.Sprintf("http://%s/api/internal/blocks/%s", addr, ulid)
	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("delete block %s on %s: status %d", ulid, addr, resp.StatusCode)
	}
	return nil
}

func (c *StorageClient) shardFor(metricName string) int {
	h := fnv.New32a()
	h.Write([]byte(metricName))
	return int(h.Sum32()) % len(c.addrs)
}

func seriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	key := name + "{"
	for i, k := range keys {
		if i > 0 {
			key += ","
		}
		key += k + "=" + labels[k]
	}
	key += "}"
	return key
}

func mergePoints(a, b []storage.Point) []storage.Point {
	result := make([]storage.Point, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].Timestamp < b[j].Timestamp {
			result = append(result, a[i])
			i++
		} else if a[i].Timestamp > b[j].Timestamp {
			result = append(result, b[j])
			j++
		} else {
			result = append(result, a[i])
			i++
			j++
		}
	}
	result = append(result, a[i:]...)
	result = append(result, b[j:]...)
	return result
}

// LatencyTracker maintains a simple histogram of query latencies.
type LatencyTracker struct {
	mu      sync.Mutex
	buckets []LatencyBucket
}

// LatencyBucket represents a histogram bucket.
type LatencyBucket struct {
	LE    string `json:"le"`
	Count int64  `json:"count"`
}

// NewLatencyTracker creates a tracker with standard latency buckets.
func NewLatencyTracker() *LatencyTracker {
	return &LatencyTracker{
		buckets: []LatencyBucket{
			{LE: "1ms"}, {LE: "5ms"}, {LE: "10ms"}, {LE: "25ms"},
			{LE: "50ms"}, {LE: "100ms"}, {LE: "250ms"}, {LE: "500ms"}, {LE: "1s"},
		},
	}
}

// Record adds a latency observation to the histogram.
func (lt *LatencyTracker) Record(d time.Duration) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	ms := d.Milliseconds()
	thresholds := []int64{1, 5, 10, 25, 50, 100, 250, 500, 1000}
	for i, t := range thresholds {
		if ms <= t {
			lt.buckets[i].Count++
			return
		}
	}
	// Over 1s goes in last bucket
	lt.buckets[len(lt.buckets)-1].Count++
}

// Buckets returns a snapshot of the histogram.
func (lt *LatencyTracker) Buckets() []LatencyBucket {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	out := make([]LatencyBucket, len(lt.buckets))
	copy(out, lt.buckets)
	return out
}
