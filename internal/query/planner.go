package query

import (
	"sort"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// minCoarsePoints is the minimum number of windows a coarse resolution must yield over
// the query span before it is chosen. It keeps a short span (or an over-large explicit
// step) on raw data rather than collapsing it to a handful of rollup points.
const minCoarsePoints = 4

// QueryPlan describes how to execute a query.
type QueryPlan struct {
	Expr      Expr
	Start     int64
	End       int64
	Step      time.Duration
	Matchers  []storage.LabelMatcher
	TimeRange [2]int64 // [minTime, maxTime] for block pruning
	// Resolution is the rollup window (ms) the executor should read from; 0 means raw.
	// It is chosen from the query span and step against the resolutions that actually
	// have data, and forced to raw for range selectors / rate().
	Resolution int64
}

// Plan creates a query plan from an AST expression and time parameters. available is
// the set of rollup resolutions (ms) that currently have data; an empty set (or a
// short span, or a range query) leaves Resolution at 0 (raw).
func Plan(expr Expr, start, end int64, step time.Duration, available []int64) *QueryPlan {
	plan := &QueryPlan{
		Expr:      expr,
		Start:     start,
		End:       end,
		Step:      step,
		TimeRange: [2]int64{start, end},
	}

	// Extract matchers from the expression for predicate pushdown
	plan.Matchers = extractMatchers(expr)

	// Extend time range for range selectors (e.g., [5m] needs 5m before start)
	rangeDur := maxRangeDuration(expr)
	if rangeDur > 0 {
		plan.TimeRange[0] = start - rangeDur.Milliseconds()
	}

	plan.Resolution = selectResolution(start, end, step.Milliseconds(), rangeDur.Milliseconds(), available)

	return plan
}

// selectResolution picks the coarsest available rollup resolution that still fits the
// query: it must not be coarser than the step (no upsampling), and must yield at least
// minCoarsePoints windows over the span. Range selectors / rate() force raw, because a
// rate over a downsampled counter is not generally correct (function-aware aggregate
// selection and rate-on-rollup are future work — see ADR-011). A span/step that does
// not justify a coarse tier falls back to raw (0).
func selectResolution(start, end, stepMs, rangeMs int64, available []int64) int64 {
	if rangeMs > 0 || stepMs <= 0 {
		return 0
	}
	span := end - start
	if span <= 0 || len(available) == 0 {
		return 0
	}
	res := append([]int64(nil), available...)
	sort.Slice(res, func(i, j int) bool { return res[i] < res[j] })

	best := int64(0)
	for _, r := range res {
		if r <= 0 {
			continue
		}
		if r <= stepMs && span/r >= minCoarsePoints {
			best = r // ascending scan keeps the coarsest qualifying resolution
		}
	}
	return best
}

func extractMatchers(expr Expr) []storage.LabelMatcher {
	switch e := expr.(type) {
	case *VectorSelector:
		return convertMatchers(e.Name, e.Matchers)
	case *RangeSelector:
		return convertMatchers(e.Vector.Name, e.Vector.Matchers)
	case *FunctionCall:
		if len(e.Args) > 0 {
			return extractMatchers(e.Args[0])
		}
	case *AggregateExpr:
		return extractMatchers(e.Expr)
	case *BinaryExpr:
		return extractMatchers(e.Left)
	}
	return nil
}

func convertMatchers(name string, matchers []Matcher) []storage.LabelMatcher {
	var result []storage.LabelMatcher
	if name != "" {
		result = append(result, storage.LabelMatcher{
			Name:  "__name__",
			Value: name,
			Type:  storage.MatchEqual,
		})
	}
	for _, m := range matchers {
		var mt storage.MatchType
		switch m.Type {
		case MatcherEqual:
			mt = storage.MatchEqual
		case MatcherNotEqual:
			mt = storage.MatchNotEqual
		case MatcherRegexp:
			mt = storage.MatchRegexp
		case MatcherNotRegexp:
			mt = storage.MatchNotRegexp
		}
		result = append(result, storage.LabelMatcher{
			Name:  m.Name,
			Value: m.Value,
			Type:  mt,
		})
	}
	return result
}

func maxRangeDuration(expr Expr) time.Duration {
	var max time.Duration
	switch e := expr.(type) {
	case *RangeSelector:
		if e.Duration > max {
			max = e.Duration
		}
	case *FunctionCall:
		for _, arg := range e.Args {
			if d := maxRangeDuration(arg); d > max {
				max = d
			}
		}
	case *AggregateExpr:
		if d := maxRangeDuration(e.Expr); d > max {
			max = d
		}
	case *BinaryExpr:
		if d := maxRangeDuration(e.Left); d > max {
			max = d
		}
		if d := maxRangeDuration(e.Right); d > max {
			max = d
		}
	}
	return max
}
