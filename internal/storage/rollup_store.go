package storage

import (
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
