package storage

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/meridiandb/meridian/internal/compress"
	"github.com/oklog/ulid/v2"
)

// BlockMeta contains metadata about a persistent block.
type BlockMeta struct {
	ULID       string          `json:"ulid"`
	MinTime    int64           `json:"min_time"`
	MaxTime    int64           `json:"max_time"`
	Stats      BlockStats      `json:"stats"`
	Compaction CompactionMeta  `json:"compaction"`
}

// BlockStats holds counts for a block.
type BlockStats struct {
	NumSamples int64 `json:"num_samples"`
	NumSeries  int   `json:"num_series"`
	NumChunks  int   `json:"num_chunks"`
}

// CompactionMeta tracks the compaction level and source blocks.
type CompactionMeta struct {
	Level   int      `json:"level"`
	Sources []string `json:"sources"`
}

// Block is an immutable, Gorilla-compressed on-disk block.
type Block struct {
	dir  string
	meta BlockMeta

	mu      sync.RWMutex
	series  []blockSeries
	index   map[string]map[string][]int // label → value → series indexes
	chunks  []byte                       // mmap'd or loaded chunk data
}

type blockSeries struct {
	id          uint64
	name        string
	labels      map[string]string
	chunkOffset uint64
	chunkLen    uint32
	minTime     int64
	maxTime     int64
}

// WriteBlock flushes a head block's data into a persistent compressed block.
func WriteBlock(blockDir string, head *HeadBlock) (*Block, error) {
	id := generateULID()
	dir := filepath.Join(blockDir, id)
	chunksDir := filepath.Join(dir, "chunks")

	if err := os.MkdirAll(chunksDir, 0o755); err != nil {
		return nil, fmt.Errorf("create block dir: %w", err)
	}

	allSeries := head.AllSeries()
	if len(allSeries) == 0 {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("no series to flush")
	}

	// Sort series by ID for deterministic output
	sort.Slice(allSeries, func(i, j int) bool { return allSeries[i].ID < allSeries[j].ID })

	var (
		chunkData   []byte
		indexData   []byte
		totalSamps  int64
		minTime     = int64(math.MaxInt64)
		maxTime     = int64(math.MinInt64)
		bSeries []blockSeries
	)

	for _, s := range allSeries {
		s.mu.Lock()
		if len(s.Timestamps) == 0 {
			s.mu.Unlock()
			continue
		}

		// Compress with Gorilla
		enc := compress.NewEncoder()
		for i := range s.Timestamps {
			enc.Write(s.Timestamps[i], s.Values[i])
		}
		compressed := enc.Bytes()

		seriesMinT := s.Timestamps[0]
		seriesMaxT := s.Timestamps[len(s.Timestamps)-1]
		sampleCount := len(s.Timestamps)
		s.mu.Unlock()

		chunkOffset := uint64(len(chunkData))
		chunkData = append(chunkData, compressed...)

		bs := blockSeries{
			id:          s.ID,
			name:        s.Name,
			labels:      s.Labels,
			chunkOffset: chunkOffset,
			chunkLen:    uint32(len(compressed)),
			minTime:     seriesMinT,
			maxTime:     seriesMaxT,
		}
		bSeries = append(bSeries, bs)

		// Build binary index entry
		indexData = append(indexData, encodeIndexEntry(bs)...)

		totalSamps += int64(sampleCount)
		if seriesMinT < minTime {
			minTime = seriesMinT
		}
		if seriesMaxT > maxTime {
			maxTime = seriesMaxT
		}
	}

	// Write chunk file
	if err := os.WriteFile(filepath.Join(chunksDir, "000001"), chunkData, 0o644); err != nil {
		return nil, fmt.Errorf("write chunks: %w", err)
	}

	// Write index
	if err := os.WriteFile(filepath.Join(dir, "index"), indexData, 0o644); err != nil {
		return nil, fmt.Errorf("write index: %w", err)
	}

	// Write meta.json
	meta := BlockMeta{
		ULID:    id,
		MinTime: minTime,
		MaxTime: maxTime,
		Stats: BlockStats{
			NumSamples: totalSamps,
			NumSeries:  len(bSeries),
			NumChunks:  len(bSeries),
		},
		Compaction: CompactionMeta{Level: 1},
	}
	metaJSON, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), metaJSON, 0o644); err != nil {
		return nil, fmt.Errorf("write meta: %w", err)
	}

	// Write empty tombstones
	os.WriteFile(filepath.Join(dir, "tombstones"), []byte{}, 0o644)

	return OpenBlock(dir)
}

// OpenBlock opens a persistent block from disk.
func OpenBlock(dir string) (*Block, error) {
	metaData, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("read meta: %w", err)
	}
	var meta BlockMeta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return nil, fmt.Errorf("parse meta: %w", err)
	}

	chunks, err := os.ReadFile(filepath.Join(dir, "chunks", "000001"))
	if err != nil {
		return nil, fmt.Errorf("read chunks: %w", err)
	}

	indexData, err := os.ReadFile(filepath.Join(dir, "index"))
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}

	series, err := decodeIndex(indexData)
	if err != nil {
		return nil, fmt.Errorf("decode index: %w", err)
	}

	b := &Block{
		dir:    dir,
		meta:   meta,
		series: series,
		chunks: chunks,
		index:  make(map[string]map[string][]int),
	}

	// Build in-memory label index
	for i, s := range series {
		b.addToIndex("__name__", s.name, i)
		for k, v := range s.labels {
			b.addToIndex(k, v, i)
		}
	}

	return b, nil
}

func (b *Block) addToIndex(label, value string, idx int) {
	if b.index[label] == nil {
		b.index[label] = make(map[string][]int)
	}
	b.index[label][value] = append(b.index[label][value], idx)
}

// Meta returns the block's metadata.
func (b *Block) Meta() BlockMeta {
	return b.meta
}

// Dir returns the block's directory path.
func (b *Block) Dir() string {
	return b.dir
}

// Overlaps returns true if the block's time range overlaps [minT, maxT].
func (b *Block) Overlaps(minT, maxT int64) bool {
	return b.meta.MinTime <= maxT && b.meta.MaxTime >= minT
}

// Query returns matching series data within the time range.
func (b *Block) Query(matchers []LabelMatcher, minTime, maxTime int64) []QueryResult {
	// Predicate pushdown: skip entire block if time range doesn't overlap
	if !b.Overlaps(minTime, maxTime) {
		return nil
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	// Find matching series indexes
	idxs := b.resolveMatchers(matchers)
	if len(idxs) == 0 {
		return nil
	}

	var results []QueryResult
	for _, idx := range idxs {
		s := b.series[idx]
		// Per-series time range check
		if s.maxTime < minTime || s.minTime > maxTime {
			continue
		}

		// Decode chunk
		if int(s.chunkOffset)+int(s.chunkLen) > len(b.chunks) {
			continue
		}
		chunkData := b.chunks[s.chunkOffset : s.chunkOffset+uint64(s.chunkLen)]
		dec := compress.NewDecoder(chunkData)

		labels := make(map[string]string, len(s.labels)+1)
		labels["__name__"] = s.name
		for k, v := range s.labels {
			labels[k] = v
		}

		var points []Point
		for dec.Next() {
			ts, val := dec.Values()
			if ts >= minTime && ts <= maxTime {
				points = append(points, Point{Timestamp: ts, Value: val})
			}
		}

		if len(points) > 0 {
			results = append(results, QueryResult{
				Name:   s.name,
				Labels: labels,
				Points: points,
			})
		}
	}
	return results
}

// QueryResult holds the decoded data for a single series from a block query.
type QueryResult struct {
	Name   string
	Labels map[string]string
	Points []Point
}

// Point is a single timestamp-value data point.
type Point struct {
	Timestamp int64
	Value     float64
}

func (b *Block) resolveMatchers(matchers []LabelMatcher) []int {
	if len(matchers) == 0 {
		result := make([]int, len(b.series))
		for i := range result {
			result[i] = i
		}
		return result
	}

	var result []int
	first := true

	for _, m := range matchers {
		var idxs []int
		switch m.Type {
		case MatchEqual:
			if vals, ok := b.index[m.Name]; ok {
				idxs = vals[m.Value]
			}
		case MatchNotEqual:
			if vals, ok := b.index[m.Name]; ok {
				for v, ids := range vals {
					if v != m.Value {
						idxs = mergeIntsSorted(idxs, ids)
					}
				}
			}
		case MatchRegexp:
			re, err := compileAnchored(m.Value)
			if err != nil {
				continue
			}
			if vals, ok := b.index[m.Name]; ok {
				for v, ids := range vals {
					if re.MatchString(v) {
						idxs = mergeIntsSorted(idxs, ids)
					}
				}
			}
		case MatchNotRegexp:
			re, err := compileAnchored(m.Value)
			if err != nil {
				continue
			}
			if vals, ok := b.index[m.Name]; ok {
				for v, ids := range vals {
					if !re.MatchString(v) {
						idxs = mergeIntsSorted(idxs, ids)
					}
				}
			}
		}

		if first {
			result = make([]int, len(idxs))
			copy(result, idxs)
			first = false
		} else {
			result = intersectIntsSorted(result, idxs)
		}
		if len(result) == 0 {
			return nil
		}
	}
	return result
}

func intersectIntsSorted(a, b []int) []int {
	result := make([]int, 0, min(len(a), len(b)))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			result = append(result, a[i])
			i++
			j++
		} else if a[i] < b[j] {
			i++
		} else {
			j++
		}
	}
	return result
}

func mergeIntsSorted(a, b []int) []int {
	if len(a) == 0 {
		return b
	}
	result := make([]int, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			result = append(result, a[i])
			i++
			j++
		} else if a[i] < b[j] {
			result = append(result, a[i])
			i++
		} else {
			result = append(result, b[j])
			j++
		}
	}
	result = append(result, a[i:]...)
	result = append(result, b[j:]...)
	return result
}

// Binary index encoding/decoding

func encodeIndexEntry(s blockSeries) []byte {
	// SeriesID(8) + NameLen(2) + Name + NumLabels(2) + Labels +
	// ChunkOffset(8) + ChunkLen(4) + MinTime(8) + MaxTime(8)
	size := 8 + 2 + len(s.name) + 2
	for k, v := range s.labels {
		size += 2 + len(k) + 2 + len(v)
	}
	size += 8 + 4 + 8 + 8

	buf := make([]byte, size)
	off := 0

	binary.BigEndian.PutUint64(buf[off:], s.id)
	off += 8
	binary.BigEndian.PutUint16(buf[off:], uint16(len(s.name)))
	off += 2
	copy(buf[off:], s.name)
	off += len(s.name)

	// Sort label keys for deterministic encoding
	keys := make([]string, 0, len(s.labels))
	for k := range s.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	binary.BigEndian.PutUint16(buf[off:], uint16(len(s.labels)))
	off += 2
	for _, k := range keys {
		v := s.labels[k]
		binary.BigEndian.PutUint16(buf[off:], uint16(len(k)))
		off += 2
		copy(buf[off:], k)
		off += len(k)
		binary.BigEndian.PutUint16(buf[off:], uint16(len(v)))
		off += 2
		copy(buf[off:], v)
		off += len(v)
	}

	binary.BigEndian.PutUint64(buf[off:], s.chunkOffset)
	off += 8
	binary.BigEndian.PutUint32(buf[off:], s.chunkLen)
	off += 4
	binary.BigEndian.PutUint64(buf[off:], uint64(s.minTime))
	off += 8
	binary.BigEndian.PutUint64(buf[off:], uint64(s.maxTime))

	return buf
}

func decodeIndex(data []byte) ([]blockSeries, error) {
	var series []blockSeries
	off := 0

	for off < len(data) {
		if off+12 > len(data) {
			return nil, fmt.Errorf("index truncated at offset %d", off)
		}

		var s blockSeries
		s.id = binary.BigEndian.Uint64(data[off:])
		off += 8
		nameLen := int(binary.BigEndian.Uint16(data[off:]))
		off += 2
		if off+nameLen > len(data) {
			return nil, fmt.Errorf("name truncated")
		}
		s.name = string(data[off : off+nameLen])
		off += nameLen

		if off+2 > len(data) {
			return nil, fmt.Errorf("labels truncated")
		}
		numLabels := int(binary.BigEndian.Uint16(data[off:]))
		off += 2

		s.labels = make(map[string]string, numLabels)
		for i := 0; i < numLabels; i++ {
			if off+2 > len(data) {
				return nil, fmt.Errorf("label key truncated")
			}
			kLen := int(binary.BigEndian.Uint16(data[off:]))
			off += 2
			if off+kLen > len(data) {
				return nil, fmt.Errorf("label key data truncated")
			}
			k := string(data[off : off+kLen])
			off += kLen

			if off+2 > len(data) {
				return nil, fmt.Errorf("label value truncated")
			}
			vLen := int(binary.BigEndian.Uint16(data[off:]))
			off += 2
			if off+vLen > len(data) {
				return nil, fmt.Errorf("label value data truncated")
			}
			v := string(data[off : off+vLen])
			off += vLen

			s.labels[k] = v
		}

		if off+28 > len(data) {
			return nil, fmt.Errorf("chunk metadata truncated")
		}
		s.chunkOffset = binary.BigEndian.Uint64(data[off:])
		off += 8
		s.chunkLen = binary.BigEndian.Uint32(data[off:])
		off += 4
		s.minTime = int64(binary.BigEndian.Uint64(data[off:]))
		off += 8
		s.maxTime = int64(binary.BigEndian.Uint64(data[off:]))
		off += 8

		series = append(series, s)
	}

	return series, nil
}

func generateULID() string {
	entropy := ulid.DefaultEntropy()
	id := ulid.MustNew(ulid.Timestamp(time.Now()), entropy)
	return id.String()
}
