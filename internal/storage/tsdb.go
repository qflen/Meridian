package storage

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
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
	TotalSamples     int64
	TotalSeries      int
	HeadSamples      int64
	HeadSeries       int
	BlockCount       int
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
	head      *HeadBlock
	startTime time.Time

	mu     sync.RWMutex
	blocks []*Block

	ingested     atomic.Int64
	flushTicker  *time.Ticker
	done         chan struct{}
	closed       atomic.Bool
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

	// Replay WAL into head
	if err := wal.Replay(db); err != nil {
		wal.Close()
		return nil, fmt.Errorf("replay WAL: %w", err)
	}

	// Start background flush loop
	db.flushTicker = time.NewTicker(opts.FlushInterval)
	go db.flushLoop()

	return db, nil
}

// HandleSeries implements WALHandler for replay.
func (db *TSDB) HandleSeries(id uint64, name string, labels map[string]string) error {
	db.head.GetOrCreateSeries(name, labels)
	return nil
}

// HandleSamples implements WALHandler for replay.
func (db *TSDB) HandleSamples(samples []Sample) error {
	for _, s := range samples {
		db.head.Ingest(s.SeriesID, s.Timestamp, s.Value)
	}
	return nil
}

// Ingest adds a single sample to the database.
func (db *TSDB) Ingest(name string, labels map[string]string, ts int64, val float64) error {
	series, created := db.head.GetOrCreateSeries(name, labels)
	if created {
		if err := db.wal.LogSeries(series.ID, name, labels); err != nil {
			return fmt.Errorf("WAL log series: %w", err)
		}
	}

	if err := db.wal.LogSamples([]Sample{{SeriesID: series.ID, Timestamp: ts, Value: val}}); err != nil {
		return fmt.Errorf("WAL log sample: %w", err)
	}

	db.head.Ingest(series.ID, ts, val)
	db.ingested.Add(1)
	return nil
}

// IngestBatch adds multiple samples to the database.
func (db *TSDB) IngestBatch(samples []IngestSample) error {
	walSamples := make([]Sample, 0, len(samples))

	for _, s := range samples {
		series, created := db.head.GetOrCreateSeries(s.Name, s.Labels)
		if created {
			if err := db.wal.LogSeries(series.ID, s.Name, s.Labels); err != nil {
				return fmt.Errorf("WAL log series: %w", err)
			}
		}
		walSamples = append(walSamples, Sample{
			SeriesID:  series.ID,
			Timestamp: s.Timestamp,
			Value:     s.Value,
		})
		db.head.Ingest(series.ID, s.Timestamp, s.Value)
	}

	if err := db.wal.LogSamples(walSamples); err != nil {
		return fmt.Errorf("WAL log samples: %w", err)
	}

	db.ingested.Add(int64(len(samples)))
	return nil
}

// Query executes a query against the head block and all persistent blocks.
func (db *TSDB) Query(_ context.Context, matchers []LabelMatcher, start, end int64) (SeriesSet, error) {
	// Query head block
	headSeries := db.head.Query(matchers, start, end)

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
	db.mu.RLock()
	blocks := make([]*Block, len(db.blocks))
	copy(blocks, db.blocks)
	db.mu.RUnlock()

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
	return db.head.SeriesInfos()
}

// Stats returns database-level statistics.
func (db *TSDB) Stats() TSDBStats {
	db.mu.RLock()
	nBlocks := len(db.blocks)
	var blockSamples int64
	var blockChunkBytes int64
	for _, b := range db.blocks {
		blockSamples += b.meta.Stats.NumSamples
		blockChunkBytes += int64(len(b.chunks))
	}
	db.mu.RUnlock()

	headSamples := db.head.SampleCount()
	totalSamples := headSamples + blockSamples
	rawBytes := totalSamples * 16 // 8 bytes timestamp + 8 bytes value
	headCompressed := db.head.CompressedSize()

	return TSDBStats{
		TotalSamples:     totalSamples,
		TotalSeries:      db.head.SeriesCount(),
		HeadSamples:      headSamples,
		HeadSeries:       db.head.SeriesCount(),
		BlockCount:       nBlocks,
		StorageBytesRaw:  rawBytes,
		ChunkBytes:       blockChunkBytes + headCompressed,
		StorageBytesDisk: blockChunkBytes + db.wal.Size(),
		WALSize:          db.wal.Size(),
	}
}

// Head returns the head block for direct access.
func (db *TSDB) Head() *HeadBlock {
	return db.head
}

// StartTime returns when this TSDB instance was opened.
func (db *TSDB) StartTime() time.Time {
	return db.startTime
}

// IngestionRate returns the total number of ingested samples.
func (db *TSDB) IngestionRate() int64 {
	return db.ingested.Load()
}

// Flush forces the head block to be persisted to disk.
func (db *TSDB) Flush() error {
	if db.head.SampleCount() == 0 {
		return nil
	}

	block, err := WriteBlock(db.opts.BlockDir, db.head)
	if err != nil {
		return fmt.Errorf("write block: %w", err)
	}

	db.mu.Lock()
	db.blocks = append(db.blocks, block)
	db.mu.Unlock()

	db.head.Reset()

	if err := db.wal.Truncate(); err != nil {
		return fmt.Errorf("truncate WAL: %w", err)
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
	if db.head.SampleCount() > 0 {
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
	headDuration := db.head.MaxTime() - db.head.MinTime()
	if db.head.MinTime() == 0 {
		return
	}
	if headDuration >= db.opts.BlockDuration.Milliseconds() || db.head.SampleCount() >= 1000000 {
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
		if !e.IsDir() {
			continue
		}
		blockDir := filepath.Join(db.opts.BlockDir, e.Name())
		block, err := OpenBlock(blockDir)
		if err != nil {
			log.Printf("TSDB: skipping block %s: %v", e.Name(), err)
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

// LabelNames returns all known label names across head and blocks.
func (db *TSDB) LabelNames() []string {
	return db.head.index.LabelNames()
}

// LabelValues returns known values for a label name.
func (db *TSDB) LabelValues(name string) []string {
	return db.head.index.LabelValues(name)
}

// IngestDirect ingests directly into head without WAL (used during WAL replay).
func (db *TSDB) IngestDirect(name string, labels map[string]string, ts int64, val float64) {
	series, _ := db.head.GetOrCreateSeries(name, labels)
	db.head.Ingest(series.ID, ts, val)
}

// needed for unused import suppression
var _ = math.MaxInt64
