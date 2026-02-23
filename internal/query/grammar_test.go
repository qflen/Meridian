package query

import (
	"context"
	"math"
	"testing"
	"time"
)

// --- Task 8: compound and decimal durations ---

func TestLexerCompoundDuration(t *testing.T) {
	tests := []struct {
		input   string
		literal string
	}{
		{"1h30m", "1h30m"},
		{"1.5h", "1.5h"},
		{"2h15m30s", "2h15m30s"},
		{"7d", "7d"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			tokens, err := NewLexer(tc.input).Tokenize()
			if err != nil {
				t.Fatal(err)
			}
			// Expect exactly one duration token then EOF.
			if len(tokens) != 2 || tokens[0].Type != TokenDuration {
				t.Fatalf("expected single duration token, got %+v", tokens)
			}
			if tokens[0].Literal != tc.literal {
				t.Fatalf("literal: got %q, want %q", tokens[0].Literal, tc.literal)
			}
		})
	}
}

func TestParseDurationCompound(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"1h30m", 90 * time.Minute},
		{"1.5h", 90 * time.Minute},
		{"5m", 5 * time.Minute},
		{"7d", 7 * 24 * time.Hour},
		{"90s", 90 * time.Second},
	}
	for _, tc := range tests {
		got, err := ParseDuration(tc.input)
		if err != nil {
			t.Fatalf("%s: %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestParseRangeWithCompoundDuration(t *testing.T) {
	expr, err := Parse("rate(http_requests_total[1h30m])")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fc, ok := expr.(*FunctionCall)
	if !ok {
		t.Fatalf("expected FunctionCall, got %T", expr)
	}
	rs, ok := fc.Args[0].(*RangeSelector)
	if !ok {
		t.Fatalf("expected RangeSelector arg, got %T", fc.Args[0])
	}
	if rs.Duration != 90*time.Minute {
		t.Fatalf("duration: got %v, want 90m", rs.Duration)
	}
}

// --- Task 9: unary minus / plus ---

func TestUnaryExpressions(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.Ingest("cpu", nil, 1000, 5)
	db.Ingest("cpu", nil, 2000, 8)
	engine := NewEngine(db)

	check := func(q string, want float64) {
		t.Helper()
		// Evaluate at the instant of the last sample (ts=2000) so -cpu negates 8.
		res, err := engine.Execute(context.Background(), q, 2000, 2000, 15*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if len(res) == 0 || len(res[0].Points) == 0 {
			t.Fatalf("%s: no result", q)
		}
		got := res[0].Points[len(res[0].Points)-1].Value
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("%s: got %v, want %v", q, got, want)
		}
	}

	check("-5", -5)
	check("+5", 5)
	check("1 - -2", 3) // 1 - (-2)
	check("-cpu", -8)  // negate the last cpu sample (8)
}

// --- Task 10: bare label-only selectors ---

func TestBareSelectorParse(t *testing.T) {
	expr, err := Parse(`{job="api"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	vs, ok := expr.(*VectorSelector)
	if !ok {
		t.Fatalf("expected VectorSelector, got %T", expr)
	}
	if vs.Name != "" {
		t.Fatalf("name should be empty for a bare selector, got %q", vs.Name)
	}
	if len(vs.Matchers) != 1 || vs.Matchers[0].Name != "job" || vs.Matchers[0].Value != "api" {
		t.Fatalf("matchers: %+v", vs.Matchers)
	}
}

func TestBareSelectorEval(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.Ingest("up", map[string]string{"job": "api"}, 1000, 1)
	db.Ingest("up", map[string]string{"job": "db"}, 1000, 1)
	engine := NewEngine(db)

	res, err := engine.Execute(context.Background(), `{job="api"}`, 1000, 1000, 15*time.Second)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 series matching job=api, got %d", len(res))
	}
	if res[0].Labels["job"] != "api" {
		t.Fatalf("matched wrong series: %+v", res[0].Labels)
	}
}
