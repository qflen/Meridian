// Package ingestion provides the gRPC ingestion server and batch writer.
package ingestion

import (
	"fmt"
	"regexp"
)

var validMetricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var validLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidateMetricName checks that a metric name follows Prometheus naming conventions.
func ValidateMetricName(name string) error {
	if name == "" {
		return fmt.Errorf("metric name cannot be empty")
	}
	if !validMetricName.MatchString(name) {
		return fmt.Errorf("invalid metric name: %q", name)
	}
	return nil
}

// ValidateLabelName checks that a label name follows Prometheus naming conventions.
func ValidateLabelName(name string) error {
	if name == "" {
		return fmt.Errorf("label name cannot be empty")
	}
	if !validLabelName.MatchString(name) {
		return fmt.Errorf("invalid label name: %q", name)
	}
	return nil
}
