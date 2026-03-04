package query

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/retention"
	"github.com/meridiandb/meridian/internal/storage"
)

// buildCounterDB ingests `hours` of a linear counter per host (value = elapsed_seconds ×
// ratePerSec[host]), sampled every 10s offset by +5s so no sample lands on a window
// boundary, then builds the 1m/1h rollup tiers (with the counter-increase column).
func buildCounterDB(t *testing.T, hours int, ratePerSec map[string]float64) *storage.TSDB {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(dir, storage.TSDBOptions{BlockDuration: time.Hour, FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	end := int64(hours) * ms(time.Hour)
	for host, rate := range ratePerSec {
		for ts := int64(5000); ts < end; ts += 10000 {
			v := float64(ts) / 1000.0 * rate
			if err := db.Ingest("requests_total", map[string]string{"host": host}, ts, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	rules := []retention.DownsampleRule{
		{SourceInterval: 10 * time.Second, TargetInterval: time.Minute, Retention: 7 * 24 * time.Hour},
		{SourceInterval: time.Minute, TargetInterval: time.Hour, Retention: 30 * 24 * time.Hour},
	}
	retention.NewDownsampler(db, rules, time.Hour).Downsample()
	return db
}

// TestRateOnRollupMatchesRaw is the TIER-3 headline: rate() served from a coarse tier's
// increase column matches rate() over raw within tolerance, and equals the counter's true
// per-second rate. It also pins that the increase column was the source (resolution > 0).
func TestRateOnRollupMatchesRaw(t *testing.T) {
	rates := map[string]float64{"a": 2.0, "b": 5.0}
	db := buildCounterDB(t, 6, rates)
	defer db.Close()
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		query   string
		step    time.Duration
		wantRes int64
	}{
		{"1m tier", "rate(requests_total[30m])", 30 * time.Minute, ms(time.Minute)},
		{"1h tier", "rate(requests_total[1h])", time.Hour, ms(time.Hour)},
	} {
		start, end := ms(time.Hour), 6*ms(time.Hour)
		coarse, meta, err := NewEngine(db).ExecuteWithMeta(ctx, tc.query, start, end, tc.step)
		if err != nil {
			t.Fatalf("%s coarse: %v", tc.name, err)
		}
		if meta.ResolutionMs != tc.wantRes {
			t.Fatalf("%s: resolution_ms=%d, want %d (rate not served from the increase column)", tc.name, meta.ResolutionMs, tc.wantRes)
		}
		raw, rawMeta, err := NewEngine(rawOnlyDS{db}).ExecuteWithMeta(ctx, tc.query, start, end, tc.step)
		if err != nil {
			t.Fatalf("%s raw: %v", tc.name, err)
		}
		if rawMeta.ResolutionMs != 0 {
			t.Fatalf("%s: raw control resolution_ms=%d, want 0", tc.name, rawMeta.ResolutionMs)
		}

		rawByKey := map[string][]storage.Point{}
		for _, s := range raw {
			rawByKey[seriesSignature(s.Labels)] = s.Points
		}
		var compared int
		for _, cs := range coarse {
			want := rates[cs.Labels["host"]]
			rp := rawByKey[seriesSignature(cs.Labels)]
			rawByTS := map[int64]float64{}
			for _, p := range rp {
				rawByTS[p.Timestamp] = p.Value
			}
			for _, p := range cs.Points {
				// Coarse rate equals the counter's true rate within ~1%.
				if rel := math.Abs(p.Value-want) / want; rel > 0.01 {
					t.Fatalf("%s [%v] ts=%d: coarse rate %v vs true %v (rel %.4f)", tc.name, cs.Labels, p.Timestamp, p.Value, want, rel)
				}
				// And tracks raw rate() at the same step within ~1%.
				if rv, ok := rawByTS[p.Timestamp]; ok {
					if rel := math.Abs(p.Value-rv) / rv; rel > 0.01 {
						t.Fatalf("%s [%v] ts=%d: coarse %v vs raw %v (rel %.4f)", tc.name, cs.Labels, p.Timestamp, p.Value, rv, rel)
					}
					compared++
				}
			}
		}
		if compared == 0 {
			t.Fatalf("%s: no coarse/raw points compared", tc.name)
		}
		t.Logf("%s: resolution=%dms, %d points within 1%% of raw and of the true rate", tc.name, meta.ResolutionMs, compared)
	}
}

// TestRateNarrowReadsRaw pins that a short-range rate query (a small step below the finest
// resolution) is served from raw, not a rollup tier — the increase column is for wide spans.
func TestRateNarrowReadsRaw(t *testing.T) {
	db := buildCounterDB(t, 2, map[string]float64{"a": 2.0})
	defer db.Close()

	start := ms(time.Hour)
	_, meta, err := NewEngine(db).ExecuteWithMeta(context.Background(), "rate(requests_total[1m])", start, start+ms(5*time.Minute), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ResolutionMs != 0 {
		t.Fatalf("narrow rate: resolution_ms=%d, want 0 (raw)", meta.ResolutionMs)
	}
	if meta.PointsRead == 0 {
		t.Fatal("narrow rate read no raw points")
	}
}

// TestRateRollupFallsBackToRawWithoutIncreaseColumn proves the graceful path: when the
// rollup tiers lack the increase column (legacy v1 blocks), rate is not served coarse —
// it forces raw — even though the *_over_time path still uses those tiers.
func TestRateRollupFallsBackToRawWithoutIncreaseColumn(t *testing.T) {
	// noIncreaseDS reports rollup resolutions but none increase-capable, mimicking a store
	// whose rollups predate the increase column.
	db := buildCounterDB(t, 6, map[string]float64{"a": 2.0})
	defer db.Close()

	ds := noIncreaseDS{db}
	start, end := ms(time.Hour), 6*ms(time.Hour)

	// max_over_time still goes coarse (its column exists)...
	_, m1, err := NewEngine(ds).ExecuteWithMeta(context.Background(), "max_over_time(requests_total[30m])", start, end, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if m1.ResolutionMs == 0 {
		t.Fatal("max_over_time should still be served coarse when only the increase column is missing")
	}
	// ...but rate forces raw, because no tier is increase-capable.
	_, m2, err := NewEngine(ds).ExecuteWithMeta(context.Background(), "rate(requests_total[30m])", start, end, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if m2.ResolutionMs != 0 {
		t.Fatalf("rate without an increase column: resolution_ms=%d, want 0 (raw fallback)", m2.ResolutionMs)
	}
}

// noIncreaseDS wraps a TSDB but reports no increase-capable resolutions, so rate cannot be
// served from a coarse tier while the *_over_time columns still can.
type noIncreaseDS struct{ db *storage.TSDB }

func (d noIncreaseDS) Query(ctx context.Context, m []storage.LabelMatcher, start, end int64) (storage.SeriesSet, error) {
	return d.db.Query(ctx, m, start, end)
}

func (d noIncreaseDS) QueryResolution(ctx context.Context, m []storage.LabelMatcher, start, end, resolution int64, agg storage.RollupAggregate) (storage.SeriesSet, error) {
	return d.db.QueryResolution(ctx, m, start, end, resolution, agg)
}

func (d noIncreaseDS) RollupResolutions() []int64         { return d.db.RollupResolutions() }
func (d noIncreaseDS) RollupIncreaseResolutions() []int64 { return nil }
