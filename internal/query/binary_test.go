package query

import (
	"context"
	"math"
	"testing"
	"time"
)

// --- Task 4: vector-to-vector binary ops with label matching ---

func TestVectorVectorDivision(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	db.Ingest("requests", map[string]string{"host": "web1"}, 1000, 200)
	db.Ingest("errors", map[string]string{"host": "web1"}, 1000, 10)
	db.Ingest("requests", map[string]string{"host": "web2"}, 1000, 50)
	db.Ingest("errors", map[string]string{"host": "web2"}, 1000, 5)
	// errors with no matching requests series — must be dropped.
	db.Ingest("errors", map[string]string{"host": "web3"}, 1000, 1)

	engine := NewEngine(db)
	res, err := engine.Execute(context.Background(), "errors / requests", 1000, 1000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 matched series, got %d", len(res))
	}
	byHost := map[string]float64{}
	for _, s := range res {
		if _, ok := s.Labels["__name__"]; ok {
			t.Fatalf("vector/vector result must drop __name__, got %+v", s.Labels)
		}
		byHost[s.Labels["host"]] = s.Points[0].Value
	}
	if math.Abs(byHost["web1"]-0.05) > 1e-9 {
		t.Fatalf("web1: got %v, want 0.05", byHost["web1"])
	}
	if math.Abs(byHost["web2"]-0.1) > 1e-9 {
		t.Fatalf("web2: got %v, want 0.1", byHost["web2"])
	}
	if _, ok := byHost["web3"]; ok {
		t.Fatalf("web3 had no matching denominator and should have been dropped")
	}
}

// --- Task 5: IEEE-754 division and unknown-operator errors ---

func TestDivisionByZeroIEEE(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	engine := NewEngine(db)

	eval := func(q string) float64 {
		t.Helper()
		res, err := engine.Execute(context.Background(), q, 0, 1000, 15*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return res[0].Points[0].Value
	}

	if v := eval("1/0"); !math.IsInf(v, 1) {
		t.Fatalf("1/0: got %v, want +Inf", v)
	}
	if v := eval("-1/0"); !math.IsInf(v, -1) {
		t.Fatalf("-1/0: got %v, want -Inf", v)
	}
	if v := eval("0/0"); !math.IsNaN(v) {
		t.Fatalf("0/0: got %v, want NaN", v)
	}
}

func TestUnknownBinaryOperatorErrors(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	engine := NewEngine(db)

	ec := &evalContext{ctx: context.Background(), lookback: defaultLookbackDelta.Milliseconds()}
	_, err := engine.evalInstant(ec,
		&BinaryExpr{Op: "%", Left: &NumberLiteral{Value: 1}, Right: &NumberLiteral{Value: 1}}, 0)
	if err == nil {
		t.Fatal("expected error for unknown binary operator, got nil")
	}
}
