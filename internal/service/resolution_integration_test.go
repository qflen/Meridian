package service_test

import (
	"context"
	"fmt"
	"math"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/query"
	"github.com/meridiandb/meridian/internal/retention"
	"github.com/meridiandb/meridian/internal/service"
	"github.com/meridiandb/meridian/internal/storage"
)

// These tests prove the cluster query path selects a rollup resolution and aggregate column
// exactly like the monolith: the querier's engine (driven by service.StorageClient, which
// implements query.ResolutionDataSource) plans a resolution from the span/step, the storage
// nodes serve the matching coarse column, and the client merges the coarse results across
// replicas — closing the formerly raw-only cluster gap (ADR-011).

func ms(d time.Duration) int64 { return d.Milliseconds() }

// rawCadence is the raw sample spacing used by the cluster fixtures: coarse enough to keep
// the WAL fsync-per-write volume (and so the test) cheap, fine enough that every 1h window
// summarises 30 raw samples. The +5s offset keeps samples off the 1m/1h boundaries so a
// coarse aggregate over a resolution-aligned window equals the raw aggregate exactly.
const (
	rawCadence = 2 * time.Minute
	rawOffset  = 5 * time.Second
)

// rawOnlyTSDB exposes only query.DataSource (not ResolutionDataSource), so an engine built
// over it is forced to read raw — the reference for "coarse cluster equals raw".
type rawOnlyTSDB struct{ db *storage.TSDB }

func (r rawOnlyTSDB) Query(ctx context.Context, m []storage.LabelMatcher, start, end int64) (storage.SeriesSet, error) {
	return r.db.Query(ctx, m, start, end)
}

// openDownsampleDB opens an isolated TSDB with long block/flush intervals so an explicit
// Flush seals everything into one block and no background flush races the test.
func openDownsampleDB(t *testing.T) *storage.TSDB {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(dir, storage.TSDBOptions{
		WALDir:        filepath.Join(dir, "wal"),
		BlockDir:      filepath.Join(dir, "blocks"),
		RollupDir:     filepath.Join(dir, "rollups"),
		BlockDuration: time.Hour,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("open TSDB: %v", err)
	}
	return db
}

func clusterDownsampleRules() []retention.DownsampleRule {
	return []retention.DownsampleRule{
		{SourceInterval: rawCadence, TargetInterval: time.Minute, Retention: 7 * 24 * time.Hour},
		{SourceInterval: time.Minute, TargetInterval: time.Hour, Retention: 30 * 24 * time.Hour},
	}
}

// startDownsampledCluster brings up `count` in-process storage nodes, writes `hours` of data
// through the replicated client (a gauge sawtooth `cpu` and a monotonic counter
// `requests_total`, two hosts each), then flushes and runs the raw→1m→1h cascade on every
// node so the rollup tiers exist. N = count, so every series replicates to every node — all
// nodes hold the same data and roll up the same tiers (the steady state). It returns the
// nodes and a health-refreshed client.
func startDownsampledCluster(t *testing.T, count, hours int) ([]*replNode, *service.StorageClient) {
	t.Helper()
	nodes := make([]*replNode, count)
	for i := 0; i < count; i++ {
		db := openDownsampleDB(t)
		n := &replNode{id: fmt.Sprintf("storage-%d", i+1), db: db}
		n.alive.Store(true)
		n.srv = httptest.NewServer(replMux(n))
		t.Cleanup(func() {
			n.srv.Close()
			db.Close()
		})
		nodes[i] = n
	}
	q := count/2 + 1
	sc := service.NewReplicatedStorageClient(replAddrs(nodes), replOpts(count, q, q))
	sc.RefreshHealth()

	end := int64(hours) * ms(time.Hour)
	step := rawCadence.Milliseconds()
	for _, host := range []string{"web-01", "web-02"} {
		off := 0.0
		if host == "web-02" {
			off = 50.0
		}
		var cpu, reqs []service.Sample
		for ts := rawOffset.Milliseconds(); ts < end; ts += step {
			cpu = append(cpu, service.Sample{TimestampMs: ts, Value: float64((ts/1000)%100) + off})
			// Monotonic counter rising 1 per second of source time → a true rate of 1.0/s.
			reqs = append(reqs, service.Sample{TimestampMs: ts, Value: float64(ts) / 1000.0})
		}
		if _, err := sc.Write(context.Background(), writeOneSeries("cpu", map[string]string{"host": host}, cpu)); err != nil {
			t.Fatalf("write cpu %s: %v", host, err)
		}
		if _, err := sc.Write(context.Background(), writeOneSeries("requests_total", map[string]string{"host": host}, reqs)); err != nil {
			t.Fatalf("write requests_total %s: %v", host, err)
		}
	}

	for _, n := range nodes {
		if err := n.db.Flush(); err != nil {
			t.Fatalf("flush %s: %v", n.id, err)
		}
		retention.NewDownsampler(n.db, clusterDownsampleRules(), time.Hour).Downsample()
	}
	sc.RefreshHealth() // re-discover now that the rollup tiers exist
	return nodes, sc
}

// TestCluster_ResolutionSelection runs the read-side assertions against one shared
// downsampled cluster: a wide span is served from a coarse tier (far fewer points,
// resolution_ms set) while a narrow span reads raw; function-aware columns return distinct
// values that each equal the raw aggregate; and rate() is served from the increase column.
func TestCluster_ResolutionSelection(t *testing.T) {
	nodes, sc := startDownsampledCluster(t, 3, 8)
	clusterEng := query.NewEngine(sc)
	rawEng := query.NewEngine(rawOnlyTSDB{nodes[0].db}) // node-1 holds all data (N=cluster)
	ctx := context.Background()
	oneHour := ms(time.Hour)

	t.Run("wide span coarse, narrow span raw", func(t *testing.T) {
		eightHours := 8 * oneHour
		_, wide, err := clusterEng.ExecuteWithMeta(ctx, "cpu", 0, eightHours, time.Hour)
		if err != nil {
			t.Fatalf("wide query: %v", err)
		}
		if wide.ResolutionMs != oneHour {
			t.Fatalf("wide cluster query resolution_ms=%d, want %d (the 1h tier was not selected)", wide.ResolutionMs, oneHour)
		}
		rawPerSpan := int(eightHours/rawCadence.Milliseconds()) * 2 // both cpu hosts
		if wide.PointsRead*4 >= rawPerSpan {
			t.Fatalf("wide query read %d points; expected far fewer than raw (~%d)", wide.PointsRead, rawPerSpan)
		}

		_, narrow, err := clusterEng.ExecuteWithMeta(ctx, "cpu", oneHour, oneHour+ms(5*time.Minute), 15*time.Second)
		if err != nil {
			t.Fatalf("narrow query: %v", err)
		}
		if narrow.ResolutionMs != 0 {
			t.Fatalf("narrow cluster query resolution_ms=%d, want 0 (raw)", narrow.ResolutionMs)
		}
		if narrow.PointsRead == 0 {
			t.Fatal("narrow query read no raw points")
		}
		t.Logf("cluster wide(8h@1h)=%dms points=%d | narrow(5m@15s)=raw points=%d",
			wide.ResolutionMs, wide.PointsRead, narrow.PointsRead)
	})

	t.Run("function-aware columns equal raw", func(t *testing.T) {
		start, end, stp := oneHour, 5*oneHour, time.Hour
		vals := map[string]map[string]map[int64]float64{} // fn -> sig -> ts -> value
		for _, fn := range []string{"max_over_time", "min_over_time"} {
			q := fn + "(cpu[1h])"
			coarse, meta, err := clusterEng.ExecuteWithMeta(ctx, q, start, end, stp)
			if err != nil {
				t.Fatalf("%s coarse: %v", q, err)
			}
			if meta.ResolutionMs != oneHour {
				t.Fatalf("%s: cluster resolution_ms=%d, want %d (coarse tier not selected)", q, meta.ResolutionMs, oneHour)
			}
			raw, _, err := rawEng.ExecuteWithMeta(ctx, q, start, end, stp)
			if err != nil {
				t.Fatalf("%s raw: %v", q, err)
			}
			assertSameSeries(t, q, coarse, raw)
			vals[fn] = seriesValues(coarse)
		}
		// The two columns must actually differ: max ≥ min everywhere, strictly greater somewhere.
		strictlyGreater := 0
		for sig, maxTS := range vals["max_over_time"] {
			for ts, mx := range maxTS {
				mn, ok := vals["min_over_time"][sig][ts]
				if !ok {
					continue
				}
				if mx < mn {
					t.Fatalf("max_over_time %v < min_over_time at ts=%d (%v < %v)", sig, ts, mx, mn)
				}
				if mx > mn {
					strictlyGreater++
				}
			}
		}
		if strictlyGreater == 0 {
			t.Fatal("max_over_time never exceeded min_over_time — the cluster did not read distinct columns")
		}
		t.Logf("function-aware columns across cluster: max>min at %d points, both equal raw", strictlyGreater)
	})

	t.Run("rate on rollup matches raw", func(t *testing.T) {
		// Start at 2h so every window is fully attributed: the dataset's very first window
		// under-counts by one inter-sample delta (the increase baseline; ADR-025). The span
		// must yield ≥4 windows for the 1h tier to be chosen (minCoarsePoints), hence [2h,6h].
		start, end, stp := 2*oneHour, 6*oneHour, time.Hour
		q := "rate(requests_total[1h])"
		coarse, meta, err := clusterEng.ExecuteWithMeta(ctx, q, start, end, stp)
		if err != nil {
			t.Fatalf("%s coarse: %v", q, err)
		}
		if meta.ResolutionMs != oneHour {
			t.Fatalf("%s: cluster resolution_ms=%d, want %d (rate not served from the increase column)", q, meta.ResolutionMs, oneHour)
		}
		raw, _, err := rawEng.ExecuteWithMeta(ctx, q, start, end, stp)
		if err != nil {
			t.Fatalf("%s raw: %v", q, err)
		}
		assertRateClose(t, q, coarse, raw, 2.0)
	})
}

// TestCluster_WideQueryWithReplicaDown proves a wide coarse query still completes (and stays
// coarse, equal to raw) when one replica is down — the quorum path on the coarse read.
func TestCluster_WideQueryWithReplicaDown(t *testing.T) {
	nodes, sc := startDownsampledCluster(t, 3, 6)
	rawEng := query.NewEngine(rawOnlyTSDB{nodes[0].db}) // a surviving node, holds all data

	nodes[2].down() // N=3, R=2 → quorum still holds with two live replicas
	sc.RefreshHealth()

	eng := query.NewEngine(sc)
	ctx := context.Background()
	start, end, stp := ms(time.Hour), 5*ms(time.Hour), time.Hour
	q := "max_over_time(cpu[1h])"

	coarse, meta, err := eng.ExecuteWithMeta(ctx, q, start, end, stp)
	if err != nil {
		t.Fatalf("wide query with one replica down should succeed: %v", err)
	}
	if meta.ResolutionMs != ms(time.Hour) {
		t.Fatalf("with one replica down: resolution_ms=%d, want %d (still coarse)", meta.ResolutionMs, ms(time.Hour))
	}
	if meta.PointsRead == 0 {
		t.Fatal("expected coarse points from the surviving replicas")
	}
	// Correctness is not sacrificed for availability: the coarse result still equals raw.
	raw, _, err := rawEng.ExecuteWithMeta(ctx, q, start, end, stp)
	if err != nil {
		t.Fatalf("%s raw: %v", q, err)
	}
	assertSameSeries(t, q+" (replica down)", coarse, raw)
	t.Logf("wide coarse query with one replica down: resolution=%dms points=%d, equals raw", meta.ResolutionMs, meta.PointsRead)
}

// ── comparison helpers ───────────────────────────────────────────────────────

func seriesSig(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(0)
	}
	return b.String()
}

func seriesValues(ss []query.ResultSeries) map[string]map[int64]float64 {
	out := make(map[string]map[int64]float64, len(ss))
	for _, s := range ss {
		m := make(map[int64]float64, len(s.Points))
		for _, p := range s.Points {
			m[p.Timestamp] = p.Value
		}
		out[seriesSig(s.Labels)] = m
	}
	return out
}

// assertSameSeries requires got and want to hold the same series with identical timestamps
// and values (within float jitter) — used where the coarse aggregate is exact (max/min/etc.).
func assertSameSeries(t *testing.T, label string, got, want []query.ResultSeries) {
	t.Helper()
	wantByKey := map[string][]storage.Point{}
	for _, s := range want {
		wantByKey[seriesSig(s.Labels)] = s.Points
	}
	if len(got) != len(want) {
		t.Fatalf("%s: %d coarse series vs %d raw series", label, len(got), len(want))
	}
	compared := 0
	for _, gs := range got {
		wp, ok := wantByKey[seriesSig(gs.Labels)]
		if !ok {
			t.Fatalf("%s: coarse series %v has no raw counterpart", label, gs.Labels)
		}
		if len(gs.Points) != len(wp) {
			t.Fatalf("%s [%v]: %d coarse points vs %d raw points", label, gs.Labels, len(gs.Points), len(wp))
		}
		for i := range gs.Points {
			if gs.Points[i].Timestamp != wp[i].Timestamp {
				t.Fatalf("%s [%v] point %d: coarse ts %d != raw ts %d", label, gs.Labels, i, gs.Points[i].Timestamp, wp[i].Timestamp)
			}
			if math.Abs(gs.Points[i].Value-wp[i].Value) > 1e-9 {
				t.Fatalf("%s [%v] point %d (ts=%d): coarse %v != raw %v", label, gs.Labels, i, gs.Points[i].Timestamp, gs.Points[i].Value, wp[i].Value)
			}
			compared++
		}
	}
	if compared == 0 {
		t.Fatalf("%s: compared no points", label)
	}
}

// assertRateClose requires every aligned coarse point to be within pct% of the raw value —
// the coarse window-averaged rate is an approximation, not exact.
func assertRateClose(t *testing.T, label string, got, want []query.ResultSeries, pct float64) {
	t.Helper()
	wantVals := seriesValues(want)
	compared := 0
	for _, gs := range got {
		wm, ok := wantVals[seriesSig(gs.Labels)]
		if !ok {
			t.Fatalf("%s: coarse series %v has no raw counterpart", label, gs.Labels)
		}
		for _, p := range gs.Points {
			rv, ok := wm[p.Timestamp]
			if !ok || math.Abs(rv) < 1e-9 {
				continue
			}
			rel := math.Abs(p.Value-rv) / math.Abs(rv) * 100
			if rel > pct {
				t.Fatalf("%s [%v] ts=%d: coarse %v vs raw %v (%.2f%% > %.2f%%)", label, gs.Labels, p.Timestamp, p.Value, rv, rel, pct)
			}
			compared++
		}
	}
	if compared == 0 {
		t.Fatalf("%s: compared no points", label)
	}
}
