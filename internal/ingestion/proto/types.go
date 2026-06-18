// Package proto defines the ingestion API message types.
//
// These types mirror the protobuf definitions in ingestion.proto and are used
// for gRPC wire format encoding. Since the project avoids code generation
// dependencies, these are hand-written Go structs.
package proto

// WriteRequest contains one or more time series to be ingested.
type WriteRequest struct {
	TimeSeries []TimeSeries
}

// TimeSeries is a named metric with labels and sample data points.
type TimeSeries struct {
	Name    string
	Labels  []Label
	Samples []Sample
}

// Label is a key-value pair attached to a time series.
type Label struct {
	Name  string
	Value string
}

// Sample is a single timestamped data point.
type Sample struct {
	TimestampMs int64
	Value       float64
}

// WriteResponse reports the number of samples accepted, plus backpressure signals.
type WriteResponse struct {
	SamplesIngested int64
	// Shed is the number of samples the server dropped under overload (its bounded
	// ingest queue was full past the block deadline). A non-zero Shed is the NACK:
	// the producer should back off. meridian_dropped_samples_total is the
	// authoritative cumulative count.
	Shed int64 `json:",omitempty"`
	// Throttled is set when the server's ingest queue is at or above its high-water
	// mark — an early hint to ease off before shedding begins.
	Throttled bool `json:",omitempty"`
}
