package query

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// DataSource abstracts the storage layer so the engine can query local or remote data.
type DataSource interface {
	Query(ctx context.Context, matchers []storage.LabelMatcher, start, end int64) (storage.SeriesSet, error)
}

// Engine executes parsed queries against a DataSource.
type Engine struct {
	ds DataSource
}

// NewEngine creates a query engine backed by the given DataSource.
// *storage.TSDB implements DataSource directly.
func NewEngine(ds DataSource) *Engine {
	return &Engine{ds: ds}
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
	// Pass the original start through. The planner's TimeRange records the widest
	// data span a future stepped evaluator will need to fetch (start - maxRange);
	// the per-selector range window is applied exactly once, in evalRange, so it
	// must not be pre-subtracted here as well.
	return e.eval(ctx, plan.Expr, start, end)
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
	ss, err := e.ds.Query(ctx, matchers, start, end)
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
	// A range vector m[d] evaluated at instant `end` covers (end-d, end]. We anchor
	// the window at `end` rather than `start` so rate()/range functions read exactly
	// one selector-width of samples regardless of how wide [start,end] is. This is
	// the single place the range duration is subtracted (see Execute).
	rangeStart := end - rs.Duration.Milliseconds()
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

	// topk/bottomk select whole series rather than collapsing them.
	if ae.Op == "topk" || ae.Op == "bottomk" {
		k := 0
		if ae.Param != nil {
			pv, err := e.eval(ctx, ae.Param, start, end)
			if err != nil {
				return nil, err
			}
			k = int(scalarValue(pv))
		}
		return e.evalTopK(ae.Op, k, series, ae.Grouping, ae.Without), nil
	}

	// Plain aggregation with no grouping collapses everything into one series.
	if len(ae.Grouping) == 0 && !ae.Without {
		groupPoints := make([][]storage.Point, len(series))
		for i, s := range series {
			groupPoints[i] = s.Points
		}
		agg := aggregateFunc(ae.Op, groupPoints)
		if len(agg) == 0 {
			return nil, nil
		}
		return []ResultSeries{{Name: "", Labels: map[string]string{}, Points: agg}}, nil
	}

	// Group by an include-list (by) or exclude-list (without).
	type group struct {
		labels map[string]string
		points [][]storage.Point
	}
	groups := make(map[string]*group)
	for _, s := range series {
		gl := groupingLabels(s.Labels, ae.Grouping, ae.Without)
		key := labelSignature(gl)
		g, ok := groups[key]
		if !ok {
			g = &group{labels: gl}
			groups[key] = g
		}
		g.points = append(g.points, s.Points)
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic output order

	var results []ResultSeries
	for _, key := range keys {
		g := groups[key]
		agg := aggregateFunc(ae.Op, g.points)
		if len(agg) > 0 {
			results = append(results, ResultSeries{Name: "", Labels: g.labels, Points: agg})
		}
	}
	return results, nil
}

// groupingLabels computes the label set that identifies a series' aggregation
// group. For by(), it keeps exactly the listed labels (empty string when a label
// is absent, so the group is well-formed). For without(), it keeps every label
// except the listed ones and the metric name.
func groupingLabels(labels map[string]string, grouping []string, without bool) map[string]string {
	out := map[string]string{}
	if without {
		excluded := map[string]bool{"__name__": true}
		for _, g := range grouping {
			excluded[g] = true
		}
		for k, v := range labels {
			if !excluded[k] {
				out[k] = v
			}
		}
		return out
	}
	for _, g := range grouping {
		out[g] = labels[g]
	}
	return out
}

// evalTopK keeps the k highest (topk) or lowest (bottomk) series per timestamp,
// within each group, preserving the selected series' labels.
func (e *Engine) evalTopK(op string, k int, series []ResultSeries, grouping []string, without bool) []ResultSeries {
	if k <= 0 || len(series) == 0 {
		return nil
	}

	// Partition series indexes into groups (one group for the whole vector when
	// there is no grouping clause).
	hasGrouping := len(grouping) > 0 || without
	groups := make(map[string][]int)
	var order []string
	for i, s := range series {
		key := ""
		if hasGrouping {
			key = labelSignature(groupingLabels(s.Labels, grouping, without))
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}

	selected := make([][]storage.Point, len(series))
	for _, key := range order {
		idxs := groups[key]
		// Index each member's points by timestamp and collect the timestamp union.
		byTS := make([]map[int64]float64, len(idxs))
		tsSet := map[int64]bool{}
		for j, idx := range idxs {
			m := make(map[int64]float64, len(series[idx].Points))
			for _, p := range series[idx].Points {
				m[p.Timestamp] = p.Value
				tsSet[p.Timestamp] = true
			}
			byTS[j] = m
		}
		timestamps := make([]int64, 0, len(tsSet))
		for ts := range tsSet {
			timestamps = append(timestamps, ts)
		}
		sort.Slice(timestamps, func(a, b int) bool { return timestamps[a] < timestamps[b] })

		for _, ts := range timestamps {
			type sv struct {
				local int
				value float64
			}
			ranked := make([]sv, 0, len(idxs))
			for j := range idxs {
				if v, ok := byTS[j][ts]; ok {
					ranked = append(ranked, sv{j, v})
				}
			}
			sort.SliceStable(ranked, func(a, b int) bool {
				if op == "bottomk" {
					return ranked[a].value < ranked[b].value
				}
				return ranked[a].value > ranked[b].value
			})
			limit := k
			if limit > len(ranked) {
				limit = len(ranked)
			}
			for _, r := range ranked[:limit] {
				idx := idxs[r.local]
				selected[idx] = append(selected[idx], storage.Point{Timestamp: ts, Value: r.value})
			}
		}
	}

	var results []ResultSeries
	for i, pts := range selected {
		if len(pts) == 0 {
			continue
		}
		results = append(results, ResultSeries{Name: series[i].Name, Labels: series[i].Labels, Points: pts})
	}
	return results
}

func (e *Engine) evalBinary(ctx context.Context, be *BinaryExpr, start, end int64) ([]ResultSeries, error) {
	if !isBinaryOp(be.Op) {
		return nil, fmt.Errorf("unsupported binary operator %q", be.Op)
	}

	left, err := e.eval(ctx, be.Left, start, end)
	if err != nil {
		return nil, err
	}
	right, err := e.eval(ctx, be.Right, start, end)
	if err != nil {
		return nil, err
	}

	leftScalar := isScalarExpr(be.Left)
	rightScalar := isScalarExpr(be.Right)

	switch {
	case leftScalar && rightScalar:
		// scalar OP scalar → scalar
		return []ResultSeries{{
			Name:   "",
			Labels: map[string]string{},
			Points: []storage.Point{{Timestamp: end, Value: applyBinaryOp(be.Op, scalarValue(left), scalarValue(right))}},
		}}, nil
	case rightScalar:
		return scaleSeries(left, be.Op, scalarValue(right), false), nil
	case leftScalar:
		return scaleSeries(right, be.Op, scalarValue(left), true), nil
	default:
		return vectorMatch(be.Op, left, right), nil
	}
}

// isBinaryOp reports whether op is a supported arithmetic operator.
func isBinaryOp(op string) bool {
	switch op {
	case "+", "-", "*", "/":
		return true
	}
	return false
}

// isScalarExpr reports whether expr evaluates to a PromQL scalar (a literal or
// an arithmetic combination of scalars) rather than an instant vector.
func isScalarExpr(expr Expr) bool {
	switch e := expr.(type) {
	case *NumberLiteral:
		return true
	case *BinaryExpr:
		return isScalarExpr(e.Left) && isScalarExpr(e.Right)
	}
	return false
}

// scalarValue extracts the single value from a scalar result.
func scalarValue(series []ResultSeries) float64 {
	if len(series) == 0 || len(series[0].Points) == 0 {
		return math.NaN()
	}
	return series[0].Points[0].Value
}

// scaleSeries applies a scalar to every point of every series. When scalarLeft
// is true the scalar is the left operand (scalar OP series), else the right.
func scaleSeries(series []ResultSeries, op string, scalar float64, scalarLeft bool) []ResultSeries {
	results := make([]ResultSeries, 0, len(series))
	for _, s := range series {
		points := make([]storage.Point, len(s.Points))
		for i, p := range s.Points {
			v := applyBinaryOp(op, p.Value, scalar)
			if scalarLeft {
				v = applyBinaryOp(op, scalar, p.Value)
			}
			points[i] = storage.Point{Timestamp: p.Timestamp, Value: v}
		}
		results = append(results, ResultSeries{Name: s.Name, Labels: s.Labels, Points: points})
	}
	return results
}

// vectorMatch applies op between two instant vectors, pairing series with an
// identical label set (the metric name is ignored) and timestamps that align.
// Unmatched series and unmatched timestamps are dropped, per PromQL.
func vectorMatch(op string, left, right []ResultSeries) []ResultSeries {
	index := make(map[string]ResultSeries, len(right))
	for _, rs := range right {
		index[labelSignature(rs.Labels)] = rs
	}

	var results []ResultSeries
	for _, ls := range left {
		rs, ok := index[labelSignature(ls.Labels)]
		if !ok {
			continue
		}
		rpts := make(map[int64]float64, len(rs.Points))
		for _, p := range rs.Points {
			rpts[p.Timestamp] = p.Value
		}
		var points []storage.Point
		for _, lp := range ls.Points {
			rv, ok := rpts[lp.Timestamp]
			if !ok {
				continue
			}
			points = append(points, storage.Point{Timestamp: lp.Timestamp, Value: applyBinaryOp(op, lp.Value, rv)})
		}
		if len(points) > 0 {
			results = append(results, ResultSeries{Name: "", Labels: dropName(ls.Labels), Points: points})
		}
	}
	return results
}

// labelSignature is a stable key over a label set, excluding the metric name,
// used to pair series in a vector-to-vector operation.
func labelSignature(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if k == "__name__" {
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

// dropName returns a copy of labels without the metric-name label, matching
// PromQL's rule that arithmetic between two vectors drops __name__.
func dropName(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if k == "__name__" {
			continue
		}
		out[k] = v
	}
	return out
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
		// Let IEEE-754 define the edges: x/0 = ±Inf, 0/0 = NaN.
		return a / b
	}
	return math.NaN()
}
