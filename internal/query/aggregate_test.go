package query

import (
	"context"
	"testing"
	"time"
)

// --- Task 6: topk / bottomk ---

func TestParseTopK(t *testing.T) {
	expr, err := Parse("topk(2, metric)")
	if err != nil {
		t.Fatal(err)
	}
	ae, ok := expr.(*AggregateExpr)
	if !ok {
		t.Fatalf("expected AggregateExpr, got %T", expr)
	}
	if ae.Op != "topk" {
		t.Fatalf("op: %s", ae.Op)
	}
	nl, ok := ae.Param.(*NumberLiteral)
	if !ok || nl.Value != 2 {
		t.Fatalf("expected Param NumberLiteral(2), got %+v", ae.Param)
	}
	if _, ok := ae.Expr.(*VectorSelector); !ok {
		t.Fatalf("expected vector arg, got %T", ae.Expr)
	}
}

func TestTopKBottomK(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.Ingest("metric", map[string]string{"host": "a"}, 1000, 10)
	db.Ingest("metric", map[string]string{"host": "b"}, 1000, 30)
	db.Ingest("metric", map[string]string{"host": "c"}, 1000, 20)
	engine := NewEngine(db)

	top, err := engine.Execute(context.Background(), "topk(2, metric)", 1000, 1000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 {
		t.Fatalf("topk(2): expected 2 series, got %d", len(top))
	}
	got := map[string]float64{}
	for _, s := range top {
		got[s.Labels["host"]] = s.Points[0].Value
	}
	if got["b"] != 30 || got["c"] != 20 {
		t.Fatalf("topk(2) kept wrong series: %+v", got)
	}
	if _, ok := got["a"]; ok {
		t.Fatalf("topk(2) should have dropped host=a (value 10)")
	}

	bottom, err := engine.Execute(context.Background(), "bottomk(1, metric)", 1000, 1000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(bottom) != 1 || bottom[0].Labels["host"] != "a" || bottom[0].Points[0].Value != 10 {
		t.Fatalf("bottomk(1): expected host=a value 10, got %+v", bottom)
	}
}

// --- Task 7: without grouping ---

func TestAggregationWithout(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.Ingest("m", map[string]string{"host": "h1", "instance": "i1"}, 1000, 10)
	db.Ingest("m", map[string]string{"host": "h1", "instance": "i2"}, 1000, 20)
	db.Ingest("m", map[string]string{"host": "h2", "instance": "i1"}, 1000, 5)
	engine := NewEngine(db)

	res, err := engine.Execute(context.Background(), "sum(m) without (instance)", 1000, 1000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(res))
	}
	for _, s := range res {
		if _, ok := s.Labels["instance"]; ok {
			t.Fatalf("without(instance) must drop the instance label: %+v", s.Labels)
		}
		if _, ok := s.Labels["__name__"]; ok {
			t.Fatalf("aggregation must drop __name__: %+v", s.Labels)
		}
		switch s.Labels["host"] {
		case "h1":
			if s.Points[0].Value != 30 {
				t.Fatalf("h1: got %v, want 30", s.Points[0].Value)
			}
		case "h2":
			if s.Points[0].Value != 5 {
				t.Fatalf("h2: got %v, want 5", s.Points[0].Value)
			}
		default:
			t.Fatalf("unexpected group: %+v", s.Labels)
		}
	}
}

// --- Task 11: well-formed groups when a series lacks the grouping label ---

func TestAggregationByMissingLabel(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.Ingest("m", map[string]string{"host": "a", "dc": "x"}, 1000, 10)
	db.Ingest("m", map[string]string{"host": "a", "dc": "y"}, 1000, 20)
	db.Ingest("m", map[string]string{"dc": "z"}, 1000, 5) // no host label
	engine := NewEngine(db)

	res, err := engine.Execute(context.Background(), "sum(m) by (host)", 1000, 1000, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(res))
	}

	var foundAbsentGroup bool
	for _, s := range res {
		host, ok := s.Labels["host"]
		if !ok {
			t.Fatalf("every group must carry the grouping label; got unlabeled group %+v", s.Labels)
		}
		if host == "" {
			foundAbsentGroup = true
			if s.Points[0].Value != 5 {
				t.Fatalf("absent-host group: got %v, want 5", s.Points[0].Value)
			}
		} else if host == "a" && s.Points[0].Value != 30 {
			t.Fatalf("host=a: got %v, want 30", s.Points[0].Value)
		}
	}
	if !foundAbsentGroup {
		t.Fatal("expected a well-formed group with host=\"\" for the series lacking host")
	}
}
