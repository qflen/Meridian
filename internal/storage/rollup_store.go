package storage

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RollupResolutionStats summarises one rollup resolution's on-disk footprint, for the
// downsampling metrics and storage-savings figure.
type RollupResolutionStats struct {
	Resolution int64 // window size, ms
	BlockCount int
	NumWindows int64 // rollup points stored
	RawSamples int64 // raw samples those windows summarise
	ChunkBytes int64 // compressed column bytes on disk
}

// rollupDirFor returns the directory that holds blocks for a given resolution.
func (db *TSDB) rollupDirFor(resolution int64) string {
	return filepath.Join(db.opts.RollupDir, ResolutionLabel(resolution))
}

// loadRollupBlocks scans the rollup directory (one subdirectory per resolution) and
// registers every well-formed rollup block under its resolution. Stale temp dirs from
// interrupted writes are removed. A corrupt block is logged and skipped — it will be
// regenerated from raw on the next downsampling pass.
func (db *TSDB) loadRollupBlocks() error {
	resDirs, err := os.ReadDir(db.opts.RollupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, rd := range resDirs {
		if !rd.IsDir() {
			continue
		}
		resPath := filepath.Join(db.opts.RollupDir, rd.Name())
		entries, err := os.ReadDir(resPath)
		if err != nil {
			log.Printf("TSDB: skipping rollup dir %s: %v", rd.Name(), err)
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				if e.IsDir() {
					if err := os.RemoveAll(filepath.Join(resPath, name)); err != nil {
						log.Printf("TSDB: failed to remove stale temp rollup %s: %v", name, err)
					}
				}
				continue
			}
			if !e.IsDir() {
				continue
			}
			block, err := OpenRollupBlock(filepath.Join(resPath, name))
			if err != nil {
				log.Printf("TSDB: skipping rollup block %s: %v", name, err)
				continue
			}
			res := block.meta.Resolution
			db.rollupBlocks[res] = append(db.rollupBlocks[res], block)
		}
	}
	for res := range db.rollupBlocks {
		db.sortRollupLocked(res)
	}
	return nil
}

// sortRollupLocked keeps a resolution's blocks ordered by min window centre. Caller
// holds rollupMu (or is in single-threaded Open).
func (db *TSDB) sortRollupLocked(res int64) {
	blocks := db.rollupBlocks[res]
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].meta.MinTime < blocks[j].meta.MinTime })
}

// PersistRollup writes a rollup block for the given resolution and registers it. The
// dir layout (one subdirectory per resolution) and the in-memory registration are
// owned here so callers (the downsampler) only supply aggregates.
func (db *TSDB) PersistRollup(resolution, coveredThrough, sourceResolution int64, series []RollupSeriesData) (*RollupBlock, error) {
	resDir := db.rollupDirFor(resolution)
	if err := os.MkdirAll(resDir, 0o755); err != nil {
		return nil, fmt.Errorf("create rollup dir: %w", err)
	}
	block, err := WriteRollupBlock(resDir, resolution, coveredThrough, sourceResolution, series)
	if err != nil {
		return nil, err
	}
	db.rollupMu.Lock()
	db.rollupBlocks[resolution] = append(db.rollupBlocks[resolution], block)
	db.sortRollupLocked(resolution)
	db.rollupMu.Unlock()
	return block, nil
}

// RollupBlocks returns the registered rollup blocks for a resolution, ordered by time.
func (db *TSDB) RollupBlocks(resolution int64) []*RollupBlock {
	db.rollupMu.RLock()
	defer db.rollupMu.RUnlock()
	src := db.rollupBlocks[resolution]
	out := make([]*RollupBlock, len(src))
	copy(out, src)
	return out
}

// RollupResolutions returns the resolutions that currently have at least one block on
// disk, ascending.
func (db *TSDB) RollupResolutions() []int64 {
	db.rollupMu.RLock()
	defer db.rollupMu.RUnlock()
	out := make([]int64, 0, len(db.rollupBlocks))
	for res, blocks := range db.rollupBlocks {
		if len(blocks) > 0 {
			out = append(out, res)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RollupCoveredThrough returns how far (exclusive source-time bound) the given
// resolution's tier is complete, i.e. the max CoveredThrough across its blocks. Zero
// means the tier is empty. The downsampler uses this as its idempotency watermark, so
// a restart resumes from disk without re-rolling or duplicating windows.
func (db *TSDB) RollupCoveredThrough(resolution int64) int64 {
	db.rollupMu.RLock()
	defer db.rollupMu.RUnlock()
	var max int64
	for _, b := range db.rollupBlocks[resolution] {
		if b.meta.CoveredThrough > max {
			max = b.meta.CoveredThrough
		}
	}
	return max
}

// RollupFrontier returns the latest window centre present in a resolution's tier, or
// math.MinInt64 if empty. The query path uses it to decide whether the freshest
// window must be rolled up on the fly.
func (db *TSDB) RollupFrontier(resolution int64) int64 {
	db.rollupMu.RLock()
	defer db.rollupMu.RUnlock()
	frontier := int64(-1 << 62)
	for _, b := range db.rollupBlocks[resolution] {
		if b.meta.MaxTime > frontier {
			frontier = b.meta.MaxTime
		}
	}
	return frontier
}

// DeleteRollupBlock unregisters and removes a rollup block from disk.
func (db *TSDB) DeleteRollupBlock(resolution int64, ulid string) error {
	db.rollupMu.Lock()
	defer db.rollupMu.Unlock()
	blocks := db.rollupBlocks[resolution]
	for i, b := range blocks {
		if b.meta.ULID == ulid {
			db.rollupBlocks[resolution] = append(blocks[:i], blocks[i+1:]...)
			return os.RemoveAll(b.dir)
		}
	}
	return fmt.Errorf("rollup block %s (res=%d) not found", ulid, resolution)
}

// QueryResolution serves [start, end] from the given rollup resolution, using the avg
// aggregate as each series' value. A resolution of 0 falls back to the raw Query. For
// a coarse resolution it returns the persisted rollup windows that overlap the range
// and, for the most recent span the rollup tier has not closed yet, rolls up raw data
// (head + sealed blocks) on the fly so the coarse series stays complete to now. The
// seam between the two is the tier's covered-through bound, which is window-aligned, so
// the two regions never overlap. Selection is transparent to the caller — the returned
// shape is identical to Query.
func (db *TSDB) QueryResolution(ctx context.Context, matchers []LabelMatcher, start, end, resolution int64) (SeriesSet, error) {
	if resolution <= 0 {
		return db.Query(ctx, matchers, start, end)
	}

	type acc struct {
		name   string
		labels map[string]string
		pts    []Point
	}
	merged := make(map[string]*acc)
	add := func(name string, labels map[string]string, pts []Point) {
		if len(pts) == 0 {
			return
		}
		noName := make(map[string]string, len(labels))
		for k, v := range labels {
			if k != "__name__" {
				noName[k] = v
			}
		}
		key := seriesKey(name, noName)
		a := merged[key]
		if a == nil {
			a = &acc{name: name, labels: labels}
			merged[key] = a
		}
		a.pts = append(a.pts, pts...)
	}

	// Persisted rollup windows (avg column) overlapping the range.
	frontier := db.RollupCoveredThrough(resolution)
	for _, b := range db.RollupBlocks(resolution) {
		for _, qr := range b.Query(matchers, start, end, rollupColAvg) {
			add(qr.Name, qr.Labels, qr.Points)
		}
	}

	// On-the-fly tail: roll up raw data from the (window-aligned) frontier to end, so
	// the freshest, not-yet-rolled window is still served. Keep only window centres at
	// or beyond the frontier to avoid overlapping the persisted region.
	tailStart := frontier
	if tailStart < start {
		tailStart = start
	}
	if tailStart <= end {
		rawSS, err := db.Query(ctx, matchers, tailStart, end)
		if err != nil {
			return nil, err
		}
		for _, rs := range rawSS {
			var pts []Point
			for _, s := range RollupPoints(rs.Points, resolution) {
				if s.Timestamp < frontier || s.Timestamp < start || s.Timestamp > end {
					continue
				}
				pts = append(pts, Point{Timestamp: s.Timestamp, Value: s.Avg})
			}
			add(rs.Name, rs.Labels, pts)
		}
	}

	out := make(SeriesSet, 0, len(merged))
	for _, a := range merged {
		sort.Slice(a.pts, func(i, j int) bool { return a.pts[i].Timestamp < a.pts[j].Timestamp })
		// Drop any duplicate timestamps at the seam, preferring the first (persisted).
		deduped := make([]Point, 0, len(a.pts))
		var last int64 = -1 << 62
		for _, p := range a.pts {
			if p.Timestamp == last {
				continue
			}
			deduped = append(deduped, p)
			last = p.Timestamp
		}
		out = append(out, ResultSeries{Name: a.name, Labels: a.labels, Points: deduped})
	}
	return out, nil
}

// RawBlockFrontier returns the largest MaxTime across immutable raw blocks, or
// math.MinInt64 if there are none. The downsampler treats this as the global
// ingestion frontier: a rollup window is only closed once a raw block exists whose
// data extends past the window's end.
func (db *TSDB) RawBlockFrontier() int64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	frontier := int64(-1 << 62)
	for _, b := range db.blocks {
		if b.meta.MaxTime > frontier {
			frontier = b.meta.MaxTime
		}
	}
	return frontier
}

// RawBlockSeries returns, per series, the raw points held in immutable raw blocks with
// timestamps in [start, end] (inclusive). It deliberately excludes the head: the
// downsampler derives rollups only from durably-flushed, immutable data, so a rollup
// is deterministic and regenerable. Points for a series are merged across blocks and
// time-ordered.
func (db *TSDB) RawBlockSeries(start, end int64) []ResultSeries {
	db.mu.RLock()
	blocks := make([]*Block, len(db.blocks))
	copy(blocks, db.blocks)
	db.mu.RUnlock()

	resultMap := make(map[string]*ResultSeries)
	for _, block := range blocks {
		for _, br := range block.Query(nil, start, end) {
			labelsNoName := make(map[string]string, len(br.Labels))
			for k, v := range br.Labels {
				if k != "__name__" {
					labelsNoName[k] = v
				}
			}
			key := seriesKey(br.Name, labelsNoName)
			if existing, ok := resultMap[key]; ok {
				existing.Points = mergePoints(existing.Points, br.Points)
			} else {
				// Labels exclude __name__ (carried in Name), matching the rollup-series
				// convention so the rollup block index isn't double-populated.
				rs := ResultSeries{Name: br.Name, Labels: labelsNoName, Points: br.Points}
				resultMap[key] = &rs
			}
		}
	}
	out := make([]ResultSeries, 0, len(resultMap))
	for _, rs := range resultMap {
		sort.Slice(rs.Points, func(i, j int) bool { return rs.Points[i].Timestamp < rs.Points[j].Timestamp })
		out = append(out, *rs)
	}
	return out
}

// RollupTierSeries returns, per series, the rollup windows of the given resolution
// whose centre falls in [start, end] (inclusive), merged across that tier's blocks.
// The downsampler consumes this to chain a finer tier into a coarser one.
func (db *TSDB) RollupTierSeries(resolution, start, end int64) []RollupSeriesData {
	blocks := db.RollupBlocks(resolution)
	merged := make(map[string]*RollupSeriesData)
	for _, b := range blocks {
		for _, sd := range b.SeriesInRange(start, end) {
			key := seriesKey(sd.Name, sd.Labels)
			if existing, ok := merged[key]; ok {
				existing.Windows = append(existing.Windows, sd.Windows...)
			} else {
				cp := sd
				merged[key] = &cp
			}
		}
	}
	out := make([]RollupSeriesData, 0, len(merged))
	for _, sd := range merged {
		sort.Slice(sd.Windows, func(i, j int) bool { return sd.Windows[i].Timestamp < sd.Windows[j].Timestamp })
		out = append(out, *sd)
	}
	return out
}

// RollupStats reports the per-resolution on-disk footprint, ascending by resolution.
func (db *TSDB) RollupStats() []RollupResolutionStats {
	db.rollupMu.RLock()
	defer db.rollupMu.RUnlock()
	out := make([]RollupResolutionStats, 0, len(db.rollupBlocks))
	for res, blocks := range db.rollupBlocks {
		st := RollupResolutionStats{Resolution: res, BlockCount: len(blocks)}
		for _, b := range blocks {
			st.NumWindows += b.meta.Stats.NumWindows
			st.RawSamples += b.meta.Stats.RawSamples
			st.ChunkBytes += b.ChunkBytes()
		}
		if st.BlockCount > 0 {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Resolution < out[j].Resolution })
	return out
}
