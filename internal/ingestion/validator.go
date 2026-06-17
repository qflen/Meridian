// Package ingestion provides the gRPC ingestion server and batch writer.
package ingestion

import (
	"fmt"
	"regexp"

	"github.com/meridiandb/meridian/internal/storage"
)

var validMetricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var validLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidateMetricName checks that a metric name follows Prometheus naming conventions
// and fits the storage field-size limit.
func ValidateMetricName(name string) error {
	if name == "" {
		return fmt.Errorf("metric name cannot be empty")
	}
	if len(name) > storage.MaxMetricNameLength {
		return fmt.Errorf("metric name length %d exceeds limit %d", len(name), storage.MaxMetricNameLength)
	}
	if !validMetricName.MatchString(name) {
		return fmt.Errorf("invalid metric name: %q", name)
	}
	return nil
}

// ValidateLabelName checks that a label name follows Prometheus naming conventions
// and fits the storage field-size limit.
func ValidateLabelName(name string) error {
	if name == "" {
		return fmt.Errorf("label name cannot be empty")
	}
	if len(name) > storage.MaxLabelNameLength {
		return fmt.Errorf("label name length %d exceeds limit %d", len(name), storage.MaxLabelNameLength)
	}
	if !validLabelName.MatchString(name) {
		return fmt.Errorf("invalid label name: %q", name)
	}
	return nil
}

// ValidateLabel checks a label's name and bounds its value size. Label values are
// length-prefixed with a uint16 on disk, so an oversized value is rejected rather
// than silently truncated (the original validator did not bound values at all).
func ValidateLabel(name, value string) error {
	if err := ValidateLabelName(name); err != nil {
		return err
	}
	if len(value) > storage.MaxLabelValueLength {
		return fmt.Errorf("label %q value length %d exceeds limit %d", name, len(value), storage.MaxLabelValueLength)
	}
	return nil
}
