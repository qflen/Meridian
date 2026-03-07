package storage

import (
	"context"
	"testing"
)

// dropOne builds the half-open hash range (h-1, h] that selects exactly the named series,
// so a test can drop one series while leaving every other untouched.
func dropOne(name string, labels map[string]string) []HashRange {
	h := testHash(seriesKey(name, labels))
	return []HashRange{{Lo: h - 1, Hi: h}}
}

func pointsFor(t *testing.T, db *TSDB, name string) []Point {
	t.Helper()
	ss, err := db.Query(context.Background(),
		[]LabelMatcher{{Name: "__name__", Value: name, Type: MatchEqual}}, 0, 1<<62)
	if err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
	for _, s := range ss {
		if s.Name == name {
			return s.Points
		}
	}
	return nil
}

// TestDropSeriesInRanges_RewritesBlockKeepingOwned proves the core GC: dropping one series'
// hash range rewrites the block holding it so the dropped series is gone while every other
// series in the same block survives intact. (ADR-031)
func TestDropSeriesInRanges_RewritesBlockKeepingOwned(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer db.Close()

	keep := map[string]string{"h": "keep"}
	drop := map[string]string{"h": "drop"}
	backfillAll(t, db, []IngestSample{
		{Name: "cpu", Labels: keep, Timestamp: 100, Value: 1},
		{Name: "cpu", Labels: keep, Timestamp: 200, Value: 2},
		{Name: "mem", Labels: drop, Timestamp: 100, Value: 9},
		{Name: "mem", Labels: drop, Timestamp: 200, Value: 8},
	})

	res, err := db.DropSeriesInRanges(dropOne("mem", drop), testHash)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	// The head was flushed into one block that mixed both series, so it must be rewritten
	// (not deleted whole) to keep the owned "cpu" series.
	if res.BlocksRewritten != 1 || res.BlocksDeleted != 0 {
		t.Fatalf("expected one block rewritten, got %+v", res)
	}
	if res.SeriesDropped != 1 || res.SamplesDropped != 2 {
		t.Errorf("expected 1 series / 2 samples dropped, got %+v", res)
	}

	if got := pointsFor(t, db, "mem"); got != nil {
		t.Errorf("dropped series mem should be gone, still has %d points", len(got))
	}
	if got := pointsFor(t, db, "cpu"); len(got) != 2 {
		t.Errorf("kept series cpu should be intact, has %d points (want 2)", len(got))
	}
}

// TestDropSeriesInRanges_DeletesFullyUnownedBlock proves a block holding only un-owned series
// is deleted whole rather than rewritten.
func TestDropSeriesInRanges_DeletesFullyUnownedBlock(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer db.Close()

	drop := map[string]string{"h": "drop"}
	backfillAll(t, db, []IngestSample{
		{Name: "mem", Labels: drop, Timestamp: 100, Value: 9},
		{Name: "mem", Labels: drop, Timestamp: 200, Value: 8},
	})

	res, err := db.DropSeriesInRanges(dropOne("mem", drop), testHash)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if res.BlocksDeleted != 1 || res.BlocksRewritten != 0 {
		t.Fatalf("a fully un-owned block must be deleted whole, got %+v", res)
	}
	if got := pointsFor(t, db, "mem"); got != nil {
		t.Errorf("dropped series should be gone, has %d points", len(got))
	}
	if n := len(db.Blocks()); n != 0 {
		t.Errorf("expected no blocks left, got %d", n)
	}
}

// TestDropSeriesInRanges_EmptyAndIdempotent proves an empty range set is a no-op (no
// foot-gun that drops everything) and that a second drop over the same range removes nothing
// more.
func TestDropSeriesInRanges_EmptyAndIdempotent(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer db.Close()

	keep := map[string]string{"h": "keep"}
	drop := map[string]string{"h": "drop"}
	backfillAll(t, db, []IngestSample{
		{Name: "cpu", Labels: keep, Timestamp: 100, Value: 1},
		{Name: "mem", Labels: drop, Timestamp: 100, Value: 9},
	})

	if res, _ := db.DropSeriesInRanges(nil, testHash); res.SeriesDropped != 0 {
		t.Fatalf("empty range set must drop nothing, got %+v", res)
	}
	if len(pointsFor(t, db, "cpu")) == 0 || len(pointsFor(t, db, "mem")) == 0 {
		t.Fatal("empty drop must leave all data in place")
	}

	if res, _ := db.DropSeriesInRanges(dropOne("mem", drop), testHash); res.SeriesDropped != 1 {
		t.Fatalf("first drop should remove mem, got %+v", res)
	}
	if res, _ := db.DropSeriesInRanges(dropOne("mem", drop), testHash); res.SeriesDropped != 0 {
		t.Fatalf("second drop over the same range must be a no-op, got %+v", res)
	}
	if len(pointsFor(t, db, "cpu")) != 1 {
		t.Error("cpu must survive both drops")
	}
}

// TestDropSeriesInRanges_DropsRollupTier proves the GC reaches rollup blocks too: a dropped
// range removes its rollup windows while leaving the owned series' rollups in place.
func TestDropSeriesInRanges_DropsRollupTier(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer db.Close()

	keep := map[string]string{"h": "keep"}
	drop := map[string]string{"h": "drop"}
	res := int64(60000)
	if _, err := db.PersistRollup(res, 600000, 0, []RollupSeriesData{
		{Name: "cpu", Labels: keep, Windows: []RollupSample{{Timestamp: 0, Avg: 1, Min: 1, Max: 1, Sum: 1, Count: 1}}},
		{Name: "mem", Labels: drop, Windows: []RollupSample{{Timestamp: 0, Avg: 9, Min: 9, Max: 9, Sum: 9, Count: 1}}},
	}); err != nil {
		t.Fatalf("persist rollup: %v", err)
	}

	dr, err := db.DropSeriesInRanges(dropOne("mem", drop), testHash)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if dr.RollupWindows != 1 || dr.SeriesDropped != 1 {
		t.Fatalf("expected one rollup series/window dropped, got %+v", dr)
	}

	// The kept series' rollup survives; the dropped one's is gone.
	got, err := db.QueryResolution(context.Background(),
		[]LabelMatcher{{Name: "__name__", Value: "cpu", Type: MatchEqual}}, 0, 1<<62, res, AggAvg)
	if err != nil {
		t.Fatalf("query kept rollup: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("kept rollup series should remain, got %d series", len(got))
	}
	goneSeries, err := db.QueryResolution(context.Background(),
		[]LabelMatcher{{Name: "__name__", Value: "mem", Type: MatchEqual}}, 0, 1<<62, res, AggAvg)
	if err != nil {
		t.Fatalf("query dropped rollup: %v", err)
	}
	if len(goneSeries) != 0 {
		t.Errorf("dropped rollup series should be gone, got %d series", len(goneSeries))
	}
}
