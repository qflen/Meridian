package storage

import (
	"testing"
)

func sampleSeries() []RollupSeriesData {
	return []RollupSeriesData{
		{
			Name:   "cpu",
			Labels: map[string]string{"host": "web-01"},
			Windows: []RollupSample{
				{Timestamp: 30000, Min: 1, Max: 5, Sum: 12, Count: 4, Avg: 3.0},
				{Timestamp: 90000, Min: 2, Max: 8, Sum: 20, Count: 5, Avg: 4.0},
			},
		},
		{
			Name:   "cpu",
			Labels: map[string]string{"host": "web-02"},
			Windows: []RollupSample{
				{Timestamp: 30000, Min: 10, Max: 10, Sum: 10, Count: 1, Avg: 10.0},
			},
		},
	}
}

func TestWriteAndReadRollupBlock(t *testing.T) {
	dir := t.TempDir()
	block, err := WriteRollupBlock(dir, 60000, 120000, 0, sampleSeries())
	if err != nil {
		t.Fatal(err)
	}

	meta := block.Meta()
	if meta.Resolution != 60000 {
		t.Fatalf("resolution: %d", meta.Resolution)
	}
	if meta.CoveredThrough != 120000 {
		t.Fatalf("covered through: %d", meta.CoveredThrough)
	}
	if meta.Stats.NumSeries != 2 {
		t.Fatalf("series: %d", meta.Stats.NumSeries)
	}
	if meta.Stats.NumWindows != 3 {
		t.Fatalf("windows: %d", meta.Stats.NumWindows)
	}
	if meta.Stats.RawSamples != 4+5+1 {
		t.Fatalf("raw samples: %d", meta.Stats.RawSamples)
	}
	if meta.MinTime != 30000 || meta.MaxTime != 90000 {
		t.Fatalf("bounds: %d..%d", meta.MinTime, meta.MaxTime)
	}

	assertRollupReadback(t, block)

	// Reopen from disk and re-check, proving the on-disk format round-trips.
	reopened, err := OpenRollupBlock(block.Dir())
	if err != nil {
		t.Fatal(err)
	}
	assertRollupReadback(t, reopened)
}

func assertRollupReadback(t *testing.T, block *RollupBlock) {
	t.Helper()

	// avg column for web-01 → two points (3.0, 4.0)
	res := block.Query([]LabelMatcher{
		{Name: "__name__", Value: "cpu", Type: MatchEqual},
		{Name: "host", Value: "web-01", Type: MatchEqual},
	}, 0, 200000, rollupColAvg)
	if len(res) != 1 {
		t.Fatalf("avg query: %d results", len(res))
	}
	if len(res[0].Points) != 2 || res[0].Points[0].Value != 3.0 || res[0].Points[1].Value != 4.0 {
		t.Fatalf("avg points: %+v", res[0].Points)
	}

	// Time-range pushdown: only the second window's centre is in [60000, 200000].
	res = block.Query([]LabelMatcher{
		{Name: "host", Value: "web-01", Type: MatchEqual},
	}, 60000, 200000, rollupColAvg)
	if len(res) != 1 || len(res[0].Points) != 1 || res[0].Points[0].Timestamp != 90000 {
		t.Fatalf("range-pruned avg: %+v", res)
	}

	// An out-of-range default falls back to avg; verify max column explicitly instead.
	res = block.Query([]LabelMatcher{
		{Name: "host", Value: "web-01", Type: MatchEqual},
	}, 0, 200000, rollupColMax)
	if len(res) != 1 || res[0].Points[0].Value != 5 || res[0].Points[1].Value != 8 {
		t.Fatalf("max points: %+v", res)
	}

	// Full reconstruction of all five aggregates via SeriesInRange.
	all := block.SeriesInRange(0, 200000)
	if len(all) != 2 {
		t.Fatalf("series in range: %d", len(all))
	}
	var web01 *RollupSeriesData
	for i := range all {
		if all[i].Labels["host"] == "web-01" {
			web01 = &all[i]
		}
	}
	if web01 == nil {
		t.Fatal("web-01 missing from SeriesInRange")
	}
	w0 := web01.Windows[0]
	if w0.Min != 1 || w0.Max != 5 || w0.Sum != 12 || w0.Count != 4 || w0.Avg != 3.0 {
		t.Fatalf("reconstructed window 0: %+v", w0)
	}
}

func TestRollupBlockReloadAfterRestart(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(dir, TSDBOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.PersistRollup(60000, 120000, 0, sampleSeries()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PersistRollup(3600000, 3600000, 60000, sampleSeries()); err != nil {
		t.Fatal(err)
	}
	if got := db.RollupCoveredThrough(60000); got != 120000 {
		t.Fatalf("covered through before restart: %d", got)
	}
	db.Close()

	// Reopen: rollup blocks must reload, tagged by resolution.
	db2, err := Open(dir, TSDBOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	if res := db2.RollupResolutions(); len(res) != 2 || res[0] != 60000 || res[1] != 3600000 {
		t.Fatalf("resolutions after restart: %v", res)
	}
	if got := db2.RollupCoveredThrough(60000); got != 120000 {
		t.Fatalf("covered through after restart: %d", got)
	}
	blocks := db2.RollupBlocks(60000)
	if len(blocks) != 1 {
		t.Fatalf("1m blocks after restart: %d", len(blocks))
	}
	res := blocks[0].Query([]LabelMatcher{
		{Name: "host", Value: "web-02", Type: MatchEqual},
	}, 0, 200000, rollupColAvg)
	if len(res) != 1 || res[0].Points[0].Value != 10.0 {
		t.Fatalf("reloaded rollup query: %+v", res)
	}

	stats := db2.RollupStats()
	if len(stats) != 2 {
		t.Fatalf("rollup stats resolutions: %d", len(stats))
	}
	if stats[0].Resolution != 60000 || stats[0].NumWindows != 3 || stats[0].ChunkBytes <= 0 {
		t.Fatalf("1m stats: %+v", stats[0])
	}
}
