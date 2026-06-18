package retention

import (
	"math"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// openManualDB opens a TSDB whose background flush never fires on its own (block
// duration far exceeds the test data), so the test controls exactly when raw blocks
// are sealed.
func openManualDB(t *testing.T, dir string) *storage.TSDB {
	t.Helper()
	db, err := storage.Open(dir, storage.TSDBOptions{
		BlockDuration: time.Hour,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// cascadeRules is a miniature cascade (raw→1s→10s) so tests stay fast while exercising
// the exact same code path as the real 5s→1m→1h cascade.
func cascadeRules() []DownsampleRule {
	return []DownsampleRule{
		{SourceInterval: 100 * time.Millisecond, TargetInterval: time.Second, Retention: time.Hour},
		{SourceInterval: time.Second, TargetInterval: 10 * time.Second, Retention: 24 * time.Hour},
	}
}

// ingestSynthetic writes 300 points/series at 100ms spacing (30s of data), sealing a
// raw block at the halfway mark so windows straddle the block boundary. host "b" skips
// every 7th sample, making per-window counts uneven (so weighting matters). Returns the
// raw points actually stored, per host.
func ingestSynthetic(t *testing.T, db *storage.TSDB) map[string][]storage.Point {
	t.Helper()
	raw := map[string][]storage.Point{"a": nil, "b": nil}
	for i := 0; i < 300; i++ {
		ts := int64(i) * 100
		av := float64(i)
		if err := db.Ingest("cpu", map[string]string{"host": "a"}, ts, av); err != nil {
			t.Fatal(err)
		}
		raw["a"] = append(raw["a"], storage.Point{Timestamp: ts, Value: av})
		if i%7 != 0 {
			bv := float64(300 - i)
			if err := db.Ingest("cpu", map[string]string{"host": "b"}, ts, bv); err != nil {
				t.Fatal(err)
			}
			raw["b"] = append(raw["b"], storage.Point{Timestamp: ts, Value: bv})
		}
		if i == 149 {
			if err := db.Flush(); err != nil { // seal a raw block mid-stream
				t.Fatal(err)
			}
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	return raw
}

func findSeries(series []storage.RollupSeriesData, host string) *storage.RollupSeriesData {
	for i := range series {
		if series[i].Labels["host"] == host {
			return &series[i]
		}
	}
	return nil
}

func TestDownsampleRawToCoarse(t *testing.T) {
	dir := t.TempDir()
	db := openManualDB(t, dir)
	defer db.Close()

	raw := ingestSynthetic(t, db)

	ds := NewDownsampler(db, cascadeRules(), time.Hour)
	if n := ds.Downsample(); n == 0 {
		t.Fatal("expected rollup blocks to be generated")
	}

	// Raw frontier is 29900 → 1s tier closed through 29000, 10s tier through 20000.
	if got := db.RollupCoveredThrough(1000); got != 29000 {
		t.Fatalf("1s covered through: %d, want 29000", got)
	}
	if got := db.RollupCoveredThrough(10000); got != 20000 {
		t.Fatalf("10s covered through: %d, want 20000", got)
	}

	// 1s tier: host "a" has a sample every 100ms → 29 full 1s windows, each count 10.
	oneSec := db.RollupTierSeries(1000, math.MinInt64/4, math.MaxInt64/4)
	a1s := findSeries(oneSec, "a")
	if a1s == nil || len(a1s.Windows) != 29 {
		t.Fatalf("1s host a windows: %v", a1s)
	}
	for _, w := range a1s.Windows {
		if w.Count != 10 {
			t.Fatalf("1s window count %d, want 10", w.Count)
		}
	}

	// The cascade equivalence, end to end through blocks: the chained 10s tier must
	// equal a direct 10s rollup of the raw points in the covered span, for BOTH hosts
	// and ALL five aggregates (host "b" has uneven counts).
	tenSec := db.RollupTierSeries(10000, math.MinInt64/4, math.MaxInt64/4)
	for _, host := range []string{"a", "b"} {
		got := findSeries(tenSec, host)
		if got == nil {
			t.Fatalf("10s tier missing host %s", host)
		}
		var inRange []storage.Point
		for _, p := range raw[host] {
			if p.Timestamp < 20000 {
				inRange = append(inRange, p)
			}
		}
		want := storage.RollupPoints(inRange, 10000)
		if len(got.Windows) != len(want) {
			t.Fatalf("host %s: %d chained 10s windows, want %d", host, len(got.Windows), len(want))
		}
		for i := range want {
			assertSampleEqual(t, host, want[i], got.Windows[i])
		}
	}
}

func assertSampleEqual(t *testing.T, host string, want, got storage.RollupSample) {
	t.Helper()
	if want.Timestamp != got.Timestamp {
		t.Fatalf("host %s ts %d != %d", host, got.Timestamp, want.Timestamp)
	}
	if want.Min != got.Min || want.Max != got.Max || want.Count != got.Count {
		t.Fatalf("host %s min/max/count: got %v/%v/%d want %v/%v/%d", host, got.Min, got.Max, got.Count, want.Min, want.Max, want.Count)
	}
	if math.Abs(want.Sum-got.Sum) > 1e-6 || math.Abs(want.Avg-got.Avg) > 1e-9 {
		t.Fatalf("host %s sum/avg: got %v/%v want %v/%v", host, got.Sum, got.Avg, want.Sum, want.Avg)
	}
}

func TestDownsampleIdempotent(t *testing.T) {
	dir := t.TempDir()
	db := openManualDB(t, dir)
	defer db.Close()
	ingestSynthetic(t, db)

	ds := NewDownsampler(db, cascadeRules(), time.Hour)
	first := ds.Downsample()
	if first == 0 {
		t.Fatal("first pass generated nothing")
	}
	// No new raw data and no advance in the source frontier → a second pass writes
	// nothing and does not duplicate windows.
	if second := ds.Downsample(); second != 0 {
		t.Fatalf("second pass generated %d blocks, want 0", second)
	}
	before := db.RollupCoveredThrough(1000)
	if before != 29000 {
		t.Fatalf("covered through changed: %d", before)
	}
}

func TestDownsampleReloadAfterRestart(t *testing.T) {
	dir := t.TempDir()
	db := openManualDB(t, dir)
	ingestSynthetic(t, db)
	ds := NewDownsampler(db, cascadeRules(), time.Hour)
	ds.Downsample()
	want1s := db.RollupCoveredThrough(1000)
	want10s := db.RollupCoveredThrough(10000)
	db.Close()

	db2 := openManualDB(t, dir)
	defer db2.Close()
	if got := db2.RollupCoveredThrough(1000); got != want1s {
		t.Fatalf("1s covered through after restart: %d != %d", got, want1s)
	}
	if got := db2.RollupCoveredThrough(10000); got != want10s {
		t.Fatalf("10s covered through after restart: %d != %d", got, want10s)
	}
	if res := db2.RollupResolutions(); len(res) != 2 {
		t.Fatalf("resolutions after restart: %v", res)
	}

	// A fresh downsampler resumes from the on-disk watermark: with no new raw data it
	// writes nothing rather than re-rolling the same span.
	ds2 := NewDownsampler(db2, cascadeRules(), time.Hour)
	if n := ds2.Downsample(); n != 0 {
		t.Fatalf("post-restart pass generated %d blocks, want 0", n)
	}
}
