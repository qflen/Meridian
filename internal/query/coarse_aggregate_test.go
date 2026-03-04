package query

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/retention"
	"github.com/meridiandb/meridian/internal/storage"
)

// rawOnlyDS wraps a TSDB but exposes only DataSource (not ResolutionDataSource), so the
// engine is forced to read raw. It lets a test evaluate the identical query against raw
// data and against the coarse rollup tiers and compare the two.
type rawOnlyDS struct{ db *storage.TSDB }

func (r rawOnlyDS) Query(ctx context.Context, m []storage.LabelMatcher, start, end int64) (storage.SeriesSet, error) {
	return r.db.Query(ctx, m, start, end)
}

// buildOffsetDB ingests `hours` of data at a 10s cadence offset by +5s, so no raw sample
// ever lands on a 1m or 1h window boundary. With a query whose step/range are aligned to
// the resolution, the half-open window (t-d, t] then selects exactly the same windows'
// worth of data whether read as raw points or as coarse rollup centres — so a correct
// coarse aggregate equals the raw aggregate exactly, not merely within tolerance. Every
// minute holds exactly six samples (equal counts), which also makes avg_over_time exact.
func buildOffsetDB(t *testing.T, hours int) *storage.TSDB {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(dir, storage.TSDBOptions{BlockDuration: time.Hour, FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	end := int64(hours) * ms(time.Hour)
	for _, host := range []string{"web-01", "web-02"} {
		off := 0.0
		if host == "web-02" {
			off = 50.0
		}
		for ts := int64(5000); ts < end; ts += 10000 {
			// Sawtooth so min/max/sum/avg are all non-trivial and host-distinct.
			v := float64((ts/1000)%100) + off
			if err := db.Ingest("cpu", map[string]string{"host": host}, ts, v); err != nil {
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

// assertCoarseEqualsRaw runs one query against the coarse rollup tiers and against raw,
// asserting the chosen resolution and that every coarse point equals its raw counterpart.
func assertCoarseEqualsRaw(t *testing.T, db *storage.TSDB, query string, start, end int64, step time.Duration, wantRes int64) {
	t.Helper()
	ctx := context.Background()

	coarse, meta, err := NewEngine(db).ExecuteWithMeta(ctx, query, start, end, step)
	if err != nil {
		t.Fatalf("%s coarse: %v", query, err)
	}
	if meta.ResolutionMs != wantRes {
		t.Fatalf("%s: resolution_ms=%d, want %d (the coarse tier was not selected)", query, meta.ResolutionMs, wantRes)
	}
	raw, rawMeta, err := NewEngine(rawOnlyDS{db}).ExecuteWithMeta(ctx, query, start, end, step)
	if err != nil {
		t.Fatalf("%s raw: %v", query, err)
	}
	if rawMeta.ResolutionMs != 0 {
		t.Fatalf("%s: raw control read resolution_ms=%d, want 0", query, rawMeta.ResolutionMs)
	}

	rawByKey := map[string][]storage.Point{}
	for _, s := range raw {
		rawByKey[seriesSignature(s.Labels)] = s.Points
	}
	if len(coarse) != len(raw) {
		t.Fatalf("%s: %d coarse series vs %d raw series", query, len(coarse), len(raw))
	}
	var compared int
	for _, cs := range coarse {
		rp, ok := rawByKey[seriesSignature(cs.Labels)]
		if !ok {
			t.Fatalf("%s: coarse series %v has no raw counterpart", query, cs.Labels)
		}
		if len(cs.Points) != len(rp) {
			t.Fatalf("%s [%v]: %d coarse points vs %d raw points", query, cs.Labels, len(cs.Points), len(rp))
		}
		for i := range cs.Points {
			if cs.Points[i].Timestamp != rp[i].Timestamp {
				t.Fatalf("%s [%v] point %d: coarse ts %d != raw ts %d", query, cs.Labels, i, cs.Points[i].Timestamp, rp[i].Timestamp)
			}
			if math.Abs(cs.Points[i].Value-rp[i].Value) > 1e-9 {
				t.Fatalf("%s [%v] point %d (ts=%d): coarse %v != raw %v", query, cs.Labels, i, cs.Points[i].Timestamp, cs.Points[i].Value, rp[i].Value)
			}
			compared++
		}
	}
	if compared == 0 {
		t.Fatalf("%s: compared no points", query)
	}
	t.Logf("%s: resolution=%dms, %d points equal raw across %d series", query, meta.ResolutionMs, compared, len(coarse))
}

// TestCoarseOverTimeEqualsRaw is the TIER-2 headline: each *_over_time function served
// from a coarse rollup tier reads the matching aggregate column and returns exactly what
// the same function computes over raw data.
func TestCoarseOverTimeEqualsRaw(t *testing.T) {
	db := buildOffsetDB(t, 6)
	defer db.Close()

	oneMin := ms(time.Minute)
	// 1m tier: a 10-minute range at a 10-minute step over [1h, 5h]; step==range==10m are
	// 1m-aligned, span/1m ≫ 4, and 1h would exceed the step → the 1m tier is chosen.
	start, end, step := ms(time.Hour), 5*ms(time.Hour), 10*time.Minute
	for _, fn := range []string{"min_over_time", "max_over_time", "sum_over_time", "count_over_time", "avg_over_time"} {
		assertCoarseEqualsRaw(t, db, fn+"(cpu[10m])", start, end, step, oneMin)
	}
}

// TestCoarseOverTime1hTier proves the 1h column (not just 1m) is read function-aware:
// a 1h range at a 1h step over a 4h span selects the 1h tier.
func TestCoarseOverTime1hTier(t *testing.T) {
	db := buildOffsetDB(t, 6)
	defer db.Close()

	oneHour := ms(time.Hour)
	start, end, step := ms(time.Hour), 5*ms(time.Hour), time.Hour
	for _, fn := range []string{"min_over_time", "max_over_time", "sum_over_time", "count_over_time"} {
		assertCoarseEqualsRaw(t, db, fn+"(cpu[1h])", start, end, step, oneHour)
	}
}
