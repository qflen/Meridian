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

// aggregateFunc applies an aggregation operation across multiple series. It is
// linear in the total number of points: each series is scanned once into a
// per-timestamp accumulator, rather than rescanning every series for every
// timestamp.
func aggregateFunc(op string, seriesSets [][]storage.Point) []storage.Point {
	if len(seriesSets) == 0 {
		return nil
	}

	type accumulator struct {
		sum, min, max float64
		count         int
	}
	accs := make(map[int64]*accumulator)
	for _, points := range seriesSets {
		for _, p := range points {
			a, ok := accs[p.Timestamp]
			if !ok {
				a = &accumulator{min: math.Inf(1), max: math.Inf(-1)}
				accs[p.Timestamp] = a
			}
			a.sum += p.Value
			a.count++
			if p.Value < a.min {
				a.min = p.Value
			}
			if p.Value > a.max {
				a.max = p.Value
			}
		}
	}

	timestamps := make([]int64, 0, len(accs))
	for ts := range accs {
		timestamps = append(timestamps, ts)
	}
	sort.Slice(timestamps, func(i, j int) bool { return timestamps[i] < timestamps[j] })

	result := make([]storage.Point, 0, len(timestamps))
	for _, ts := range timestamps {
		a := accs[ts]
		var v float64
		switch op {
		case "sum":
			v = a.sum
		case "avg":
			v = a.sum / float64(a.count)
		case "max":
			v = a.max
		case "min":
			v = a.min
		case "count":
			v = float64(a.count)
		}
		result = append(result, storage.Point{Timestamp: ts, Value: v})
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
