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

// WriteResponse reports the number of samples successfully ingested.
type WriteResponse struct {
	SamplesIngested int64
}
