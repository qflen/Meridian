package storage

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/meridiandb/meridian/internal/compress"
)

// TestRollupPointsIncreaseWithReset checks the per-window counter increase, including a
// reset inside the window: a decrease means the counter restarted, so the post-reset
// value is its increase since (the same handling rate() applies to raw samples).
func TestRollupPointsIncreaseWithReset(t *testing.T) {
	// One 1m window: 10 → 30 (+20), then a reset to 5 (counts as +5). First sample is the
	// baseline. Total increase = 25.
	pts := []Point{
		{Timestamp: 10000, Value: 10},
		{Timestamp: 20000, Value: 30},
		{Timestamp: 30000, Value: 5},
	}
	got := RollupPoints(pts, 60000)
	if len(got) != 1 {
		t.Fatalf("expected 1 window, got %d", len(got))
	}
	if math.Abs(got[0].Increase-25) > 1e-9 {
		t.Fatalf("increase with reset: got %v, want 25", got[0].Increase)
	}
	// Sanity: the other aggregates are unaffected.
	if got[0].Min != 5 || got[0].Max != 30 || got[0].Count != 3 {
		t.Fatalf("aggregates: %+v", got[0])
	}
}

// TestRollupIncreaseTelescopes pins the additive, left-boundary-inclusive attribution:
// each consecutive delta is charged to the later sample's window, so the increase summed
// across contiguous windows equals the counter's growth across them (last − first).
func TestRollupIncreaseTelescopes(t *testing.T) {
	// Two 1m windows. A:[0,60s) has 10,20; B:[60s,120s) has 35,40. The 20→35 delta
	// crosses the boundary and is charged to B.
	pts := []Point{
		{Timestamp: 10000, Value: 10},
		{Timestamp: 50000, Value: 20},
		{Timestamp: 70000, Value: 35},
		{Timestamp: 110000, Value: 40},
	}
	got := RollupPoints(pts, 60000)
	if len(got) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(got))
	}
	if math.Abs(got[0].Increase-10) > 1e-9 { // 20-10, no predecessor before 10
		t.Fatalf("window A increase: got %v, want 10", got[0].Increase)
	}
	if math.Abs(got[1].Increase-20) > 1e-9 { // (35-20)+(40-35) = 15+5
		t.Fatalf("window B increase: got %v, want 20", got[1].Increase)
	}
	// Telescoping: ΣIncrease over all windows = last − first = 40 − 10 = 30.
	if sum := got[0].Increase + got[1].Increase; math.Abs(sum-30) > 1e-9 {
		t.Fatalf("ΣIncrease: got %v, want 30 (40-10)", sum)
	}
}

// TestChainIncreaseEqualsDirect proves the cascade is additive for increase too: chaining
// a fine tier into a coarse one (ChainRollups sums sub-increases) equals rolling raw
// straight to the coarse interval — including across a counter reset.
func TestChainIncreaseEqualsDirect(t *testing.T) {
	var pts []Point
	v := 0.0
	for i := 0; i < 200; i++ {
		ts := int64(i) * 100 // 100ms spacing, spans [0,20s)
		v += 3
		if i == 90 {
			v = 1 // a reset partway through
		}
		pts = append(pts, Point{Timestamp: ts, Value: v})
	}
	const fine, coarse = int64(1000), int64(10000) // 1s → 10s

	direct := RollupPoints(pts, coarse)
	chained := ChainRollups(RollupPoints(pts, fine), coarse)
	if len(direct) != len(chained) {
		t.Fatalf("window count: direct %d, chained %d", len(direct), len(chained))
	}
	for i := range direct {
		if direct[i].Timestamp != chained[i].Timestamp {
			t.Fatalf("window %d centre: %d vs %d", i, direct[i].Timestamp, chained[i].Timestamp)
		}
		if math.Abs(direct[i].Increase-chained[i].Increase) > 1e-6 {
			t.Fatalf("window %d increase: direct %v != chained %v", i, direct[i].Increase, chained[i].Increase)
		}
	}
}

// TestRollupBlockIncreaseColumn round-trips the increase column through a written v2 block,
// via both the columnar Query path and SeriesInRange.
func TestRollupBlockIncreaseColumn(t *testing.T) {
	dir := t.TempDir()
	series := []RollupSeriesData{{
		Name:   "http_requests_total",
		Labels: map[string]string{"job": "api"},
		Windows: []RollupSample{
			{Timestamp: 30000, Min: 1, Max: 5, Sum: 12, Count: 4, Avg: 3, Increase: 7},
			{Timestamp: 90000, Min: 2, Max: 8, Sum: 20, Count: 5, Avg: 4, Increase: 11},
		},
	}}
	block, err := WriteRollupBlock(dir, 60000, 120000, 0, series)
	if err != nil {
		t.Fatal(err)
	}
	if block.Meta().Version != rollupFormatVersion {
		t.Fatalf("written block version: %d, want %d", block.Meta().Version, rollupFormatVersion)
	}
	check := func(b *RollupBlock) {
		if !b.hasIncrease() {
			t.Fatal("v2 block should report hasIncrease")
		}
		res := b.Query([]LabelMatcher{{Name: "job", Value: "api", Type: MatchEqual}}, 0, 200000, rollupColIncrease)
		if len(res) != 1 || len(res[0].Points) != 2 {
			t.Fatalf("increase query: %+v", res)
		}
		if res[0].Points[0].Value != 7 || res[0].Points[1].Value != 11 {
			t.Fatalf("increase points: %+v", res[0].Points)
		}
		all := b.SeriesInRange(0, 200000)
		if len(all) != 1 || len(all[0].Windows) != 2 {
			t.Fatalf("series in range: %+v", all)
		}
		if all[0].Windows[0].Increase != 7 || all[0].Windows[1].Increase != 11 {
			t.Fatalf("reconstructed increase: %+v", all[0].Windows)
		}
	}
	check(block)
	reopened, err := OpenRollupBlock(block.Dir())
	if err != nil {
		t.Fatal(err)
	}
	check(reopened) // proves the on-disk v2 format round-trips
}

// TestRollupBlockV1GracefulLoad writes a legacy five-column block (no increase column, no
// format_version) and proves it loads and serves its five aggregates, while the increase
// column reads as absent so rate-on-rollup can fall back to raw.
func TestRollupBlockV1GracefulLoad(t *testing.T) {
	dir := t.TempDir()
	series := []RollupSeriesData{{
		Name:   "cpu",
		Labels: map[string]string{"host": "web-01"},
		Windows: []RollupSample{
			{Timestamp: 30000, Min: 1, Max: 5, Sum: 12, Count: 4, Avg: 3},
			{Timestamp: 90000, Min: 2, Max: 8, Sum: 20, Count: 5, Avg: 4},
		},
	}}
	blockDir := writeLegacyV1RollupBlock(t, dir, 60000, 120000, series)

	b, err := OpenRollupBlock(blockDir)
	if err != nil {
		t.Fatalf("open legacy block: %v", err)
	}
	if b.Meta().Version >= rollupFormatV2 || b.hasIncrease() || b.numCols() != numRollupColsV1 {
		t.Fatalf("legacy block should be v1 with five columns: version=%d hasIncrease=%v numCols=%d", b.Meta().Version, b.hasIncrease(), b.numCols())
	}
	// The five original columns still serve correctly (max here).
	res := b.Query([]LabelMatcher{{Name: "host", Value: "web-01", Type: MatchEqual}}, 0, 200000, rollupColMax)
	if len(res) != 1 || res[0].Points[0].Value != 5 || res[0].Points[1].Value != 8 {
		t.Fatalf("legacy max column: %+v", res)
	}
	// The increase column is absent → Query returns nothing (the rate-on-rollup fallback).
	if got := b.Query(nil, 0, 200000, rollupColIncrease); got != nil {
		t.Fatalf("legacy increase column should read empty, got %+v", got)
	}
	// SeriesInRange reconstructs the five aggregates with Increase defaulted to 0.
	all := b.SeriesInRange(0, 200000)
	if len(all) != 1 || all[0].Windows[0].Min != 1 || all[0].Windows[0].Increase != 0 {
		t.Fatalf("legacy SeriesInRange: %+v", all)
	}
}

// TestRollupIncreaseResolutions covers the tier-level gate: a tier qualifies only when
// every block carries the increase column, so a tier with a legacy block is excluded.
func TestRollupIncreaseResolutions(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, TSDBOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	series := []RollupSeriesData{{
		Name:    "cpu",
		Labels:  map[string]string{"host": "a"},
		Windows: []RollupSample{{Timestamp: 30000, Min: 1, Max: 5, Sum: 12, Count: 4, Avg: 3, Increase: 9}},
	}}
	// 1m tier: a v2 block (has increase).
	if _, err := db.PersistRollup(60000, 60000, 0, series); err != nil {
		t.Fatal(err)
	}
	// 1h tier: a legacy v1 block (no increase), written straight to disk then reloaded.
	resDir := db.rollupDirFor(3600000)
	if err := os.MkdirAll(resDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLegacyV1RollupBlock(t, resDir, 3600000, 3600000, series)
	if err := db.loadRollupBlocks(); err != nil {
		t.Fatal(err)
	}

	inc := db.RollupIncreaseResolutions()
	if len(inc) != 1 || inc[0] != 60000 {
		t.Fatalf("increase-capable resolutions: got %v, want [60000] (the 1h tier has a legacy block)", inc)
	}
	// Both tiers still report as having rollup data for the *_over_time path.
	if all := db.RollupResolutions(); len(all) != 2 {
		t.Fatalf("rollup resolutions: got %v, want both tiers", all)
	}
}

// writeLegacyV1RollupBlock writes a pre-v2 rollup block: five aggregate columns, a
// five-ref index, and a meta.json with no format_version field — exactly what an older
// build produced. It exists only to exercise backward-compatible loading.
func writeLegacyV1RollupBlock(t *testing.T, resDir string, resolution, coveredThrough int64, series []RollupSeriesData) string {
	t.Helper()
	sort.Slice(series, func(i, j int) bool {
		return seriesKey(series[i].Name, series[i].Labels) < seriesKey(series[j].Name, series[j].Labels)
	})
	id := generateULID()
	blockDir := filepath.Join(resDir, id)
	chunksDir := filepath.Join(blockDir, "chunks")
	if err := os.MkdirAll(chunksDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var chunkData, indexData []byte
	minTime, maxTime := int64(math.MaxInt64), int64(math.MinInt64)
	for sid, s := range series {
		w := s.Windows
		bs := rollupBlockSeries{id: uint64(sid + 1), name: s.Name, labels: s.Labels, minTime: w[0].Timestamp, maxTime: w[len(w)-1].Timestamp, windows: len(w)}
		for col := 0; col < numRollupColsV1; col++ { // five columns only
			enc := compress.NewEncoder()
			for _, win := range w {
				enc.Write(win.Timestamp, rollupColumnValue(win, col))
			}
			compressed := enc.Bytes()
			bs.cols[col] = chunkRef{offset: uint64(len(chunkData)), length: uint32(len(compressed))}
			chunkData = append(chunkData, compressed...)
		}
		indexData = append(indexData, encodeV1IndexEntry(bs)...)
		if bs.minTime < minTime {
			minTime = bs.minTime
		}
		if bs.maxTime > maxTime {
			maxTime = bs.maxTime
		}
	}
	if err := os.WriteFile(filepath.Join(chunksDir, "000001"), chunkData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blockDir, "index"), indexData, 0o644); err != nil {
		t.Fatal(err)
	}
	// meta.json deliberately omits format_version, as a v1 block on disk would.
	meta := map[string]interface{}{
		"ulid": id, "min_time": minTime, "max_time": maxTime,
		"resolution_ms": resolution, "covered_through": coveredThrough,
		"stats": map[string]interface{}{"num_series": len(series)}, "source": map[string]interface{}{},
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blockDir, "meta.json"), metaJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	return blockDir
}

// encodeV1IndexEntry mirrors the pre-v2 index entry layout: five column refs per series.
func encodeV1IndexEntry(s rollupBlockSeries) []byte {
	keys := make([]string, 0, len(s.labels))
	for k := range s.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf []byte
	put16 := func(v int) { var b [2]byte; binary.BigEndian.PutUint16(b[:], uint16(v)); buf = append(buf, b[:]...) }
	put32 := func(v uint32) { var b [4]byte; binary.BigEndian.PutUint32(b[:], v); buf = append(buf, b[:]...) }
	put64 := func(v uint64) { var b [8]byte; binary.BigEndian.PutUint64(b[:], v); buf = append(buf, b[:]...) }

	put64(s.id)
	put16(len(s.name))
	buf = append(buf, s.name...)
	put16(len(s.labels))
	for _, k := range keys {
		put16(len(k))
		buf = append(buf, k...)
		put16(len(s.labels[k]))
		buf = append(buf, s.labels[k]...)
	}
	for col := 0; col < numRollupColsV1; col++ {
		put64(s.cols[col].offset)
		put32(s.cols[col].length)
	}
	put64(uint64(s.minTime))
	put64(uint64(s.maxTime))
	put32(uint32(s.windows))
	return buf
}
