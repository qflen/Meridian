package retention

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// DefaultDownsampleInterval is how often the background loop runs a downsampling pass
// when no interval is configured.
const DefaultDownsampleInterval = 1 * time.Minute

// DownsampleRule defines a rollup rule for downsampling: data at SourceInterval is
// rolled up to TargetInterval and the resulting tier is kept for Retention. The first
// rule in a cascade reads raw blocks; a later rule whose SourceInterval matches an
// earlier rule's TargetInterval reads (chains) that finer rollup tier instead.
type DownsampleRule struct {
	SourceInterval time.Duration
	TargetInterval time.Duration
	Retention      time.Duration
}

// RollupResult holds the aggregated values for a single rollup window. It is the
// canonical storage.RollupSample (min/max/sum/avg/count) — aliased here so the
// downsampling API reads naturally while a single type flows through storage, the
// rollup blocks, and the query path.
type RollupResult = storage.RollupSample

// Rollup computes per-window aggregates (min, max, sum, count, and the count-weighted
// avg) for raw points using fixed, globally-aligned windows of windowMs. It is the
// raw→coarse step of the cascade; see storage.RollupPoints.
func Rollup(points []storage.Point, windowMs int64) []RollupResult {
	return storage.RollupPoints(points, windowMs)
}

// ChainRollups derives a coarser tier from an already-rolled finer tier, weighting
// the average by Count so a 1h window built from 1m rollups equals the 1h window
// built directly from raw. The coarse interval must be a multiple of the finer one
// (the cascade guarantees it). See storage.ChainRollups.
func ChainRollups(samples []RollupResult, windowMs int64) []RollupResult {
	return storage.ChainRollups(samples, windowMs)
}

// internalRule is a rule resolved to integer milliseconds plus its source tier
// (0 = raw points, otherwise the finer rollup resolution to chain from).
type internalRule struct {
	targetMs int64
	sourceMs int64
}

// Downsampler runs the downsampling cascade over a TSDB: on each pass it advances
// every tier as far as its source is durably closed, emitting resolution-tagged
// rollup blocks. It is idempotent — each tier resumes from the covered-through
// watermark recorded on disk — so a crash mid-pass loses nothing (rollups are
// regenerable from raw).
type Downsampler struct {
	db       *storage.TSDB
	rules    []internalRule
	interval time.Duration

	mu     sync.Mutex // serializes passes
	ticker *time.Ticker
	done   chan struct{}

	passes    atomic.Int64
	generated atomic.Int64
}

// NewDownsampler creates a downsampler for db with the given cascade rules, running a
// pass every interval (DefaultDownsampleInterval when interval <= 0). Rules are
// ordered finest-target-first so a coarser tier always chains from a freshly-advanced
// finer tier within the same pass.
func NewDownsampler(db *storage.TSDB, rules []DownsampleRule, interval time.Duration) *Downsampler {
	if interval <= 0 {
		interval = DefaultDownsampleInterval
	}
	targets := make(map[int64]bool, len(rules))
	for _, r := range rules {
		targets[r.TargetInterval.Milliseconds()] = true
	}
	internal := make([]internalRule, 0, len(rules))
	for _, r := range rules {
		ir := internalRule{targetMs: r.TargetInterval.Milliseconds()}
		if src := r.SourceInterval.Milliseconds(); targets[src] {
			ir.sourceMs = src // chain from the finer rollup tier
		}
		if ir.targetMs > 0 {
			internal = append(internal, ir)
		}
	}
	// Finest target first so 1h chains from the 1m tier advanced earlier this pass.
	for i := 0; i < len(internal); i++ {
		for j := i + 1; j < len(internal); j++ {
			if internal[j].targetMs < internal[i].targetMs {
				internal[i], internal[j] = internal[j], internal[i]
			}
		}
	}
	return &Downsampler{
		db:       db,
		rules:    internal,
		interval: interval,
		done:     make(chan struct{}),
	}
}

// Start begins the background downsampling loop.
func (d *Downsampler) Start() {
	d.ticker = time.NewTicker(d.interval)
	go d.loop()
}

// Stop halts the background loop.
func (d *Downsampler) Stop() {
	close(d.done)
	if d.ticker != nil {
		d.ticker.Stop()
	}
}

func (d *Downsampler) loop() {
	for {
		select {
		case <-d.done:
			return
		case <-d.ticker.C:
			if n := d.Downsample(); n > 0 {
				log.Printf("Downsampler: generated %d rollup block(s)", n)
			}
		}
	}
}

// Downsample runs one full cascade pass and returns the number of rollup blocks
// written. It is safe to call directly (tests, on-demand). Tiers advance in order so
// a coarser tier sees the finer tier's new windows immediately.
func (d *Downsampler) Downsample() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.passes.Add(1)
	total := 0
	for _, r := range d.rules {
		n, err := d.generateTier(r)
		if err != nil {
			log.Printf("Downsampler: tier %s failed: %v", storage.ResolutionLabel(r.targetMs), err)
			continue
		}
		total += n
	}
	d.generated.Add(int64(total))
	return total
}

// generateTier advances a single resolution as far as its source is closed, writing at
// most one rollup block covering the newly-closed span.
func (d *Downsampler) generateTier(r internalRule) (int, error) {
	rolledThrough := d.db.RollupCoveredThrough(r.targetMs)

	var sourceFrontier int64
	if r.sourceMs == 0 {
		sourceFrontier = d.db.RawBlockFrontier()
	} else {
		sourceFrontier = d.db.RollupCoveredThrough(r.sourceMs)
	}

	// A window is closed only once the source extends past its end. closedThrough is
	// the largest window-aligned bound the source has reached.
	closedThrough := (sourceFrontier / r.targetMs) * r.targetMs
	if closedThrough <= rolledThrough {
		return 0, nil
	}

	var series []storage.RollupSeriesData
	if r.sourceMs == 0 {
		for _, rs := range d.db.RawBlockSeries(rolledThrough, closedThrough-1) {
			windows := storage.RollupPoints(rs.Points, r.targetMs)
			if len(windows) > 0 {
				series = append(series, storage.RollupSeriesData{Name: rs.Name, Labels: rs.Labels, Windows: windows})
			}
		}
	} else {
		for _, sd := range d.db.RollupTierSeries(r.sourceMs, rolledThrough, closedThrough-1) {
			windows := storage.ChainRollups(sd.Windows, r.targetMs)
			if len(windows) > 0 {
				series = append(series, storage.RollupSeriesData{Name: sd.Name, Labels: sd.Labels, Windows: windows})
			}
		}
	}
	if len(series) == 0 {
		// No source data in the closed span yet; leave the watermark so the span is
		// reconsidered once data arrives (cheap — no block is written).
		return 0, nil
	}

	if _, err := d.db.PersistRollup(r.targetMs, closedThrough, r.sourceMs, series); err != nil {
		return 0, err
	}
	return 1, nil
}

// Passes returns how many downsampling passes have run (for observability/tests).
func (d *Downsampler) Passes() int64 { return d.passes.Load() }

// Generated returns the cumulative number of rollup blocks written.
func (d *Downsampler) Generated() int64 { return d.generated.Load() }
