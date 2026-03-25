package query

import (
	"math"
	"sort"

	"github.com/meridiandb/meridian/internal/storage"
)

// rate computes the per-second rate of increase over a range of counter values.
func rate(points []storage.Point) []storage.Point {
	if len(points) < 2 {
		return nil
	}

	first := points[0]
	last := points[len(points)-1]
	durationSec := float64(last.Timestamp-first.Timestamp) / 1000.0
	if durationSec <= 0 {
		return nil
	}

	// Handle counter resets: sum up all positive increases
	var totalIncrease float64
	for i := 1; i < len(points); i++ {
		diff := points[i].Value - points[i-1].Value
		if diff >= 0 {
			totalIncrease += diff
		} else {
			// Counter reset: assume the new value is the increase
			totalIncrease += points[i].Value
		}
	}

	rateVal := totalIncrease / durationSec
	return []storage.Point{{Timestamp: last.Timestamp, Value: rateVal}}
}

// aggregateFunc applies an aggregation operation across multiple series.
func aggregateFunc(op string, seriesSets [][]storage.Point) []storage.Point {
	if len(seriesSets) == 0 {
		return nil
	}

	// Collect all unique timestamps
	tsSet := make(map[int64]bool)
	for _, points := range seriesSets {
		for _, p := range points {
			tsSet[p.Timestamp] = true
		}
	}

	timestamps := make([]int64, 0, len(tsSet))
	for ts := range tsSet {
		timestamps = append(timestamps, ts)
	}
	sort.Slice(timestamps, func(i, j int) bool { return timestamps[i] < timestamps[j] })

	// For each timestamp, aggregate across all series that have a value at that time
	result := make([]storage.Point, 0, len(timestamps))
	for _, ts := range timestamps {
		var values []float64
		for _, points := range seriesSets {
			for _, p := range points {
				if p.Timestamp == ts {
					values = append(values, p.Value)
					break
				}
			}
		}
		if len(values) == 0 {
			continue
		}

		var aggVal float64
		switch op {
		case "sum":
			for _, v := range values {
				aggVal += v
			}
		case "avg":
			for _, v := range values {
				aggVal += v
			}
			aggVal /= float64(len(values))
		case "max":
			aggVal = math.Inf(-1)
			for _, v := range values {
				if v > aggVal {
					aggVal = v
				}
			}
		case "min":
			aggVal = math.Inf(1)
			for _, v := range values {
				if v < aggVal {
					aggVal = v
				}
			}
		case "count":
			aggVal = float64(len(values))
		}

		result = append(result, storage.Point{Timestamp: ts, Value: aggVal})
	}

	return result
}

// histogramQuantile computes the phi-quantile from a histogram (simplified).
func histogramQuantile(phi float64, points []storage.Point) []storage.Point {
	if len(points) == 0 || phi < 0 || phi > 1 {
		return nil
	}

	values := make([]float64, len(points))
	for i, p := range points {
		values[i] = p.Value
	}
	sort.Float64s(values)

	idx := int(math.Ceil(phi*float64(len(values)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(values) {
		idx = len(values) - 1
	}

	return []storage.Point{{
		Timestamp: points[len(points)-1].Timestamp,
		Value:     values[idx],
	}}
}
