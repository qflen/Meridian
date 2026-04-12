package query

import (
	"testing"
)

func TestParserVectorSelector(t *testing.T) {
	expr, err := Parse("cpu_usage")
	if err != nil {
		t.Fatal(err)
	}
	vs, ok := expr.(*VectorSelector)
	if !ok {
		t.Fatalf("expected VectorSelector, got %T", expr)
	}
	if vs.Name != "cpu_usage" {
		t.Fatalf("name: %s", vs.Name)
	}
}

func TestParserVectorWithLabels(t *testing.T) {
	expr, err := Parse(`cpu_usage{host="web-01", region!="eu"}`)
	if err != nil {
		t.Fatal(err)
	}
	vs, ok := expr.(*VectorSelector)
	if !ok {
		t.Fatalf("expected VectorSelector, got %T", expr)
	}
	if len(vs.Matchers) != 2 {
		t.Fatalf("expected 2 matchers, got %d", len(vs.Matchers))
	}
	if vs.Matchers[0].Type != MatcherEqual || vs.Matchers[0].Name != "host" || vs.Matchers[0].Value != "web-01" {
		t.Fatalf("first matcher: %+v", vs.Matchers[0])
	}
	if vs.Matchers[1].Type != MatcherNotEqual {
		t.Fatalf("second matcher type: %d", vs.Matchers[1].Type)
	}
}

func TestParserRegexMatcher(t *testing.T) {
	expr, err := Parse(`cpu_usage{host=~"web-.*"}`)
	if err != nil {
		t.Fatal(err)
	}
	vs := expr.(*VectorSelector)
	if vs.Matchers[0].Type != MatcherRegexp {
		t.Fatalf("expected regex matcher")
	}
}

func TestParserRangeSelector(t *testing.T) {
	expr, err := Parse(`cpu_usage{host="web-01"}[5m]`)
	if err != nil {
		t.Fatal(err)
	}
	rs, ok := expr.(*RangeSelector)
	if !ok {
		t.Fatalf("expected RangeSelector, got %T", expr)
	}
	if rs.Duration.Minutes() != 5 {
		t.Fatalf("duration: %v", rs.Duration)
	}
	if rs.Vector.Name != "cpu_usage" {
		t.Fatalf("name: %s", rs.Vector.Name)
	}
}

func TestParserFunctionCall(t *testing.T) {
	expr, err := Parse("rate(http_requests_total[5m])")
	if err != nil {
		t.Fatal(err)
	}
	fc, ok := expr.(*FunctionCall)
	if !ok {
		t.Fatalf("expected FunctionCall, got %T", expr)
	}
	if fc.Name != "rate" {
		t.Fatalf("name: %s", fc.Name)
	}
	if len(fc.Args) != 1 {
		t.Fatalf("args: %d", len(fc.Args))
	}
	rs, ok := fc.Args[0].(*RangeSelector)
	if !ok {
		t.Fatalf("arg should be RangeSelector, got %T", fc.Args[0])
	}
	if rs.Vector.Name != "http_requests_total" {
		t.Fatalf("inner name: %s", rs.Vector.Name)
	}
}

func TestParserAggregateWithGrouping(t *testing.T) {
	expr, err := Parse("sum(rate(http_requests_total[5m])) by (method, status)")
	if err != nil {
		t.Fatal(err)
	}
	ae, ok := expr.(*AggregateExpr)
	if !ok {
		t.Fatalf("expected AggregateExpr, got %T", expr)
	}
	if ae.Op != "sum" {
		t.Fatalf("op: %s", ae.Op)
	}
	if len(ae.Grouping) != 2 {
		t.Fatalf("grouping: %v", ae.Grouping)
	}
	if ae.Grouping[0] != "method" || ae.Grouping[1] != "status" {
		t.Fatalf("grouping labels: %v", ae.Grouping)
	}
}

func TestParserAggregateWithoutGrouping(t *testing.T) {
	expr, err := Parse("avg(cpu_usage)")
	if err != nil {
		t.Fatal(err)
	}
	ae, ok := expr.(*AggregateExpr)
	if !ok {
		t.Fatalf("expected AggregateExpr, got %T", expr)
	}
	if ae.Op != "avg" {
		t.Fatalf("op: %s", ae.Op)
	}
	if len(ae.Grouping) != 0 {
		t.Fatalf("expected no grouping, got %v", ae.Grouping)
	}
}

func TestParserAggregateByBeforeArgs(t *testing.T) {
	expr, err := Parse("avg by (host)(cpu_usage_percent)")
	if err != nil {
		t.Fatal(err)
	}
	ae, ok := expr.(*AggregateExpr)
	if !ok {
		t.Fatalf("expected AggregateExpr, got %T", expr)
	}
	if ae.Op != "avg" {
		t.Fatalf("op: %s", ae.Op)
	}
	if len(ae.Grouping) != 1 || ae.Grouping[0] != "host" {
		t.Fatalf("expected grouping [host], got %v", ae.Grouping)
	}
	vs, ok := ae.Expr.(*VectorSelector)
	if !ok {
		t.Fatalf("inner expr should be VectorSelector, got %T", ae.Expr)
	}
	if vs.Name != "cpu_usage_percent" {
		t.Fatalf("inner name: %s", vs.Name)
	}
}

func TestParserBinaryExpression(t *testing.T) {
	expr, err := Parse("cpu_usage * 100")
	if err != nil {
		t.Fatal(err)
	}
	be, ok := expr.(*BinaryExpr)
	if !ok {
		t.Fatalf("expected BinaryExpr, got %T", expr)
	}
	if be.Op != "*" {
		t.Fatalf("op: %s", be.Op)
	}
	if _, ok := be.Left.(*VectorSelector); !ok {
		t.Fatalf("left should be VectorSelector")
	}
	if nl, ok := be.Right.(*NumberLiteral); !ok || nl.Value != 100 {
		t.Fatalf("right should be NumberLiteral(100)")
	}
}

func TestParserBinaryPrecedence(t *testing.T) {
	expr, err := Parse("a + b * c")
	if err != nil {
		t.Fatal(err)
	}
	be, ok := expr.(*BinaryExpr)
	if !ok {
		t.Fatalf("expected BinaryExpr, got %T", expr)
	}
	if be.Op != "+" {
		t.Fatalf("top op should be +, got %s", be.Op)
	}
	rbe, ok := be.Right.(*BinaryExpr)
	if !ok {
		t.Fatalf("right should be BinaryExpr")
	}
	if rbe.Op != "*" {
		t.Fatalf("right op should be *, got %s", rbe.Op)
	}
}

func TestParserComplexQuery(t *testing.T) {
	queries := []string{
		`rate(http_requests_total{method="GET"}[5m])`,
		`sum(rate(http_requests_total[5m])) by (method)`,
		`avg(cpu_usage{host=~"web-.*"}) by (host)`,
		`max(memory_bytes)`,
		`min(cpu_usage)`,
		`count(up)`,
		`cpu_usage * 100`,
		`memory_used / memory_total * 100`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			_, err := Parse(q)
			if err != nil {
				t.Fatalf("failed to parse %q: %v", q, err)
			}
		})
	}
}

func TestParserErrors(t *testing.T) {
	tests := []string{
		"{",
		"cpu{host=}",
		"rate(",
		"sum() by",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			_, err := Parse(input)
			if err == nil {
				t.Fatalf("expected parse error for %q", input)
			}
		})
	}
}
