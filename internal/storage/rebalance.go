package storage

import (
	"os"
	"sort"

	"github.com/meridiandb/meridian/internal/compress"
)

// DropResult reports what a range-targeted drop removed from a node's local store.
type DropResult struct {
	// SeriesDropped is the distinct series removed across the raw and rollup tiers.
	SeriesDropped int
	// SamplesDropped is the raw samples removed (from raw blocks and the flushed head).
	SamplesDropped int64
	// RollupWindows is the rollup windows removed across all resolutions.
	RollupWindows int64
	// BlocksRewritten is the blocks rewritten to exclude their un-owned series.
	BlocksRewritten int
	// BlocksDeleted is the blocks deleted whole because they held only un-owned series.
	BlocksDeleted int
}

// DropSeriesInRanges removes every series this node holds whose ring position (hashOf of the
// series' canonical key) falls in any of the given hash ranges — the data a node no longer
// owns after a rebalance (ADR-031). hashOf is injected by the caller (the storage node passes
// cluster.HashKey) so the classification matches how writes were routed and how RangeDigest /
// RangeExport classify, keeping the storage layer ring-agnostic.
//
// An empty range set is a no-op, so a caller can never accidentally drop everything: GC is
// always expressed as the specific arcs that moved away, never "keep only these". The drop
// spans the head, the raw blocks, and the rollup tiers:
//
//   - The head is flushed to a block first, so no dropped data lingers in the mutable head or
//     its WAL segments, where a restart would replay it back. (A flush failure is non-fatal:
//     this round simply drops less and the next round retries once the head flushes normally.)
//   - Each raw/rollup block is then either left untouched (no un-owned series), deleted whole
//     (only un-owned series), or rewritten to keep just its owned series — reusing the same
//     temp→fsync→rename commit WriteBlock uses, so a crash mid-rewrite leaves the original
//     block intact.
//
// It is idempotent (re-running over the same ranges drops nothing more) and best-effort: a
// single block's rewrite failure is skipped, not propagated, so one bad block cannot wedge a
// rebalance. Cluster-level safety — GC only after the new owners confirm receipt at quorum,
// and never the last copy — is enforced by the caller (the migration coordinator); this method
// is the mechanical local drop it issues once those conditions hold.
func (db *TSDB) DropSeriesInRanges(ranges []HashRange, hashOf func(string) uint64) (DropResult, error) {
	var res DropResult
	if len(ranges) == 0 {
		return res, nil
	}
	inDrop := func(key string) bool { return inAnyRange(hashOf(key), ranges) }

	// Move head data into a block first so a dropped series cannot survive in the head/WAL
	// and be replayed on restart. Best-effort: if the flush is disabled (a prior durable
	// write failure) the head is left as-is and only block-resident data is dropped now.
	_ = db.Flush()

	// Raw tier.
	for _, b := range db.Blocks() {
		keep, dropped := classifyRawBlock(b, inDrop)
		if dropped == 0 {
			continue
		}
		if len(keep) == 0 {
			// Whole block is un-owned: drop it outright.
			if err := db.DeleteBlock(b.meta.ULID); err != nil {
				continue
			}
			res.BlocksDeleted++
			res.SeriesDropped += dropped
			res.SamplesDropped += b.meta.Stats.NumSamples
			continue
		}
		keptSamples, err := db.rewriteRawBlockKeeping(b, keep)
		if err != nil {
			continue
		}
		res.BlocksRewritten++
		res.SeriesDropped += dropped
		// Dropped samples = everything the block held minus what the rewrite kept.
		if ds := b.meta.Stats.NumSamples - keptSamples; ds > 0 {
			res.SamplesDropped += ds
		}
	}

	// Rollup tiers (derived data — regenerable, but reclaimed here too so a shed range does
	// not keep paying for cold rollups).
	for _, resn := range db.RollupResolutions() {
		for _, b := range db.RollupBlocks(resn) {
			keepKeys, dropped, droppedWindows := classifyRollupBlock(b, inDrop)
			if dropped == 0 {
				continue
			}
			if len(keepKeys) == 0 {
				if err := db.DeleteRollupBlock(resn, b.meta.ULID); err != nil {
					continue
				}
				res.BlocksDeleted++
				res.SeriesDropped += dropped
				res.RollupWindows += b.meta.Stats.NumWindows
				continue
			}
			if err := db.rewriteRollupBlockKeeping(resn, b, keepKeys); err != nil {
				continue
			}
			res.BlocksRewritten++
			res.SeriesDropped += dropped
			res.RollupWindows += droppedWindows
		}
	}

	return res, nil
}

// classifyRawBlock partitions a raw block's series into kept indices (owned — not in the drop
// set) and a dropped count, by hashing each series' canonical key through inDrop.
func classifyRawBlock(b *Block, inDrop func(string) bool) (keep []int, dropped int) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for i, s := range b.series {
		if inDrop(seriesKey(s.name, s.labels)) {
			dropped++
			continue
		}
		keep = append(keep, i)
	}
	return keep, dropped
}

// classifyRollupBlock partitions a rollup block's series into kept keys (owned) and a dropped
// count / window tally.
func classifyRollupBlock(b *RollupBlock, inDrop func(string) bool) (keepKeys map[string]bool, dropped int, droppedWindows int64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	keepKeys = make(map[string]bool)
	for _, s := range b.series {
		key := seriesKey(s.name, s.labels)
		if inDrop(key) {
			dropped++
			droppedWindows += int64(s.windows)
			continue
		}
		keepKeys[key] = true
	}
	return keepKeys, dropped, droppedWindows
}

// rewriteRawBlockKeeping writes a new raw block holding only the kept series of src (by series
// index), atomically swaps it in for src in the block list, and removes src's directory. It
// copies each kept series' samples by decoding and re-ingesting into a transient head, then
// reuses WriteBlock's crash-safe commit — so an interrupted rewrite leaves the original block
// in place. The recorded WAL low-water-mark is preserved so replay still skips the same
// segments. Returns the kept sample count.
func (db *TSDB) rewriteRawBlockKeeping(src *Block, keepIdx []int) (int64, error) {
	tmp := NewHeadBlock()
	var keptSamples int64
	src.mu.RLock()
	for _, idx := range keepIdx {
		s := src.series[idx]
		if int(s.chunkOffset)+int(s.chunkLen) > len(src.chunks) {
			continue
		}
		dec := compress.NewDecoder(src.chunks[s.chunkOffset : s.chunkOffset+uint64(s.chunkLen)])
		series, _ := tmp.GetOrCreateSeries(s.name, s.labels)
		for dec.Next() {
			ts, val := dec.Values()
			if tmp.Ingest(series.ID, ts, val) == ingestAccepted {
				keptSamples++
			}
		}
	}
	src.mu.RUnlock()

	newBlock, err := WriteBlock(db.opts.BlockDir, tmp, src.meta.WALLowWaterMark)
	if err != nil {
		return 0, err
	}
	db.replaceRawBlock(src, newBlock)
	return keptSamples, nil
}

// rewriteRollupBlockKeeping writes a new rollup block holding only the series in keepKeys,
// reconstructed from src's full per-series windows, swaps it in for src, and removes src's
// directory. Reuses WriteRollupBlock's crash-safe commit and carries over src's covered-through
// and source-resolution provenance.
func (db *TSDB) rewriteRollupBlockKeeping(res int64, src *RollupBlock, keepKeys map[string]bool) error {
	all := src.SeriesInRange(src.meta.MinTime, src.meta.MaxTime)
	kept := make([]RollupSeriesData, 0, len(all))
	for _, s := range all {
		if keepKeys[seriesKey(s.Name, s.Labels)] {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return db.DeleteRollupBlock(res, src.meta.ULID)
	}
	newBlock, err := WriteRollupBlock(db.rollupDirFor(res), res, src.meta.CoveredThrough, src.meta.Source.SourceResolution, kept)
	if err != nil {
		return err
	}
	db.replaceRollupBlock(res, src, newBlock)
	return nil
}

// replaceRawBlock swaps old for new in the block list under the write lock, keeping the list
// ordered by min time, then removes old's directory. If old is already gone (a concurrent
// drop), the freshly written new block is discarded so it does not leak.
func (db *TSDB) replaceRawBlock(old, new *Block) {
	db.mu.Lock()
	for i, b := range db.blocks {
		if b == old {
			db.blocks[i] = new
			sort.Slice(db.blocks, func(i, j int) bool { return db.blocks[i].meta.MinTime < db.blocks[j].meta.MinTime })
			db.mu.Unlock()
			os.RemoveAll(old.dir)
			return
		}
	}
	db.mu.Unlock()
	os.RemoveAll(new.dir)
}

// replaceRollupBlock swaps old for new in a resolution's block list under the rollup lock,
// keeping it ordered by min time, then removes old's directory.
func (db *TSDB) replaceRollupBlock(res int64, old, new *RollupBlock) {
	db.rollupMu.Lock()
	blocks := db.rollupBlocks[res]
	for i, b := range blocks {
		if b == old {
			blocks[i] = new
			db.rollupBlocks[res] = blocks
			db.sortRollupLocked(res)
			db.rollupMu.Unlock()
			os.RemoveAll(old.dir)
			return
		}
	}
	db.rollupMu.Unlock()
	os.RemoveAll(new.dir)
}
