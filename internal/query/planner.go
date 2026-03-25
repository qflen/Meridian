package query

import (
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// QueryPlan describes how to execute a query.
type QueryPlan struct {
	Expr      Expr
	Start     int64
	End       int64
	Step      time.Duration
	Matchers  []storage.LabelMatcher
	TimeRange [2]int64 // [minTime, maxTime] for block pruning
}

// Plan creates a query plan from an AST expression and time parameters.
func Plan(expr Expr, start, end int64, step time.Duration) *QueryPlan {
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

	return plan
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
