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

// Plan creates a query plan from an AST expression and time parameters. available is the
// set of rollup resolutions (ms) that currently have data; increaseAvailable is the subset
// whose counter-increase column is complete (used to serve rate). An empty set (or a short
// span, or a raw-only range query) leaves Resolution at 0 (raw).
func Plan(expr Expr, start, end int64, step time.Duration, available, increaseAvailable []int64) *QueryPlan {
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
	rangeMs := rangeDur.Milliseconds()
	if rangeMs > 0 {
		plan.TimeRange[0] = start - rangeMs
	}

	// A range query can still be served from a coarse tier when every range selector is
	// wrapped by a function whose aggregate is stored per window — the *_over_time family
	// reads its matching aggregate column, rate the counter-increase column. Otherwise (a
	// bare range selector, last_over_time, an unknown function) it forces raw. A query that
	// reads rate is restricted to the increase-capable tiers. See ADR-025.
	rangeCoarse := true
	avail := available
	if rangeMs > 0 {
		hasRate, allEligible := classifyRange(expr)
		rangeCoarse = allEligible
		if hasRate {
			avail = increaseAvailable
		}
	}
	plan.Resolution = selectResolution(start, end, step.Milliseconds(), rangeMs, rangeCoarse, avail)

	return plan
}

// selectResolution picks the coarsest available rollup resolution that still fits the
// query: it must not be coarser than the step (no upsampling), and must yield at least
// minCoarsePoints windows over the span. A range query is served coarse only when
// rangeCoarse is set (every range selector maps to a stored aggregate column — the
// *_over_time family, or rate via the increase column); in that case the resolution must
// also be no coarser than the range so each step's window contains at least one rollup
// window. A range query that is not rangeCoarse, or a span/step that does not justify a
// coarse tier, falls back to raw (0). See ADR-025.
func selectResolution(start, end, stepMs, rangeMs int64, rangeCoarse bool, available []int64) int64 {
	if stepMs <= 0 {
		return 0
	}
	if rangeMs > 0 && !rangeCoarse {
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
		if r > stepMs {
			continue // coarser than the step would upsample
		}
		if rangeMs > 0 && r > rangeMs {
			continue // a range window must hold at least one rollup window
		}
		if span/r >= minCoarsePoints {
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

// coarseEligibleFunc reports whether a function's range-vector argument can be served
// from a coarse rollup tier, because the function's per-window aggregate is stored as a
// rollup column. The *_over_time family qualifies (last_over_time does not — there is no
// stored "last" column); rate qualifies via the counter-increase column (ADR-025).
func coarseEligibleFunc(name string) bool {
	switch name {
	case "rate", "avg_over_time", "min_over_time", "max_over_time", "sum_over_time", "count_over_time":
		return true
	}
	return false
}

// classifyRange reports, for an expression containing range selectors, whether every range
// selector is the direct argument of a coarse-eligible function (allEligible) and whether
// any of them is rate (hasRate). A bare range selector, or one wrapped by last_over_time /
// an unknown function, clears allEligible — a single global resolution is chosen per query,
// so one raw-only range vector keeps the whole query on raw. hasRate steers resolution
// selection to the increase-capable tiers, which carry the column rate needs.
func classifyRange(expr Expr) (hasRate, allEligible bool) {
	allEligible = true
	var walk func(e Expr, parentCoarse bool)
	walk = func(e Expr, parentCoarse bool) {
		switch n := e.(type) {
		case *RangeSelector:
			if !parentCoarse {
				allEligible = false
			}
		case *FunctionCall:
			if n.Name == "rate" {
				hasRate = true
			}
			ce := coarseEligibleFunc(n.Name)
			for _, a := range n.Args {
				walk(a, ce)
			}
		case *AggregateExpr:
			walk(n.Expr, false)
			if n.Param != nil {
				walk(n.Param, false)
			}
		case *BinaryExpr:
			walk(n.Left, false)
			walk(n.Right, false)
		}
	}
	walk(expr, false)
	return
}

// aggForFunc maps a function name to the rollup aggregate column its range-vector
// argument should read at a coarse resolution. Functions that are not coarse-eligible
// default to AggAvg; their selectors are fetched raw anyway (the planner forces raw), so
// the choice is moot.
func aggForFunc(name string) storage.RollupAggregate {
	switch name {
	case "min_over_time":
		return storage.AggMin
	case "max_over_time":
		return storage.AggMax
	case "sum_over_time":
		return storage.AggSum
	case "count_over_time":
		return storage.AggCount
	case "rate":
		return storage.AggIncrease
	default: // avg_over_time, last_over_time, bare selectors, histogram buckets
		return storage.AggAvg
	}
}

// selectorAggregates annotates every leaf vector selector with the rollup aggregate
// column it must read, derived from the function that wraps it. A selector outside any
// coarse-eligible function reads AggAvg (the instant-vector value). Each selector node is
// created once by the parser, so a single column per node is unambiguous even when a
// query mixes functions (e.g. max_over_time(a[5m]) / count_over_time(b[5m])).
func selectorAggregates(expr Expr) map[*VectorSelector]storage.RollupAggregate {
	out := make(map[*VectorSelector]storage.RollupAggregate)
	var walk func(e Expr, agg storage.RollupAggregate)
	walk = func(e Expr, agg storage.RollupAggregate) {
		switch n := e.(type) {
		case *VectorSelector:
			out[n] = agg
		case *RangeSelector:
			out[n.Vector] = agg
		case *FunctionCall:
			a := aggForFunc(n.Name)
			for _, arg := range n.Args {
				walk(arg, a)
			}
		case *AggregateExpr:
			walk(n.Expr, storage.AggAvg)
			if n.Param != nil {
				walk(n.Param, storage.AggAvg)
			}
		case *BinaryExpr:
			walk(n.Left, storage.AggAvg)
			walk(n.Right, storage.AggAvg)
		}
	}
	walk(expr, storage.AggAvg)
	return out
}
