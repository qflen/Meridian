package storage

import (
	"testing"
	"time"
)

func TestRateMeterReflectsThroughputThenDecays(t *testing.T) {
	m := newRateMeter(5 * time.Second)
	base := time.Unix(1_700_000_000, 0)

	// Steady ~1000 samples/sec for 5s.
	for i := 0; i <= 5; i++ {
		m.observe(int64(i*1000), base.Add(time.Duration(i)*time.Second))
	}
	if r := m.rate(base.Add(5 * time.Second)); r < 800 || r > 1200 {
		t.Fatalf("active rate = %v, want ~1000/s", r)
	}

	// Ingestion stops: keep observing the same total across a full window. The rate
	// must return to 0 rather than staying pinned at the last value.
	stop := base.Add(5 * time.Second)
	const finalTotal = 5000
	for i := 1; i <= 6; i++ {
		m.observe(finalTotal, stop.Add(time.Duration(i)*time.Second))
	}
	if r := m.rate(stop.Add(6 * time.Second)); r != 0 {
		t.Fatalf("idle rate = %v, want 0 after a full window with no ingestion", r)
	}
}

func TestRateMeterEdgeCases(t *testing.T) {
	m := newRateMeter(5 * time.Second)
	base := time.Unix(1_700_000_000, 0)

	// Empty / single observation → 0 (cannot compute a rate yet).
	if r := m.rate(base); r != 0 {
		t.Fatalf("empty meter rate = %v, want 0", r)
	}
	m.observe(1000, base)
	if r := m.rate(base); r != 0 {
		t.Fatalf("single-observation rate = %v, want 0", r)
	}

	// A counter reset (total drops, e.g. process restart) must not produce a
	// negative rate.
	m.observe(10, base.Add(time.Second))
	if r := m.rate(base.Add(time.Second)); r < 0 {
		t.Fatalf("rate after counter reset = %v, want >= 0", r)
	}
}

func TestTSDBIngestionRateIsNotCumulative(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultTSDBOptions()
	opts.WALDir = dir + "/wal"
	opts.BlockDir = dir + "/blocks"
	opts.FlushInterval = time.Hour
	opts.RateSampleInterval = time.Hour // suppress the background sampler for determinism
	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 100; i++ {
		if err := db.Ingest("m", map[string]string{"h": "a"}, int64(i)*1000, float64(i)); err != nil {
			t.Fatal(err)
		}
	}

	// IngestedTotal is the cumulative, monotonic count behind the Prometheus
	// _total counter.
	if got := db.IngestedTotal(); got != 100 {
		t.Fatalf("IngestedTotal = %d, want cumulative 100", got)
	}
	// IngestionRate is now a windowed rate. With the sampler suppressed only the
	// open-time observation exists, so the rate is 0 — crucially NOT the cumulative
	// 100 that the old counter-as-rate returned.
	if got := db.IngestionRate(); got != 0 {
		t.Fatalf("IngestionRate = %d; expected a windowed rate (0 here), not the cumulative count", got)
	}
}
