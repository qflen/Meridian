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

// ResolutionDataSource is an optional capability: a DataSource that can serve a query
// from a chosen rollup resolution and report which resolutions currently have data.
// When the backing store implements it, the engine selects a resolution from the query
// span/step and reads coarse rollup points for wide spans; otherwise every query reads
// raw. *storage.TSDB implements this; the remote StorageClient does not (so the cluster
// path reads raw — see ADR-011).
type ResolutionDataSource interface {
	DataSource
	// QueryResolution serves [start,end] from the given rollup resolution (ms); a
	// resolution of 0 is equivalent to Query (raw).
	QueryResolution(ctx context.Context, matchers []storage.LabelMatcher, start, end, resolution int64) (storage.SeriesSet, error)
	// RollupResolutions returns the resolutions (ms) that currently have rollup data.
	RollupResolutions() []int64
}

// QueryMeta reports how a query was served: the resolution chosen (0 = raw) and the
// number of points fetched from storage across all selectors. The transparent
// resolution selection is observable here without changing the result shape.
type QueryMeta struct {
	ResolutionMs int64
	PointsRead   int
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

const (
	// defaultLookbackDelta is the staleness window for instant-vector selection:
	// at evaluation time t a series contributes its most recent sample with
	// timestamp in [t-delta, t]. A series with no sample in that window produces a
	// gap (no point), never a zero. 5m matches Prometheus's default look-back delta.
	// See ADR-014.
	defaultLookbackDelta = 5 * time.Minute

	// defaultStepPoints is the target number of points across [start,end] when the
	// caller does not specify a step; the step is sized so the range yields about
	// this many evaluations.
	defaultStepPoints = 250

	// minStep floors the auto-derived step so a tiny range cannot explode into
	// sub-second sampling.
	minStep = time.Second

	// maxStepCount caps the number of evaluation steps per query. It bounds both
	// CPU and output size, pre-empting a DoS via attacker-controlled start/end/step.
	maxStepCount = 11000
)

// Execute evaluates a PromQL-subset query as a range query: it evaluates the
// expression as an instant query at each step t in {start, start+step, ..., end}
// and assembles a matrix — one point list per series, keyed by label set, in time
// order. A series contributes a point at step t only when it has data there
// (within the look-back window for instant vectors), so gaps remain gaps.
//
// When start == end a single instant is evaluated. When step <= 0 a step is
// derived so the range yields roughly defaultStepPoints points (floored at 1s).
func (e *Engine) Execute(ctx context.Context, query string, start, end int64, step time.Duration) ([]ResultSeries, error) {
	res, _, err := e.ExecuteWithMeta(ctx, query, start, end, step)
	return res, err
}

// ExecuteWithMeta is Execute plus QueryMeta describing how the query was served (the
// resolution selected and the points fetched from storage). Callers that want to
// surface the resolution selection (the HTTP API, the smoke harness) use this; the
// result series are identical to Execute.
func (e *Engine) ExecuteWithMeta(ctx context.Context, query string, start, end int64, step time.Duration) ([]ResultSeries, QueryMeta, error) {
	expr, err := Parse(query)
	if err != nil {
		return nil, QueryMeta{}, fmt.Errorf("parse: %w", err)
	}
	if start > end {
		return nil, QueryMeta{}, fmt.Errorf("invalid range: start %d is after end %d", start, end)
	}

	stepMs := step.Milliseconds()
	if stepMs <= 0 {
		stepMs = defaultStepMs(start, end)
	}

	// Number of steps on the grid {start, start+step, ..., <=end}. start==end gives
	// exactly one step (a single instant evaluation).
	nSteps := int((end-start)/stepMs) + 1
	if nSteps > maxStepCount {
		return nil, QueryMeta{}, fmt.Errorf("query would evaluate %d steps, exceeding the maximum of %d; widen the step or narrow the range", nSteps, maxStepCount)
	}

	ec, err := e.newEvalContext(ctx, expr, start, end, stepMs)
	if err != nil {
		return nil, QueryMeta{}, err
	}

	asm := newMatrixAssembler()
	for i := 0; i < nSteps; i++ {
		t := start + int64(i)*stepMs
		vec, err := e.evalInstant(ec, expr, t)
		if err != nil {
			return nil, QueryMeta{}, err
		}
		asm.add(vec)
	}
	return asm.matrix(), QueryMeta{ResolutionMs: ec.resolution, PointsRead: ec.pointsRead}, nil
}

// defaultStepMs sizes an auto step so [start,end] yields ~defaultStepPoints points,
// floored at minStep.
func defaultStepMs(start, end int64) int64 {
	floor := minStep.Milliseconds()
	span := end - start
	if span <= 0 {
		return floor
	}
	s := span / defaultStepPoints
	if s < floor {
		s = floor
	}
	return s
}

// evalContext carries per-Execute state: the request context, the look-back delta,
// and the data pre-fetched once per leaf selector. Per-step evaluation slices this
// in memory rather than re-querying storage, so a range query is one fetch per
// selector, not one per step.
type evalContext struct {
	ctx        context.Context
	lookback   int64                                 // staleness window, ms
	fetched    map[*VectorSelector]storage.SeriesSet // full-window data per leaf selector
	resolution int64                                 // rollup resolution served, 0 = raw
	pointsRead int                                   // points fetched from storage
}

// newEvalContext fetches every leaf selector's full needed window exactly once.
// The window spans [start - maxRange - lookback, end]: the planner widens the
// lower bound to start-maxRange (its block-pruning span, covering the widest range
// selector); extending it by the look-back delta also covers instant-vector
// staleness at t=start. Every step is then answered by slicing this single fetch —
// no storage round-trip per step (avoids an N+1 over steps). Matchers are pushed
// down per selector so each leaf prunes on its own predicates.
func (e *Engine) newEvalContext(ctx context.Context, expr Expr, start, end, stepMs int64) (*evalContext, error) {
	// Resolution selection is transparent: if the store can serve rollups, plan the
	// resolution from the span/step against the resolutions that have data.
	rds, hasResolution := e.ds.(ResolutionDataSource)
	var available []int64
	if hasResolution {
		available = rds.RollupResolutions()
	}
	plan := Plan(expr, start, end, time.Duration(stepMs)*time.Millisecond, available)

	// At a coarse resolution, rollup points are one window apart, so the staleness
	// window must be at least one window wide or most steps would fall in a gap.
	lookback := defaultLookbackDelta.Milliseconds()
	if plan.Resolution > lookback {
		lookback = plan.Resolution
	}

	ec := &evalContext{
		ctx:        ctx,
		lookback:   lookback,
		fetched:    make(map[*VectorSelector]storage.SeriesSet),
		resolution: plan.Resolution,
	}

	fetchStart := plan.TimeRange[0] - lookback
	fetchEnd := plan.TimeRange[1]
	for _, vs := range collectSelectors(expr) {
		if _, ok := ec.fetched[vs]; ok {
			continue // same selector node referenced twice — fetch once
		}
		var (
			ss  storage.SeriesSet
			err error
		)
		if plan.Resolution > 0 && hasResolution {
			ss, err = rds.QueryResolution(ctx, convertMatchers(vs.Name, vs.Matchers), fetchStart, fetchEnd, plan.Resolution)
		} else {
			ss, err = e.ds.Query(ctx, convertMatchers(vs.Name, vs.Matchers), fetchStart, fetchEnd)
		}
		if err != nil {
			return nil, err
		}
		// Per-step lookback and window slicing assume time-sorted points. Storage
		// already sorts, but guard against an unsorted DataSource defensively.
		for i := range ss {
			pts := ss[i].Points
			if !sort.SliceIsSorted(pts, func(a, b int) bool { return pts[a].Timestamp < pts[b].Timestamp }) {
				sort.Slice(pts, func(a, b int) bool { return pts[a].Timestamp < pts[b].Timestamp })
			}
			ec.pointsRead += len(pts)
		}
		ec.fetched[vs] = ss
	}
	return ec, nil
}

// collectSelectors walks the AST and returns every leaf vector selector, including
// the one wrapped by each range selector.
func collectSelectors(expr Expr) []*VectorSelector {
	var out []*VectorSelector
	var walk func(Expr)
	walk = func(ex Expr) {
		switch n := ex.(type) {
		case *VectorSelector:
			out = append(out, n)
		case *RangeSelector:
			out = append(out, n.Vector)
		case *FunctionCall:
			for _, a := range n.Args {
				walk(a)
			}
		case *AggregateExpr:
			walk(n.Expr)
			if n.Param != nil {
				walk(n.Param)
			}
		case *BinaryExpr:
			walk(n.Left)
			walk(n.Right)
		}
	}
	walk(expr)
	return out
}

// evalInstant evaluates expr at a single instant t and returns the instant vector
// as ResultSeries each carrying at most one point stamped at t (a range selector
// is the exception: it yields the raw windowed samples for a range function). All
// the per-timestamp helpers are reused unchanged; they collapse naturally to one
// point when each input series holds a single sample at t.
func (e *Engine) evalInstant(ec *evalContext, expr Expr, t int64) ([]ResultSeries, error) {
	if err := ec.ctx.Err(); err != nil {
		return nil, err
	}
	switch ex := expr.(type) {
	case *NumberLiteral:
		return []ResultSeries{{
			Name:   "",
			Labels: map[string]string{},
			Points: []storage.Point{{Timestamp: t, Value: ex.Value}},
		}}, nil
	case *VectorSelector:
		return e.instantVector(ec, ex, t), nil
	case *RangeSelector:
		return e.rangeVector(ec, ex, t), nil
	case *FunctionCall:
		return e.evalFunctionInstant(ec, ex, t)
	case *AggregateExpr:
		return e.evalAggregateInstant(ec, ex, t)
	case *BinaryExpr:
		return e.evalBinaryInstant(ec, ex, t)
	}
	return nil, fmt.Errorf("unsupported expression type: %T", expr)
}

// instantVector returns, for each matching series, its most recent sample with
// timestamp in [t-lookback, t], stamped at t. A series with no sample in that
// window contributes no point (a gap), per PromQL staleness — never a zero.
func (e *Engine) instantVector(ec *evalContext, vs *VectorSelector, t int64) []ResultSeries {
	ss := ec.fetched[vs]
	var out []ResultSeries
	for _, s := range ss {
		// Largest index with Timestamp <= t (points are time-sorted).
		i := sort.Search(len(s.Points), func(k int) bool { return s.Points[k].Timestamp > t }) - 1
		if i < 0 {
			continue // no sample at or before t
		}
		if s.Points[i].Timestamp < t-ec.lookback {
			continue // newest sample is older than the look-back delta → stale gap
		}
		out = append(out, ResultSeries{
			Name:   s.Name,
			Labels: s.Labels,
			Points: []storage.Point{{Timestamp: t, Value: s.Points[i].Value}},
		})
	}
	return out
}

// rangeVector returns, for each matching series, the raw samples in (t-rangeDur, t]
// with their original timestamps — the half-open window Prometheus feeds to range
// functions. It is only meaningful as a function argument (e.g. rate(x[5m])); the
// pure helper is then called with window end = t.
func (e *Engine) rangeVector(ec *evalContext, rs *RangeSelector, t int64) []ResultSeries {
	ss := ec.fetched[rs.Vector]
	lo := t - rs.Duration.Milliseconds()
	var out []ResultSeries
	for _, s := range ss {
		a := sort.Search(len(s.Points), func(k int) bool { return s.Points[k].Timestamp > lo }) // exclude lo
		b := sort.Search(len(s.Points), func(k int) bool { return s.Points[k].Timestamp > t })  // include t
		if a >= b {
			continue
		}
		pts := make([]storage.Point, b-a)
		copy(pts, s.Points[a:b])
		out = append(out, ResultSeries{Name: s.Name, Labels: s.Labels, Points: pts})
	}
	return out
}

func (e *Engine) evalFunctionInstant(ec *evalContext, fc *FunctionCall, t int64) ([]ResultSeries, error) {
	switch fc.Name {
	case "rate":
		if len(fc.Args) != 1 {
			return nil, fmt.Errorf("rate() requires exactly 1 argument")
		}
		rs, ok := fc.Args[0].(*RangeSelector)
		if !ok {
			return nil, fmt.Errorf("rate() requires a range vector argument, e.g. rate(metric[5m])")
		}
		series, err := e.evalInstant(ec, fc.Args[0], t)
		if err != nil {
			return nil, err
		}
		rangeMs := rs.Duration.Milliseconds()
		var results []ResultSeries
		for _, s := range series {
			v, ok := rate(s.Points, rangeMs, t)
			if !ok {
				continue
			}
			results = append(results, ResultSeries{
				Name:   s.Name,
				Labels: s.Labels,
				Points: []storage.Point{{Timestamp: t, Value: v}},
			})
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
		series, err := e.evalInstant(ec, fc.Args[1], t)
		if err != nil {
			return nil, err
		}
		return histogramQuantile(phiExpr.Value, series), nil

	default:
		// Treat an unknown single-argument function name as an aggregate op.
		if len(fc.Args) == 1 {
			return e.evalAggregateInstant(ec, &AggregateExpr{Op: fc.Name, Expr: fc.Args[0]}, t)
		}
		return nil, fmt.Errorf("unknown function: %s", fc.Name)
	}
}

func (e *Engine) evalAggregateInstant(ec *evalContext, ae *AggregateExpr, t int64) ([]ResultSeries, error) {
	series, err := e.evalInstant(ec, ae.Expr, t)
	if err != nil {
		return nil, err
	}

	// topk/bottomk select whole series rather than collapsing them.
	if ae.Op == "topk" || ae.Op == "bottomk" {
		k := 0
		if ae.Param != nil {
			pv, err := e.evalInstant(ec, ae.Param, t)
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

func (e *Engine) evalBinaryInstant(ec *evalContext, be *BinaryExpr, t int64) ([]ResultSeries, error) {
	if !isBinaryOp(be.Op) {
		return nil, fmt.Errorf("unsupported binary operator %q", be.Op)
	}

	left, err := e.evalInstant(ec, be.Left, t)
	if err != nil {
		return nil, err
	}
	right, err := e.evalInstant(ec, be.Right, t)
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
			Points: []storage.Point{{Timestamp: t, Value: applyBinaryOp(be.Op, scalarValue(left), scalarValue(right))}},
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

// seriesSignature is a stable key over a full label set, including the metric
// name, used to identify a series across steps when assembling the matrix.
func seriesSignature(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
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

// matrixAssembler collects the per-step instant vectors into a matrix. Series are
// keyed by their full label set, kept in first-appearance order, and accumulate
// strictly time-increasing points (a step that omits a series simply leaves a gap;
// duplicate or out-of-order timestamps from a degenerate top-level range selector
// are dropped).
type matrixAssembler struct {
	order []string
	byKey map[string]*ResultSeries
}

func newMatrixAssembler() *matrixAssembler {
	return &matrixAssembler{byKey: make(map[string]*ResultSeries)}
}

func (m *matrixAssembler) add(vec []ResultSeries) {
	for _, s := range vec {
		key := seriesSignature(s.Labels)
		rs, ok := m.byKey[key]
		if !ok {
			rs = &ResultSeries{Name: s.Name, Labels: s.Labels}
			m.byKey[key] = rs
			m.order = append(m.order, key)
		}
		for _, p := range s.Points {
			if n := len(rs.Points); n > 0 && p.Timestamp <= rs.Points[n-1].Timestamp {
				continue // keep points strictly increasing in time
			}
			rs.Points = append(rs.Points, p)
		}
	}
}

func (m *matrixAssembler) matrix() []ResultSeries {
	out := make([]ResultSeries, 0, len(m.order))
	for _, key := range m.order {
		out = append(out, *m.byKey[key])
	}
	return out
}
