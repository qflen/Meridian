// Package retention implements TTL-based block deletion and automatic downsampling.
package retention

import (
	"log"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// DefaultEnforceInterval is how often the retention loop runs when unspecified.
const DefaultEnforceInterval = 5 * time.Minute

// Enforcer periodically deletes blocks whose data has exceeded its retention period.
// Each storage tier has its own TTL: raw blocks expire first, while the coarser rollup
// tiers are kept longer (a 30-day view is still answerable from the 1h tier long after
// the raw blocks behind it are gone). A raw block is never dropped before the finest
// rollup tier has captured its data, so shortening the raw TTL trades resolution for
// space without losing history.
type Enforcer struct {
	db           *storage.TSDB
	rawRetention time.Duration
	// rollupRetention maps a rollup resolution (ms) to its TTL. Empty means downsampling
	// is off and only the raw tier is enforced (with no coverage guard).
	rollupRetention map[int64]time.Duration
	interval        time.Duration

	ticker *time.Ticker
	done   chan struct{}
}

// NewEnforcer creates a raw-only retention enforcer (no rollup tiers) running on the
// default interval.
func NewEnforcer(db *storage.TSDB, retention time.Duration) *Enforcer {
	return NewEnforcerWithTiers(db, retention, nil, DefaultEnforceInterval)
}

// NewEnforcerWithTiers creates a per-resolution retention enforcer: raw blocks expire
// after rawRetention, and each rollup resolution after its own TTL in rollupRetention.
func NewEnforcerWithTiers(db *storage.TSDB, rawRetention time.Duration, rollupRetention map[int64]time.Duration, interval time.Duration) *Enforcer {
	if interval <= 0 {
		interval = DefaultEnforceInterval
	}
	return &Enforcer{
		db:              db,
		rawRetention:    rawRetention,
		rollupRetention: rollupRetention,
		interval:        interval,
		done:            make(chan struct{}),
	}
}

// Start begins the retention enforcement loop.
func (e *Enforcer) Start() {
	e.ticker = time.NewTicker(e.interval)
	go e.loop()
}

// Stop halts the retention enforcement loop.
func (e *Enforcer) Stop() {
	close(e.done)
	if e.ticker != nil {
		e.ticker.Stop()
	}
}

// Enforce runs a single retention check across every tier and returns the number of
// blocks deleted (raw + rollup).
func (e *Enforcer) Enforce() int {
	now := time.Now().UnixMilli()
	return e.enforceRaw(now) + e.enforceRollups(now)
}

// finestResolution returns the smallest configured rollup resolution, or 0 if none.
func (e *Enforcer) finestResolution() int64 {
	var finest int64
	for res := range e.rollupRetention {
		if finest == 0 || res < finest {
			finest = res
		}
	}
	return finest
}

// enforceRaw deletes raw blocks past the raw TTL. When downsampling is on, a block is
// kept until the finest rollup tier has covered its data, so expiring raw never loses
// history that has not yet been rolled up.
func (e *Enforcer) enforceRaw(now int64) int {
	cutoff := now - e.rawRetention.Milliseconds()
	finest := e.finestResolution()
	downsamplingOn := finest > 0
	var covered int64
	if downsamplingOn {
		covered = e.db.RollupCoveredThrough(finest)
	}

	deleted := 0
	for _, block := range e.db.Blocks() {
		meta := block.Meta()
		if meta.MaxTime >= cutoff {
			continue
		}
		if downsamplingOn && meta.MaxTime >= covered {
			// Not yet captured by the finest rollup tier — keep it so its data is not
			// lost. It becomes eligible once the downsampler advances past it.
			continue
		}
		log.Printf("Retention: deleting raw block %s (max_time=%d < cutoff=%d)", meta.ULID, meta.MaxTime, cutoff)
		if err := e.db.DeleteBlock(meta.ULID); err != nil {
			log.Printf("Retention: error deleting raw block %s: %v", meta.ULID, err)
			continue
		}
		deleted++
	}
	return deleted
}

// enforceRollups deletes rollup blocks past their per-resolution TTL.
func (e *Enforcer) enforceRollups(now int64) int {
	deleted := 0
	for res, ttl := range e.rollupRetention {
		cutoff := now - ttl.Milliseconds()
		for _, block := range e.db.RollupBlocks(res) {
			meta := block.Meta()
			if meta.MaxTime >= cutoff {
				continue
			}
			log.Printf("Retention: deleting %s rollup block %s (max_time=%d < cutoff=%d)",
				storage.ResolutionLabel(res), meta.ULID, meta.MaxTime, cutoff)
			if err := e.db.DeleteRollupBlock(res, meta.ULID); err != nil {
				log.Printf("Retention: error deleting rollup block %s: %v", meta.ULID, err)
				continue
			}
			deleted++
		}
	}
	return deleted
}

func (e *Enforcer) loop() {
	for {
		select {
		case <-e.done:
			return
		case <-e.ticker.C:
			e.Enforce()
		}
	}
}
