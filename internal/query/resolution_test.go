package query

import (
	"context"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/retention"
	"github.com/meridiandb/meridian/internal/storage"
)

func ms(d time.Duration) int64 { return d.Milliseconds() }

// TestSelectResolution exercises the planner's resolution choice directly.
func TestSelectResolution(t *testing.T) {
	avail := []int64{ms(time.Minute), ms(time.Hour)}
	span30d := ms(30 * 24 * time.Hour)

	cases := []struct {
		name              string
		span, stepMs, rng int64
		avail             []int64
		want              int64
	}{
		{"wide span, big step -> 1h", span30d, ms(3 * time.Hour), 0, avail, ms(time.Hour)},
		{"medium span, 5m step -> 1m", ms(2 * time.Hour), ms(5 * time.Minute), 0, avail, ms(time.Minute)},
		{"narrow span, small step -> raw", ms(10 * time.Minute), ms(15 * time.Second), 0, avail, 0},
		{"range selector / rate -> raw", span30d, ms(3 * time.Hour), ms(5 * time.Minute), avail, 0},
		{"no rollups available -> raw", span30d, ms(3 * time.Hour), 0, nil, 0},
		{"step below finest resolution -> raw", ms(time.Hour), ms(30 * time.Second), 0, avail, 0},
	}
	for _, c := range cases {
		got := selectResolution(0, c.span, c.stepMs, c.rng, c.avail)
		if got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// buildDownsampledDB ingests a wide span of raw data at rawIntervalMs, seals it into
// raw blocks, and runs the real 5s→1m→1h cascade so both rollup tiers exist.
func buildDownsampledDB(t *testing.T, hours int, rawIntervalMs int64) *storage.TSDB {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(dir, storage.TSDBOptions{BlockDuration: time.Hour, FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	end := int64(hours) * ms(time.Hour)
	for _, host := range []string{"web-01", "web-02"} {
		for ts := int64(0); ts < end; ts += rawIntervalMs {
			if err := db.Ingest("cpu", map[string]string{"host": host}, ts, float64(ts/1000%100)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	rules := []retention.DownsampleRule{
		{SourceInterval: 5 * time.Second, TargetInterval: time.Minute, Retention: 7 * 24 * time.Hour},
		{SourceInterval: time.Minute, TargetInterval: time.Hour, Retention: 30 * 24 * time.Hour},
	}
	retention.NewDownsampler(db, rules, time.Hour).Downsample()
	return db
}

// TestEngineResolutionWideVsNarrow is the headline behaviour: a wide span is served
// from a coarse rollup tier reading far fewer points, while a narrow span reads raw —
// all transparent to the caller (the result shape is unchanged).
func TestEngineResolutionWideVsNarrow(t *testing.T) {
	db := buildDownsampledDB(t, 8, ms(15*time.Second))
	defer db.Close()
	eng := NewEngine(db)
	ctx := context.Background()

	eightHours := 8 * ms(time.Hour)

	// Wide: 8h at a 1h step → the 1h tier. Far fewer points than raw.
	_, wide, err := eng.ExecuteWithMeta(ctx, "cpu", 0, eightHours, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if wide.ResolutionMs != ms(time.Hour) {
		t.Fatalf("wide query resolution: got %d, want %d (1h)", wide.ResolutionMs, ms(time.Hour))
	}
	rawPerSpan := int(eightHours/ms(15*time.Second)) * 2 // both series
	if wide.PointsRead >= rawPerSpan/10 {
		t.Fatalf("wide query read %d points; expected far fewer than raw (%d)", wide.PointsRead, rawPerSpan)
	}

	// Narrow: a 5-minute window at a 15s step → raw.
	_, narrow, err := eng.ExecuteWithMeta(ctx, "cpu", ms(time.Hour), ms(time.Hour)+ms(5*time.Minute), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if narrow.ResolutionMs != 0 {
		t.Fatalf("narrow query resolution: got %d, want 0 (raw)", narrow.ResolutionMs)
	}
	if narrow.PointsRead == 0 {
		t.Fatal("narrow query read no raw points")
	}

	t.Logf("wide(8h@1h): resolution=%dms points=%d | narrow(5m@15s): resolution=raw points=%d",
		wide.ResolutionMs, wide.PointsRead, narrow.PointsRead)
}
