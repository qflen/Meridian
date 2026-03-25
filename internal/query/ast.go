// Package query implements a PromQL-subset query engine for Meridian.
package query

import "time"

// Expr is the interface all AST nodes implement.
type Expr interface {
	exprNode()
}

// VectorSelector selects instant vectors by metric name and label matchers.
type VectorSelector struct {
	Name     string
	Matchers []Matcher
}

func (*VectorSelector) exprNode() {}

// RangeSelector wraps a vector selector with a duration for range queries.
type RangeSelector struct {
	Vector   *VectorSelector
	Duration time.Duration
}

func (*RangeSelector) exprNode() {}

// FunctionCall represents a function invocation like rate() or avg().
type FunctionCall struct {
	Name string
	Args []Expr
}

func (*FunctionCall) exprNode() {}

// AggregateExpr represents an aggregation with optional grouping.
type AggregateExpr struct {
	Op       string
	Expr     Expr
	Grouping []string // label names for by() clause
}

func (*AggregateExpr) exprNode() {}

// BinaryExpr represents a binary arithmetic operation.
type BinaryExpr struct {
	Op    string
	Left  Expr
	Right Expr
}

func (*BinaryExpr) exprNode() {}

// NumberLiteral represents a numeric constant.
type NumberLiteral struct {
	Value float64
}

func (*NumberLiteral) exprNode() {}

// Matcher specifies a label matching criterion.
type Matcher struct {
	Name  string
	Value string
	Type  MatcherType
}

// MatcherType defines how a label matcher compares values.
type MatcherType int

const (
	// MatcherEqual matches when label == value.
	MatcherEqual MatcherType = iota
	// MatcherNotEqual matches when label != value.
	MatcherNotEqual
	// MatcherRegexp matches when label =~ value.
	MatcherRegexp
	// MatcherNotRegexp matches when label !~ value.
	MatcherNotRegexp
)
