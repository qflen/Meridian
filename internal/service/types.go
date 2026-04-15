// Package service provides shared types and HTTP clients for inter-service communication.
package service

import "github.com/meridiandb/meridian/internal/storage"

// WriteRequest is sent from ingestor → storage to write samples.
type WriteRequest struct {
	TimeSeries []TimeSeries `json:"time_series"`
}

// TimeSeries is a named metric with labels and sample data points.
type TimeSeries struct {
	Name    string   `json:"name"`
	Labels  []Label  `json:"labels"`
	Samples []Sample `json:"samples"`
}

// Label is a key-value pair attached to a time series.
type Label struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Sample is a single timestamped data point.
type Sample struct {
	TimestampMs int64   `json:"timestamp_ms"`
	Value       float64 `json:"value"`
}

// WriteResponse reports the number of samples successfully ingested.
type WriteResponse struct {
	SamplesIngested int64 `json:"samples_ingested"`
}

// QueryRequest is sent from querier → storage to query raw series data.
type QueryRequest struct {
	Matchers []MatcherJSON `json:"matchers"`
	Start    int64         `json:"start"`
	End      int64         `json:"end"`
}

// MatcherJSON serializes a label matcher over the wire.
type MatcherJSON struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type"` // "=", "!=", "=~", "!~"
}

// SeriesResult is a single series in a query response.
type SeriesResult struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Points []PointJSON       `json:"points"`
}

// PointJSON is a timestamp-value pair.
type PointJSON struct {
	Timestamp int64   `json:"t"`
	Value     float64 `json:"v"`
}

// QueryResponse is returned by storage nodes for query requests.
type QueryResponse struct {
	Status string         `json:"status"`
	Data   []SeriesResult `json:"data"`
}

// BlockInfo describes a persistent block on a storage node.
type BlockInfo struct {
	ULID       string `json:"ulid"`
	NodeID     string `json:"node_id"`
	MinTime    int64  `json:"min_time"`
	MaxTime    int64  `json:"max_time"`
	NumSamples int64  `json:"num_samples"`
	NumSeries  int    `json:"num_series"`
	Level      int    `json:"level"`
}

// NodeInfo describes a service in the cluster topology.
type NodeInfo struct {
	ID      string `json:"id"`
	Addr    string `json:"addr"`
	State   string `json:"state"`
	Role    string `json:"role"` // gateway, ingestor, storage, querier, compactor
	Series  int    `json:"series"`
	Samples int64  `json:"samples"`
}

// StatsResponse from a storage node.
type StatsResponse struct {
	TotalSamples      int64   `json:"total_samples"`
	TotalSeries       int     `json:"total_series"`
	BlockCount        int     `json:"blocks"`
	CompressionRatio  string  `json:"compression_ratio"`
	StorageBytesRaw   int64   `json:"storage_bytes_raw"`
	StorageBytesDisk  int64   `json:"storage_bytes_compressed"`
	HeadSamples       int64   `json:"head_samples"`
	HeadSeries        int     `json:"head_series"`
	WALSize           int64   `json:"wal_size"`
	IngestionRate     int64   `json:"ingestion_rate"`
	Uptime            string  `json:"uptime"`
}

// MatcherToStorage converts a MatcherJSON to a storage.LabelMatcher.
func MatcherToStorage(m MatcherJSON) storage.LabelMatcher {
	var mt storage.MatchType
	switch m.Type {
	case "=":
		mt = storage.MatchEqual
	case "!=":
		mt = storage.MatchNotEqual
	case "=~":
		mt = storage.MatchRegexp
	case "!~":
		mt = storage.MatchNotRegexp
	default:
		mt = storage.MatchEqual
	}
	return storage.LabelMatcher{Name: m.Name, Value: m.Value, Type: mt}
}

// StorageToMatcher converts a storage.LabelMatcher to a MatcherJSON.
func StorageToMatcher(m storage.LabelMatcher) MatcherJSON {
	var t string
	switch m.Type {
	case storage.MatchEqual:
		t = "="
	case storage.MatchNotEqual:
		t = "!="
	case storage.MatchRegexp:
		t = "=~"
	case storage.MatchNotRegexp:
		t = "!~"
	default:
		t = "="
	}
	return MatcherJSON{Name: m.Name, Value: m.Value, Type: t}
}
