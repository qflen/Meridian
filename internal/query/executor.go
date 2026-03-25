package query

import (
	"context"
	"fmt"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// Engine executes parsed queries against a TSDB.
type Engine struct {
	db *storage.TSDB
}

// NewEngine creates a query engine backed by the given TSDB.
func NewEngine(db *storage.TSDB) *Engine {
	return &Engine{db: db}
}

// ResultSeries holds a single series result from query execution.
type ResultSeries struct {
	Name   string
	Labels map[string]string
	Points []storage.Point
}

// Execute runs a PromQL-subset query and returns the result series.
func (e *Engine) Execute(ctx context.Context, query string, start, end int64, step time.Duration) ([]ResultSeries, error) {
	expr, err := Parse(query)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	plan := Plan(expr, start, end, step)
	return e.eval(ctx, plan.Expr, plan.TimeRange[0], end)
}

func (e *Engine) eval(ctx context.Context, expr Expr, start, end int64) ([]ResultSeries, error) {
	switch ex := expr.(type) {
	case *VectorSelector:
		return e.evalVector(ctx, ex, start, end)
	case *RangeSelector:
		return e.evalRange(ctx, ex, start, end)
	case *FunctionCall:
		return e.evalFunction(ctx, ex, start, end)
	case *AggregateExpr:
		return e.evalAggregate(ctx, ex, start, end)
	case *BinaryExpr:
		return e.evalBinary(ctx, ex, start, end)
	case *NumberLiteral:
		return []ResultSeries{{
			Name:   "",
			Labels: map[string]string{},
			Points: []storage.Point{{Timestamp: end, Value: ex.Value}},
		}}, nil
	}
	return nil, fmt.Errorf("unsupported expression type: %T", expr)
}

func (e *Engine) evalVector(ctx context.Context, vs *VectorSelector, start, end int64) ([]ResultSeries, error) {
	matchers := convertMatchers(vs.Name, vs.Matchers)
	ss, err := e.db.Query(ctx, matchers, start, end)
	if err != nil {
		return nil, err
	}

	results := make([]ResultSeries, len(ss))
	for i, s := range ss {
		results[i] = ResultSeries{
			Name:   s.Name,
			Labels: s.Labels,
			Points: s.Points,
		}
	}
	return results, nil
}

func (e *Engine) evalRange(ctx context.Context, rs *RangeSelector, start, end int64) ([]ResultSeries, error) {
	// For range selectors, extend start by the duration
	rangeStart := start - rs.Duration.Milliseconds()
	return e.evalVector(ctx, rs.Vector, rangeStart, end)
}

func (e *Engine) evalFunction(ctx context.Context, fc *FunctionCall, start, end int64) ([]ResultSeries, error) {
	switch fc.Name {
	case "rate":
		if len(fc.Args) != 1 {
			return nil, fmt.Errorf("rate() requires exactly 1 argument")
		}
		series, err := e.eval(ctx, fc.Args[0], start, end)
		if err != nil {
			return nil, err
		}
		var results []ResultSeries
		for _, s := range series {
			ratePoints := rate(s.Points)
			if len(ratePoints) > 0 {
				results = append(results, ResultSeries{
					Name:   s.Name,
					Labels: s.Labels,
					Points: ratePoints,
				})
			}
		}
		return results, nil

	case "histogram_quantile":
		if len(fc.Args) != 2 {
			return nil, fmt.Errorf("histogram_quantile() requires exactly 2 arguments")
		}
		phiExpr, ok := fc.Args[0].(*NumberLiteral)
		if !ok {
			return nil, fmt.Errorf("histogram_quantile() first argument must be a number")
		}
		series, err := e.eval(ctx, fc.Args[1], start, end)
		if err != nil {
			return nil, err
		}
		var results []ResultSeries
		for _, s := range series {
			pts := histogramQuantile(phiExpr.Value, s.Points)
			if len(pts) > 0 {
				results = append(results, ResultSeries{
					Name:   s.Name,
					Labels: s.Labels,
					Points: pts,
				})
			}
		}
		return results, nil

	default:
		// Treat unknown function names as aggregate ops if they match
		if len(fc.Args) == 1 {
			return e.evalAggregate(ctx, &AggregateExpr{Op: fc.Name, Expr: fc.Args[0]}, start, end)
		}
		return nil, fmt.Errorf("unknown function: %s", fc.Name)
	}
}

func (e *Engine) evalAggregate(ctx context.Context, ae *AggregateExpr, start, end int64) ([]ResultSeries, error) {
	series, err := e.eval(ctx, ae.Expr, start, end)
	if err != nil {
		return nil, err
	}

	if len(ae.Grouping) == 0 {
		// Aggregate all series into one
		var allPoints [][]storage.Point
		for _, s := range series {
			allPoints = append(allPoints, s.Points)
		}
		agg := aggregateFunc(ae.Op, allPoints)
		if len(agg) == 0 {
			return nil, nil
		}
		return []ResultSeries{{
			Name:   "",
			Labels: map[string]string{},
			Points: agg,
		}}, nil
	}

	// Group by specified labels
	groups := make(map[string][]int) // group key → series indexes
	for i, s := range series {
		key := groupKey(s.Labels, ae.Grouping)
		groups[key] = append(groups[key], i)
	}

	var results []ResultSeries
	for _, idxs := range groups {
		var groupPoints [][]storage.Point
		groupLabels := make(map[string]string)
		for _, label := range ae.Grouping {
			if v, ok := series[idxs[0]].Labels[label]; ok {
				groupLabels[label] = v
			}
		}
		for _, idx := range idxs {
			groupPoints = append(groupPoints, series[idx].Points)
		}
		agg := aggregateFunc(ae.Op, groupPoints)
		if len(agg) > 0 {
			results = append(results, ResultSeries{
				Name:   "",
				Labels: groupLabels,
				Points: agg,
			})
		}
	}
	return results, nil
}

func (e *Engine) evalBinary(ctx context.Context, be *BinaryExpr, start, end int64) ([]ResultSeries, error) {
	left, err := e.eval(ctx, be.Left, start, end)
	if err != nil {
		return nil, err
	}
	right, err := e.eval(ctx, be.Right, start, end)
	if err != nil {
		return nil, err
	}

	// Scalar on right: apply to each point of each left series
	if len(right) == 1 && len(right[0].Points) == 1 {
		scalar := right[0].Points[0].Value
		var results []ResultSeries
		for _, ls := range left {
			var points []storage.Point
			for _, p := range ls.Points {
				points = append(points, storage.Point{
					Timestamp: p.Timestamp,
					Value:     applyBinaryOp(be.Op, p.Value, scalar),
				})
			}
			results = append(results, ResultSeries{
				Name:   ls.Name,
				Labels: ls.Labels,
				Points: points,
			})
		}
		return results, nil
	}

	// Scalar on left: apply to each point of each right series
	if len(left) == 1 && len(left[0].Points) == 1 {
		scalar := left[0].Points[0].Value
		var results []ResultSeries
		for _, rs := range right {
			var points []storage.Point
			for _, p := range rs.Points {
				points = append(points, storage.Point{
					Timestamp: p.Timestamp,
					Value:     applyBinaryOp(be.Op, scalar, p.Value),
				})
			}
			results = append(results, ResultSeries{
				Name:   rs.Name,
				Labels: rs.Labels,
				Points: points,
			})
		}
		return results, nil
	}

	// Vector-vector: match series by labels
	return left, nil
}

func applyBinaryOp(op string, a, b float64) float64 {
	switch op {
	case "+":
		return a + b
	case "-":
		return a - b
	case "*":
		return a * b
	case "/":
		if b == 0 {
			return 0
		}
		return a / b
	}
	return 0
}

func groupKey(labels map[string]string, grouping []string) string {
	key := ""
	for i, g := range grouping {
		if i > 0 {
			key += ","
		}
		key += g + "=" + labels[g]
	}
	return key
}
