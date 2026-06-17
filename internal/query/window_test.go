package query

import (
	"context"
	"testing"
	"time"
)

// TestRangeWindowAppliedOnce guards against the range duration being subtracted
// twice (once in the planner, once in evalRange). A point that sits just outside
// the [dur] window must be excluded; with the double-subtraction bug the window
// was twice as wide and the point leaked in.
func TestRangeWindowAppliedOnce(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	const minute = int64(60_000)
	end := 10 * minute // 600000

	// Window for metric[5m] evaluated at `end` is [end-5m, end] = [300000, 600000].
	db.Ingest("metric", map[string]string{"host": "a"}, 4*minute, 1) // 240000: outside, must be dropped
	db.Ingest("metric", map[string]string{"host": "a"}, 5*minute, 2) // 300000: lower edge, included
	db.Ingest("metric", map[string]string{"host": "a"}, 7*minute, 3) // 420000: inside
	db.Ingest("metric", map[string]string{"host": "a"}, 10*minute, 4) // 600000: upper edge, included

	engine := NewEngine(db)
	// Evaluate at a single instant (start == end) so the window is unambiguous.
	results, err := engine.Execute(context.Background(), "metric[5m]", end, end, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 series, got %d", len(results))
	}
	pts := results[0].Points
	if len(pts) != 3 {
		t.Fatalf("expected 3 points in [end-5m, end], got %d: %+v", len(pts), pts)
	}
	for _, p := range pts {
		if p.Timestamp < 5*minute {
			t.Fatalf("point at %d is outside the 5m window and should have been excluded", p.Timestamp)
		}
	}
}
