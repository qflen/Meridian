package storage

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// RollupSample is the aggregated value of one fixed downsampling window for a single
// series. All five aggregates are retained so a coarse resolution can answer a range
// of functions and so coarser tiers can be derived from finer ones without re-reading
// raw data (see ChainRollups). Timestamp is the window centre (windowStart+window/2),
// which keeps a coarse point visually placed inside the interval it summarises.
type RollupSample struct {
	Timestamp int64   // window centre, ms
	Min       float64 // smallest raw value in the window
	Max       float64 // largest raw value in the window
	Sum       float64 // sum of raw values
	Avg       float64 // count-weighted mean: Sum/Count
	Count     int     // number of raw samples in the window
}

// rollupAcc accumulates one window while aggregating.
type rollupAcc struct {
	min, max, sum float64
	count         int
}

func newRollupAcc() *rollupAcc {
	return &rollupAcc{min: math.Inf(1), max: math.Inf(-1)}
}

// finalize emits the accumulated windows in ascending time order, computing Avg as
// the count-weighted mean (Sum/Count).
func finalize(buckets map[int64]*rollupAcc, windowMs int64) []RollupSample {
	out := make([]RollupSample, 0, len(buckets))
	for windowStart, a := range buckets {
		out = append(out, RollupSample{
			Timestamp: windowStart + windowMs/2,
			Min:       a.min,
			Max:       a.max,
			Sum:       a.sum,
			Avg:       a.sum / float64(a.count),
			Count:     a.count,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	return out
}

// RollupPoints aggregates raw points into fixed windows of windowMs milliseconds,
// returning one RollupSample per non-empty window in ascending time order. Windows
// are globally aligned to multiples of windowMs so the same wall-clock window always
// has the same boundaries regardless of where a series' data starts — this is what
// lets independently-rolled spans (and the cascade) line up exactly.
//
// Points need not be sorted; aggregation is order-independent. Avg is the true mean
// of the raw values in the window (Sum/Count), not an average of partial averages.
func RollupPoints(points []Point, windowMs int64) []RollupSample {
	if len(points) == 0 || windowMs <= 0 {
		return nil
	}
	buckets := make(map[int64]*rollupAcc)
	for _, p := range points {
		windowStart := floorDiv(p.Timestamp, windowMs) * windowMs
		a := buckets[windowStart]
		if a == nil {
			a = newRollupAcc()
			buckets[windowStart] = a
		}
		if p.Value < a.min {
			a.min = p.Value
		}
		if p.Value > a.max {
			a.max = p.Value
		}
		a.sum += p.Value
		a.count++
	}
	return finalize(buckets, windowMs)
}

// ChainRollups derives a coarser tier from an already-rolled finer tier. The coarse
// interval MUST be an exact multiple of the finer interval the input was produced at
// (the cascade — 5s→1m→1h — guarantees this); under that condition the result is
// identical, for all five aggregates, to rolling the original raw data straight to
// the coarse interval:
//
//	Sum   = Σ fine.Sum
//	Count = Σ fine.Count
//	Min   = min fine.Min
//	Max   = max fine.Max
//	Avg   = Sum/Count            (count-weighted — a plain mean of fine averages is wrong)
//
// Each finer sample is placed by its window centre, which always falls inside the
// coarse window it belongs to, so the bucketing recovers the correct coarse window.
func ChainRollups(samples []RollupSample, windowMs int64) []RollupSample {
	if len(samples) == 0 || windowMs <= 0 {
		return nil
	}
	buckets := make(map[int64]*rollupAcc)
	for _, s := range samples {
		windowStart := floorDiv(s.Timestamp, windowMs) * windowMs
		a := buckets[windowStart]
		if a == nil {
			a = newRollupAcc()
			buckets[windowStart] = a
		}
		if s.Min < a.min {
			a.min = s.Min
		}
		if s.Max > a.max {
			a.max = s.Max
		}
		a.sum += s.Sum
		a.count += s.Count
	}
	return finalize(buckets, windowMs)
}

// floorDiv divides a by b rounding toward negative infinity, so window alignment is
// correct for negative timestamps (Go's integer division truncates toward zero).
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// ResolutionLabel renders a window size as a compact, filesystem-safe label
// ("5s", "1m", "1h", "90s") for rollup directory names and metric labels. It is
// cosmetic: the authoritative resolution is the integer stored in the block meta.
func ResolutionLabel(ms int64) string {
	if ms <= 0 {
		return "raw"
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	default:
		return fmt.Sprintf("%dms", ms)
	}
}
