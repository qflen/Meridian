package query

import (
	"math"
	"sort"
	"strconv"
	"strings"

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

// histogramQuantile computes the phi-quantile from a set of classic histogram
// bucket series. The input series are the `_bucket` series distinguished by
// their `le` label; they are grouped by their remaining labels, and within each
// group, for each timestamp, the cumulative bucket counts are interpolated to
// locate the value at rank phi.
func histogramQuantile(phi float64, series []ResultSeries) []ResultSeries {
	if phi < 0 || phi > 1 {
		return nil
	}

	// Group bucket series by every label except `le`.
	type group struct {
		labels  map[string]string
		buckets []ResultSeries
	}
	groups := make(map[string]*group)
	var order []string
	for _, s := range series {
		if _, ok := s.Labels["le"]; !ok {
			continue // not a histogram bucket
		}
		key := histogramGroupKey(s.Labels)
		g, ok := groups[key]
		if !ok {
			g = &group{labels: labelsExcluding(s.Labels, "le", "__name__")}
			groups[key] = g
			order = append(order, key)
		}
		g.buckets = append(g.buckets, s)
	}

	var results []ResultSeries
	for _, key := range order {
		g := groups[key]

		// Parse each bucket's le and index its values by timestamp.
		type bucketSeries struct {
			upperBound float64
			byTS       map[int64]float64
		}
		bss := make([]bucketSeries, 0, len(g.buckets))
		tsSet := map[int64]bool{}
		for _, b := range g.buckets {
			le, err := strconv.ParseFloat(b.Labels["le"], 64)
			if err != nil {
				continue // unparseable upper bound — ignore this bucket
			}
			m := make(map[int64]float64, len(b.Points))
			for _, p := range b.Points {
				m[p.Timestamp] = p.Value
				tsSet[p.Timestamp] = true
			}
			bss = append(bss, bucketSeries{upperBound: le, byTS: m})
		}
		sort.Slice(bss, func(i, j int) bool { return bss[i].upperBound < bss[j].upperBound })

		timestamps := make([]int64, 0, len(tsSet))
		for ts := range tsSet {
			timestamps = append(timestamps, ts)
		}
		sort.Slice(timestamps, func(i, j int) bool { return timestamps[i] < timestamps[j] })

		var points []storage.Point
		for _, ts := range timestamps {
			buckets := make([]bucket, 0, len(bss))
			for _, bs := range bss {
				if v, ok := bs.byTS[ts]; ok {
					buckets = append(buckets, bucket{upperBound: bs.upperBound, count: v})
				}
			}
			q := bucketQuantile(phi, buckets)
			if math.IsNaN(q) {
				continue
			}
			points = append(points, storage.Point{Timestamp: ts, Value: q})
		}
		if len(points) > 0 {
			results = append(results, ResultSeries{Name: "", Labels: g.labels, Points: points})
		}
	}
	return results
}

// bucket is a single histogram bucket: an inclusive upper bound (le) and the
// cumulative observation count at or below it.
type bucket struct {
	upperBound float64
	count      float64
}

// bucketQuantile interpolates the phi-quantile within a set of cumulative
// buckets sorted ascending by upper bound. The highest bucket must be +Inf.
func bucketQuantile(phi float64, buckets []bucket) float64 {
	if len(buckets) < 2 || !math.IsInf(buckets[len(buckets)-1].upperBound, +1) {
		return math.NaN()
	}

	// Cumulative counts must be non-decreasing; clamp any float jitter.
	for i := 1; i < len(buckets); i++ {
		if buckets[i].count < buckets[i-1].count {
			buckets[i].count = buckets[i-1].count
		}
	}

	total := buckets[len(buckets)-1].count
	if total == 0 {
		return math.NaN()
	}
	rank := phi * total

	b := 0
	for b < len(buckets)-1 && buckets[b].count < rank {
		b++
	}
	if b == len(buckets)-1 {
		// Rank lands in the +Inf bucket; the best estimate is the highest finite bound.
		return buckets[len(buckets)-2].upperBound
	}
	if b == 0 {
		if buckets[0].upperBound <= 0 {
			return buckets[0].upperBound
		}
		// Interpolate from an implicit lower bound of 0.
		return buckets[0].upperBound * (rank / buckets[0].count)
	}
	lower, upper := buckets[b-1].upperBound, buckets[b].upperBound
	cumLower, cumUpper := buckets[b-1].count, buckets[b].count
	return lower + (upper-lower)*(rank-cumLower)/(cumUpper-cumLower)
}

// histogramGroupKey is a stable key over a bucket series' labels, excluding le,
// so series that differ only by their bucket boundary share a group.
func histogramGroupKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if k == "le" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(labels[k])
		sb.WriteByte(0)
	}
	return sb.String()
}

// labelsExcluding returns a copy of labels with the named labels removed.
func labelsExcluding(labels map[string]string, exclude ...string) map[string]string {
	ex := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		ex[e] = true
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if !ex[k] {
			out[k] = v
		}
	}
	return out
}
