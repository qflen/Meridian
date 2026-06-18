package retention

import (
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// DownsampleRule defines a rollup rule for downsampling: raw (or the previous tier)
// at SourceInterval is rolled up to TargetInterval and kept for Retention.
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
