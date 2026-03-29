package retention

import (
	"math"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// DownsampleRule defines a rollup rule for downsampling.
type DownsampleRule struct {
	SourceInterval time.Duration
	TargetInterval time.Duration
	Retention      time.Duration
}

// Downsampler performs automatic rollup of time-series data at progressively
// coarser intervals to reduce storage while preserving long-term trends.
type Downsampler struct {
	db    *storage.TSDB
	rules []DownsampleRule
}

// RollupResult holds the aggregated values for a single rollup window.
type RollupResult struct {
	Timestamp int64
	Min       float64
	Max       float64
	Avg       float64
	Sum       float64
	Count     int
}

// NewDownsampler creates a downsampler with the given rules.
func NewDownsampler(db *storage.TSDB, rules []DownsampleRule) *Downsampler {
	return &Downsampler{
		db:    db,
		rules: rules,
	}
}

// Rollup computes aggregated values for points within fixed windows.
func Rollup(points []storage.Point, windowMs int64) []RollupResult {
	if len(points) == 0 || windowMs <= 0 {
		return nil
	}

	var results []RollupResult
	windowStart := (points[0].Timestamp / windowMs) * windowMs

	var (
		minVal = math.Inf(1)
		maxVal = math.Inf(-1)
		sum    float64
		count  int
	)

	for _, p := range points {
		windowEnd := windowStart + windowMs
		if p.Timestamp >= windowEnd {
			// Emit current window
			if count > 0 {
				results = append(results, RollupResult{
					Timestamp: windowStart + windowMs/2,
					Min:       minVal,
					Max:       maxVal,
					Avg:       sum / float64(count),
					Sum:       sum,
					Count:     count,
				})
			}
			// Advance to the window containing this point
			windowStart = (p.Timestamp / windowMs) * windowMs
			minVal = math.Inf(1)
			maxVal = math.Inf(-1)
			sum = 0
			count = 0
		}

		if p.Value < minVal {
			minVal = p.Value
		}
		if p.Value > maxVal {
			maxVal = p.Value
		}
		sum += p.Value
		count++
	}

	// Emit last window
	if count > 0 {
		results = append(results, RollupResult{
			Timestamp: windowStart + windowMs/2,
			Min:       minVal,
			Max:       maxVal,
			Avg:       sum / float64(count),
			Sum:       sum,
			Count:     count,
		})
	}

	return results
}
