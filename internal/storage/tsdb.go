package storage

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TSDBOptions configures the time-series database engine.
type TSDBOptions struct {
	WALDir          string
	BlockDir        string
	BlockDuration   time.Duration
	RetentionPeriod time.Duration
	FlushInterval   time.Duration
}

// DefaultTSDBOptions returns sensible defaults.
func DefaultTSDBOptions() TSDBOptions {
	return TSDBOptions{
		WALDir:          "./data/wal",
		BlockDir:        "./data/blocks",
		BlockDuration:   15 * time.Minute,
		RetentionPeriod: 15 * 24 * time.Hour,
		FlushInterval:   30 * time.Second,
	}
}

// TSDBStats holds database-level statistics.
type TSDBStats struct {
	TotalSamples int64
	TotalSeries  int
	HeadSamples  int64
	HeadSeries   int
	BlockCount   int
	// StorageBytesRaw is the cost of the data if stored as raw 16-byte (ts,val) samples.
	StorageBytesRaw int64
	// ChunkBytes is the actual Gorilla-compressed size: compressed chunk bytes across all
	// flushed blocks plus the size the current head would occupy if compressed now. This is
	// the meaningful number for the compression ratio.
	ChunkBytes int64
	// StorageBytesDisk is the on-disk footprint (block chunks + WAL), which carries WAL
	// framing overhead and is only a good compression proxy once blocks have been flushed.
	StorageBytesDisk int64
	WALSize          int64
	// OutOfOrderSamples is the running count of samples rejected for arriving out of
	// order (older than the series' last sample, or a conflicting value at the same ts).
	OutOfOrderSamples int64
}

// IngestSample represents a single sample for batch ingestion.
type IngestSample struct {
	Name      string
	Labels    map[string]string
	Timestamp int64
	Value     float64
}

// SeriesSet is a slice of result series from a query.
type SeriesSet []ResultSeries

// ResultSeries is a single series from a query result.
type ResultSeries struct {
	Name   string
	Labels map[string]string
	Points []Point
}

// TSDB is the top-level time-series database orchestrator.
type TSDB struct {
	opts      TSDBOptions
	wal       *WAL
	startTime time.Time

	// mu guards head and blocks. Ingest takes it as a reader so the WAL append and
	// the head append form one critical section; Flush takes it as a writer for the
	// brief head-swap + WAL-rotate cut, so no sample ever straddles a flush.
	mu     sync.RWMutex
	head   *HeadBlock
	blocks []*Block

	// flushMu serializes the whole Flush operation (including the out-of-lock block
	// write), so block WAL low-water-marks are recorded in increasing order.
	flushMu     sync.Mutex
	flushFailed atomic.Bool

	ingested    atomic.Int64
	outOfOrder  atomic.Int64
	flushTicker *time.Ticker
	done        chan struct{}
	closed      atomic.Bool
}

// Open creates or opens a TSDB at the given data directory.
func Open(dataDir string, opts TSDBOptions) (*TSDB, error) {
	if opts.WALDir == "" {
		opts.WALDir = filepath.Join(dataDir, "wal")
	}
	if opts.BlockDir == "" {
		opts.BlockDir = filepath.Join(dataDir, "blocks")
	}
	if opts.BlockDuration == 0 {
		opts.BlockDuration = 15 * time.Minute
	}
	if opts.FlushInterval == 0 {
		opts.FlushInterval = 30 * time.Second
	}

	if err := os.MkdirAll(opts.BlockDir, 0o755); err != nil {
		return nil, fmt.Errorf("create block dir: %w", err)
	}

	wal, err := OpenWAL(opts.WALDir)
	if err != nil {
		return nil, fmt.Errorf("open WAL: %w", err)
	}

	head := NewHeadBlock()

	db := &TSDB{
		opts:      opts,
		wal:       wal,
		head:      head,
		startTime: time.Now(),
		done:      make(chan struct{}),
	}

	// Load existing blocks from disk
	if err := db.loadBlocks(); err != nil {
		wal.Close()
		return nil, fmt.Errorf("load blocks: %w", err)
	}

	// Replay only the WAL beyond what persisted blocks already cover. A crash that
	// left both a block and its source WAL segments on disk replays exactly once.
	if err := wal.ReplayFrom(db.maxCoveredWAL(), db); err != nil {
		wal.Close()
		return nil, fmt.Errorf("replay WAL: %w", err)
	}

	// Start background flush loop
	db.flushTicker = time.NewTicker(opts.FlushInterval)
	go db.flushLoop()

	return db, nil
}

// HandleSeries implements WALHandler for replay. It restores the series under the
// exact ID it was logged with so replayed sample frames resolve correctly.
func (db *TSDB) HandleSeries(id uint64, name string, labels map[string]string) error {
	db.head.getOrCreateSeriesWithID(id, name, labels)
	return nil
}

// HandleSamples implements WALHandler for replay. It applies the same ordering
// policy as live ingest, so the replayed head is identical to the pre-crash head
// (out-of-order frames logged before the policy check are rejected identically).
func (db *TSDB) HandleSamples(samples []Sample) error {
	for _, s := range samples {
		db.head.Ingest(s.SeriesID, s.Timestamp, s.Value)
	}
	return nil
}

// maxCoveredWAL returns the highest WAL low-water-mark durably covered by a loaded
// block. Replay skips segments at or below it. Blocks predating the field report 0,
// which conservatively replays the whole WAL.
func (db *TSDB) maxCoveredWAL() int {
	maxCovered := 0
	for _, b := range db.blocks {
		if b.meta.WALLowWaterMark > maxCovered {
			maxCovered = b.meta.WALLowWaterMark
		}
	}
	return maxCovered
}

// Ingest adds a single sample to the database. Samples are written to the WAL first,
// then applied to the head under the in-order policy; out-of-order samples are
// dropped and counted, not returned as errors. Oversized names/labels are rejected.
func (db *TSDB) Ingest(name string, labels map[string]string, ts int64, val float64) error {
	if err := validateSeriesLabels(name, labels); err != nil {
		return err
	}

	// RLock spans the WAL append and the head append so the pair is atomic with
	// respect to a concurrent Flush cut (which takes the write lock).
	db.mu.RLock()
	defer db.mu.RUnlock()
	head := db.head

	series, created := head.GetOrCreateSeries(name, labels)
	if created {
		if err := db.wal.LogSeries(series.ID, name, labels); err != nil {
			return fmt.Errorf("WAL log series: %w", err)
		}
	}
	if err := db.wal.LogSamples([]Sample{{SeriesID: series.ID, Timestamp: ts, Value: val}}); err != nil {
		return fmt.Errorf("WAL log sample: %w", err)
	}

	switch head.Ingest(series.ID, ts, val) {
	case ingestAccepted:
		db.ingested.Add(1)
	case ingestOutOfOrder:
		db.outOfOrder.Add(1)
	case ingestDuplicate:
		// identical to the series' last sample — deduplicated, not stored.
	}
	return nil
}

// IngestBatch adds multiple samples to the database. The whole batch is validated
// first, then logged and applied under the RLock as one critical section.
func (db *TSDB) IngestBatch(samples []IngestSample) error {
	for i := range samples {
		if err := validateSeriesLabels(samples[i].Name, samples[i].Labels); err != nil {
			return err
		}
	}

	db.mu.RLock()
	defer db.mu.RUnlock()
	head := db.head

	walSamples := make([]Sample, 0, len(samples))
	ids := make([]uint64, len(samples))
	for i, s := range samples {
		series, created := head.GetOrCreateSeries(s.Name, s.Labels)
		if created {
			if err := db.wal.LogSeries(series.ID, s.Name, s.Labels); err != nil {
				return fmt.Errorf("WAL log series: %w", err)
			}
		}
		ids[i] = series.ID
		walSamples = append(walSamples, Sample{
			SeriesID:  series.ID,
			Timestamp: s.Timestamp,
			Value:     s.Value,
		})
	}

	if err := db.wal.LogSamples(walSamples); err != nil {
		return fmt.Errorf("WAL log samples: %w", err)
	}

	var accepted, ooo int64
	for i, s := range samples {
		switch head.Ingest(ids[i], s.Timestamp, s.Value) {
		case ingestAccepted:
			accepted++
		case ingestOutOfOrder:
			ooo++
		case ingestDuplicate:
		}
	}
	db.ingested.Add(accepted)
	db.outOfOrder.Add(ooo)
	return nil
}

// Query executes a query against the head block and all persistent blocks.
func (db *TSDB) Query(_ context.Context, matchers []LabelMatcher, start, end int64) (SeriesSet, error) {
	// Snapshot head + blocks under the lock so a concurrent flush cut can't swap the
	// head out from under us mid-query.
	db.mu.RLock()
	head := db.head
	blocks := make([]*Block, len(db.blocks))
	copy(blocks, db.blocks)
	db.mu.RUnlock()

	// Query head block
	headSeries := head.Query(matchers, start, end)

	// Merge results from head
	resultMap := make(map[string]*ResultSeries)
	for _, ms := range headSeries {
		ms.mu.Lock()
		key := seriesKey(ms.Name, ms.Labels)
		labels := make(map[string]string, len(ms.Labels)+1)
		labels["__name__"] = ms.Name
		for k, v := range ms.Labels {
			labels[k] = v
		}

		var points []Point
		for i, ts := range ms.Timestamps {
			if ts >= start && ts <= end {
				points = append(points, Point{Timestamp: ts, Value: ms.Values[i]})
			}
		}
		ms.mu.Unlock()

		if len(points) > 0 {
			resultMap[key] = &ResultSeries{
				Name:   ms.Name,
				Labels: labels,
				Points: points,
			}
		}
	}

	// Query blocks
	for _, block := range blocks {
		blockResults := block.Query(matchers, start, end)
		for _, br := range blockResults {
			// Build key without __name__ to match head key format
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
				rs := ResultSeries{
					Name:   br.Name,
					Labels: br.Labels,
					Points: br.Points,
				}
				resultMap[key] = &rs
			}
		}
	}

	result := make(SeriesSet, 0, len(resultMap))
	for _, rs := range resultMap {
		// Sort points by timestamp
		sort.Slice(rs.Points, func(i, j int) bool {
			return rs.Points[i].Timestamp < rs.Points[j].Timestamp
		})
		result = append(result, *rs)
	}

	return result, nil
}

// Series returns metadata for all known series.
func (db *TSDB) Series() []SeriesInfo {
	return db.Head().SeriesInfos()
}

// Stats returns database-level statistics.
func (db *TSDB) Stats() TSDBStats {
	db.mu.RLock()
	head := db.head
	nBlocks := len(db.blocks)
	var blockSamples int64
	var blockChunkBytes int64
	for _, b := range db.blocks {
		blockSamples += b.meta.Stats.NumSamples
		blockChunkBytes += int64(len(b.chunks))
	}
	db.mu.RUnlock()

	headSamples := head.SampleCount()
	totalSamples := headSamples + blockSamples
	rawBytes := totalSamples * 16 // 8 bytes timestamp + 8 bytes value
	headCompressed := head.CompressedSize()
	walSize := db.wal.Size()

	return TSDBStats{
		TotalSamples:      totalSamples,
		TotalSeries:       head.SeriesCount(),
		HeadSamples:       headSamples,
		HeadSeries:        head.SeriesCount(),
		BlockCount:        nBlocks,
		StorageBytesRaw:   rawBytes,
		ChunkBytes:        blockChunkBytes + headCompressed,
		StorageBytesDisk:  blockChunkBytes + walSize,
		WALSize:           walSize,
		OutOfOrderSamples: db.outOfOrder.Load(),
	}
}

// Head returns the current head block for direct access.
func (db *TSDB) Head() *HeadBlock {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.head
}

// OutOfOrderTotal returns the number of samples rejected for arriving out of order.
func (db *TSDB) OutOfOrderTotal() int64 {
	return db.outOfOrder.Load()
}

// StartTime returns when this TSDB instance was opened.
func (db *TSDB) StartTime() time.Time {
	return db.startTime
}

// IngestionRate returns the total number of ingested samples.
func (db *TSDB) IngestionRate() int64 {
	return db.ingested.Load()
}

// Flush persists the current head to a durable on-disk block. It is crash-consistent
// and loses no concurrently-ingested sample:
//
//  1. Under db.mu (writer), it captures the old head, installs a fresh head, and
//     rotates the WAL so in-flight and future writes land in a new segment. The
//     returned low-water-mark is the cut: everything the old head holds is in WAL
//     segments <= it; everything the new head will hold is beyond it.
//  2. Outside the lock, it writes the old head to a block (temp dir → fsync → atomic
//     rename → fsync parent). The rename is the single durable commit point. The
//     block records the low-water-mark.
//  3. Only after the block is durable does it best-effort delete the now-covered WAL
//     segments. Deletion is pure cleanup — replay skips covered segments by the
//     recorded low-water-mark whether or not they were deleted.
//
// If the block write fails, the old head's data is still safe in the rotated WAL
// segments (which are never deleted on failure) and is recovered on the next Open.
// Further flushes are then disabled until restart so a later flush cannot record a
// higher low-water-mark that would skip those uncovered segments on replay.
func (db *TSDB) Flush() error {
	db.flushMu.Lock()
	defer db.flushMu.Unlock()

	if db.flushFailed.Load() {
		return fmt.Errorf("flush disabled after a prior durable block write failure; restart to recover")
	}

	// Phase 1 — atomic cut.
	db.mu.Lock()
	old := db.head
	if old.SampleCount() == 0 {
		db.mu.Unlock()
		return nil
	}
	lowWaterMark, err := db.wal.Rotate()
	if err != nil {
		db.mu.Unlock()
		return fmt.Errorf("rotate WAL: %w", err)
	}
	db.head = NewHeadBlock()
	db.mu.Unlock()

	// Phase 2 — flush the old head; the rename inside WriteBlock is the commit point.
	block, err := WriteBlock(db.opts.BlockDir, old, lowWaterMark)
	if err != nil {
		db.flushFailed.Store(true)
		return fmt.Errorf("write block: %w", err)
	}

	// Phase 3 — publish the block, then best-effort cleanup.
	db.mu.Lock()
	db.blocks = append(db.blocks, block)
	db.mu.Unlock()

	if err := db.wal.RemoveSegmentsThrough(lowWaterMark); err != nil {
		log.Printf("TSDB: WAL cleanup of segments <= %d failed (non-fatal): %v", lowWaterMark, err)
	}

	return nil
}

// Close flushes pending data and shuts down the TSDB.
func (db *TSDB) Close() error {
	if db.closed.Swap(true) {
		return nil
	}

	close(db.done)
	if db.flushTicker != nil {
		db.flushTicker.Stop()
	}

	// Flush remaining head data
	db.mu.RLock()
	hasData := db.head.SampleCount() > 0
	db.mu.RUnlock()
	if hasData {
		if err := db.Flush(); err != nil {
			log.Printf("TSDB: error flushing on close: %v", err)
		}
	}

	return db.wal.Close()
}

func (db *TSDB) flushLoop() {
	for {
		select {
		case <-db.done:
			return
		case <-db.flushTicker.C:
			db.maybeFlush()
		}
	}
}

func (db *TSDB) maybeFlush() {
	db.mu.RLock()
	head := db.head
	db.mu.RUnlock()

	if head.SampleCount() == 0 {
		return
	}
	headDuration := head.MaxTime() - head.MinTime()
	if headDuration >= db.opts.BlockDuration.Milliseconds() || head.SampleCount() >= 1_000_000 {
		if err := db.Flush(); err != nil {
			log.Printf("TSDB: flush error: %v", err)
		}
	}
}

func (db *TSDB) loadBlocks() error {
	entries, err := os.ReadDir(db.opts.BlockDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, e := range entries {
		name := e.Name()
		// Leftover temp block dirs (".<ulid>.tmp") are interrupted, never-committed
		// flushes; remove them and rely on WAL replay for their data.
		if strings.HasPrefix(name, ".") {
			if e.IsDir() {
				if err := os.RemoveAll(filepath.Join(db.opts.BlockDir, name)); err != nil {
					log.Printf("TSDB: failed to remove stale temp block %s: %v", name, err)
				}
			}
			continue
		}
		if !e.IsDir() {
			continue
		}
		blockDir := filepath.Join(db.opts.BlockDir, name)
		block, err := OpenBlock(blockDir)
		if err != nil {
			log.Printf("TSDB: skipping block %s: %v", name, err)
			continue
		}
		db.blocks = append(db.blocks, block)
	}

	// Sort blocks by min time
	sort.Slice(db.blocks, func(i, j int) bool {
		return db.blocks[i].meta.MinTime < db.blocks[j].meta.MinTime
	})

	return nil
}

// DeleteBlock removes a block from the database and deletes it from disk.
func (db *TSDB) DeleteBlock(ulid string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	for i, b := range db.blocks {
		if b.meta.ULID == ulid {
			db.blocks = append(db.blocks[:i], db.blocks[i+1:]...)
			return os.RemoveAll(b.dir)
		}
	}
	return fmt.Errorf("block %s not found", ulid)
}

// Blocks returns a copy of the current block list.
func (db *TSDB) Blocks() []*Block {
	db.mu.RLock()
	defer db.mu.RUnlock()
	blocks := make([]*Block, len(db.blocks))
	copy(blocks, db.blocks)
	return blocks
}

func mergePoints(a, b []Point) []Point {
	result := make([]Point, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].Timestamp < b[j].Timestamp {
			result = append(result, a[i])
			i++
		} else if a[i].Timestamp > b[j].Timestamp {
			result = append(result, b[j])
			j++
		} else {
			// Prefer head (newer) data on collision
			result = append(result, a[i])
			i++
			j++
		}
	}
	result = append(result, a[i:]...)
	result = append(result, b[j:]...)
	return result
}

// CompressionRatio returns the ratio of raw data size to Gorilla-compressed chunk size.
// This reflects the compression algorithm's effectiveness and excludes WAL framing overhead.
func (db *TSDB) CompressionRatio() float64 {
	stats := db.Stats()
	if stats.ChunkBytes == 0 {
		return 0
	}
	return float64(stats.StorageBytesRaw) / float64(stats.ChunkBytes)
}

// LabelNames returns all known label names in the current head.
func (db *TSDB) LabelNames() []string {
	return db.Head().index.LabelNames()
}

// LabelValues returns known values for a label name in the current head.
func (db *TSDB) LabelValues(name string) []string {
	return db.Head().index.LabelValues(name)
}

// validateSeriesLabels enforces the size limits required so names and labels
// round-trip through the uint16 length fields in WAL frames and block index entries
// without truncation. Oversized input is rejected here rather than silently
// corrupting the WAL or index downstream.
func validateSeriesLabels(name string, labels map[string]string) error {
	if name == "" {
		return fmt.Errorf("metric name cannot be empty")
	}
	if len(name) > MaxMetricNameLength {
		return fmt.Errorf("metric name length %d exceeds limit %d", len(name), MaxMetricNameLength)
	}
	for k, v := range labels {
		if k == "" {
			return fmt.Errorf("label name cannot be empty")
		}
		if len(k) > MaxLabelNameLength {
			return fmt.Errorf("label name %q length %d exceeds limit %d", k, len(k), MaxLabelNameLength)
		}
		if len(v) > MaxLabelValueLength {
			return fmt.Errorf("label %q value length %d exceeds limit %d", k, len(v), MaxLabelValueLength)
		}
	}
	return nil
}
