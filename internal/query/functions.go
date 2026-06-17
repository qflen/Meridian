package query

import (
	"math"
	"sort"

	"github.com/meridiandb/meridian/internal/storage"
)

// rate computes the per-second average rate of increase of a counter over the
// window [end-rangeMs, end]. It divides the increase by the selector range (not
// the sample span), corrects for counter resets, and extrapolates to the window
// edges the way Prometheus does. The bool reports whether a value was produced.
//
// It takes the window end explicitly rather than reading it from the samples so
// it composes with a per-step matrix evaluator: each step calls rate() with the
// samples in that step's window and the step's timestamp.
func rate(points []storage.Point, rangeMs, end int64) (float64, bool) {
	if len(points) < 2 || rangeMs <= 0 {
		return 0, false
	}

	first := points[0]
	last := points[len(points)-1]
	sampledInterval := float64(last.Timestamp-first.Timestamp) / 1000.0
	if sampledInterval <= 0 {
		return 0, false
	}

	// Total increase across the window. On a counter reset (a decrease), the
	// post-reset value is itself the increase since the prior cycle, so add it.
	var increase float64
	prev := first.Value
	for _, p := range points[1:] {
		if p.Value < prev {
			increase += p.Value
		} else {
			increase += p.Value - prev
		}
		prev = p.Value
	}

	rangeStart := end - rangeMs
	durationToStart := float64(first.Timestamp-rangeStart) / 1000.0
	durationToEnd := float64(end-last.Timestamp) / 1000.0
	averageInterval := sampledInterval / float64(len(points)-1)

	// A counter cannot be negative, so if the first sample is small relative to
	// the increase, assume the series began at zero shortly before it and only
	// extrapolate back that far.
	if increase > 0 && first.Value >= 0 {
		durationToZero := sampledInterval * (first.Value / increase)
		if durationToZero < durationToStart {
			durationToStart = durationToZero
		}
	}

	// Extrapolate to each window edge, but no further than half a sample interval
	// past the outermost samples.
	threshold := averageInterval * 1.1
	extrapolated := sampledInterval
	if durationToStart < threshold {
		extrapolated += durationToStart
	} else {
		extrapolated += averageInterval / 2
	}
	if durationToEnd < threshold {
		extrapolated += durationToEnd
	} else {
		extrapolated += averageInterval / 2
	}

	factor := (extrapolated / sampledInterval) / (float64(rangeMs) / 1000.0)
	return increase * factor, true
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
