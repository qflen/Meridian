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

// QueryRequest is sent from querier → storage to query series data. Resolution and
// Aggregate are the cluster's half of query-time rollup selection (ADR-011): the querier
// runs the same planner the monolith does and asks each node to serve a chosen rollup
// resolution and aggregate column. Resolution is the requested rollup window in ms; 0 (the
// zero value, so older callers are unaffected) means raw. Aggregate names the rollup column
// to read at a coarse resolution (see AggregateToWire); it is ignored when Resolution is 0.
type QueryRequest struct {
	Matchers   []MatcherJSON `json:"matchers"`
	Start      int64         `json:"start"`
	End        int64         `json:"end"`
	Resolution int64         `json:"resolution,omitempty"`
	Aggregate  string        `json:"aggregate,omitempty"`
}

// DigestRequest asks a storage node for a Merkle range digest (ADR-030): the per-window
// content hashes over the series whose ring position falls in any of Ranges, for samples
// in [Start, End], bucketed by Window ms. Each range is a [lo, hi] hash arc — half-open
// (lo, hi]; lo > hi wraps the ring; lo == hi is the whole ring. The response is a
// storage.MerkleDigest. The arc set is supplied by the coordinator (which holds the
// ring) so the storage node stays ring-agnostic.
type DigestRequest struct {
	Ranges [][2]uint64 `json:"ranges"`
	Start  int64       `json:"start"`
	End    int64       `json:"end"`
	Window int64       `json:"window"`
}

// RangeRequest asks a storage node to export the raw samples whose ring position falls
// in any of Ranges, for samples in [Start, End] (ADR-030). The response is a
// WriteRequest, so the coordinator can push whatever a peer is missing straight back
// through the same backfill path hinted handoff uses.
type RangeRequest struct {
	Ranges [][2]uint64 `json:"ranges"`
	Start  int64       `json:"start"`
	End    int64       `json:"end"`
}

// DropRequest asks a storage node to drop every series whose ring position falls in any of
// Ranges — the data the node no longer owns after a rebalance (ADR-031). Each range is a
// [lo, hi] hash arc, half-open (lo, hi]; lo > hi wraps the ring. The arc set is supplied by
// the migration coordinator (which holds the ring) so the storage node stays ring-agnostic,
// and is only ever issued after the new owners have confirmed receipt at quorum. An empty
// range set is a no-op.
type DropRequest struct {
	Ranges [][2]uint64 `json:"ranges"`
}

// DropResponse reports what a DropRequest removed from a storage node (ADR-031), so the
// coordinator can account for the bytes/series reclaimed.
type DropResponse struct {
	SeriesDropped   int   `json:"series_dropped"`
	SamplesDropped  int64 `json:"samples_dropped"`
	RollupWindows   int64 `json:"rollup_windows_dropped"`
	BlocksRewritten int   `json:"blocks_rewritten"`
	BlocksDeleted   int   `json:"blocks_deleted"`
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

// QueryResponse is returned by storage nodes for query requests. ResolutionMs reports the
// resolution the node actually served (0 = raw): a node serves the coarsest tier it holds
// that is no coarser than the requested resolution, so a node missing the requested tier
// (just restarted, mid-downsample, heterogeneous) reports a finer resolution and the
// querier reconciles the merge across replicas. It is the wire counterpart of the
// monolith's transparent resolution reporting.
type QueryResponse struct {
	Status       string         `json:"status"`
	Data         []SeriesResult `json:"data"`
	ResolutionMs int64          `json:"resolution_ms"`
}

// ResolutionsResponse advertises a storage node's rollup tier availability so the querier
// can run the same resolution planner the monolith does. Resolutions lists every tier (ms)
// that currently has data; IncreaseResolutions is the subset whose counter-increase column
// is complete, i.e. the tiers from which rate() can be served (ADR-025).
type ResolutionsResponse struct {
	Resolutions         []int64 `json:"resolutions"`
	IncreaseResolutions []int64 `json:"increase_resolutions"`
}

// BlockInfo describes a persistent block on a storage node. Resolution is 0 for a raw
// block and the rollup window size in milliseconds for a rollup block, so the compactor
// can apply a per-resolution retention TTL.
type BlockInfo struct {
	ULID       string `json:"ulid"`
	NodeID     string `json:"node_id"`
	MinTime    int64  `json:"min_time"`
	MaxTime    int64  `json:"max_time"`
	NumSamples int64  `json:"num_samples"`
	NumSeries  int    `json:"num_series"`
	Level      int    `json:"level"`
	Resolution int64  `json:"resolution_ms"`
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
	// IngestionRate is a windowed samples/sec rate (not a cumulative count); per-node
	// rates sum to the cluster rate when aggregated.
	IngestionRate int64  `json:"ingestion_rate"`
	Uptime        string `json:"uptime"`
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

// AggregateToWire renders a storage.RollupAggregate as a stable wire token. A string
// (rather than the enum's integer value) keeps the protocol legible and robust to the
// constant order changing. An unknown aggregate maps to "avg", the safe default column.
func AggregateToWire(agg storage.RollupAggregate) string {
	switch agg {
	case storage.AggMin:
		return "min"
	case storage.AggMax:
		return "max"
	case storage.AggSum:
		return "sum"
	case storage.AggCount:
		return "count"
	case storage.AggIncrease:
		return "increase"
	default:
		return "avg"
	}
}

// AggregateFromWire parses a wire token back into a storage.RollupAggregate, defaulting to
// AggAvg for the empty string or an unrecognised token.
func AggregateFromWire(s string) storage.RollupAggregate {
	switch s {
	case "min":
		return storage.AggMin
	case "max":
		return storage.AggMax
	case "sum":
		return storage.AggSum
	case "count":
		return storage.AggCount
	case "increase":
		return storage.AggIncrease
	default:
		return storage.AggAvg
	}
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
