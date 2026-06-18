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

	"github.com/meridiandb/meridian/internal/compress"
)

// Rollup column order. A rollup block stores, per series, five independent
// Gorilla-compressed columns over the same window-centre timestamps. Keeping every
// aggregate lets the query path serve avg today and min/max/sum/count later (function-
// aware selection), and lets a coarser tier be chained from this one without raw data.
const (
	rollupColMin = iota
	rollupColMax
	rollupColSum
	rollupColCount
	rollupColAvg
	numRollupCols
)

// RollupBlockMeta describes a persistent rollup block. A rollup block is derived data
// (regenerable from raw), so it carries no WAL low-water-mark; a crash mid-rollup is
// recovered by regenerating from raw.
type RollupBlockMeta struct {
	ULID    string `json:"ulid"`
	MinTime int64  `json:"min_time"` // earliest window centre
	MaxTime int64  `json:"max_time"` // latest window centre
	// Resolution is the rollup window size in milliseconds (e.g. 60000 for 1m).
	Resolution int64 `json:"resolution_ms"`
	// CoveredThrough is the exclusive upper bound, in source time, that this block's
	// tier is complete to (always a multiple of Resolution). The downsampler advances
	// its per-resolution watermark to the max CoveredThrough on disk, and the query
	// path uses it as the seam below which the rollup tier is authoritative and above
	// which the freshest window is rolled up on the fly. See ADR-011.
	CoveredThrough int64           `json:"covered_through"`
	Stats          RollupStats     `json:"stats"`
	Source         RollupSourceMeta `json:"source"`
}

// RollupStats holds counts for a rollup block.
type RollupStats struct {
	NumSeries  int   `json:"num_series"`
	NumWindows int64 `json:"num_windows"` // rollup points across all series
	RawSamples int64 `json:"raw_samples"` // raw samples these windows summarise (Σ count)
}

// RollupSourceMeta records the tier this block was derived from, for provenance.
type RollupSourceMeta struct {
	// SourceResolution is the finer interval this was built from: 0 means raw points,
	// otherwise the millisecond interval of the source rollup tier.
	SourceResolution int64 `json:"source_resolution_ms"`
}

// RollupSeriesData is one series' worth of rollup windows handed to the writer.
type RollupSeriesData struct {
	Name    string
	Labels  map[string]string
	Windows []RollupSample // ascending by Timestamp (window centre)
}

// RollupBlock is an immutable, resolution-tagged, Gorilla-compressed on-disk block of
// rollup windows. It mirrors the raw Block layout (meta.json + binary index + chunks)
// but each series carries five aggregate columns instead of one value stream.
type RollupBlock struct {
	dir  string
	meta RollupBlockMeta

	mu     sync.RWMutex
	series []rollupBlockSeries
	index  map[string]map[string][]int // label → value → series indexes
	chunks []byte
}

type rollupBlockSeries struct {
	id      uint64
	name    string
	labels  map[string]string
	cols    [numRollupCols]chunkRef
	minTime int64
	maxTime int64
	windows int
}

type chunkRef struct {
	offset uint64
	length uint32
}

// WriteRollupBlock writes a rollup block atomically into resDir (the per-resolution
// rollup directory), reusing the raw block's crash-safe machinery: every file is
// written into a temp dir and fsynced, the dir is atomically renamed into place, and
// the parent is fsynced. The rename is the single durable commit point; an interrupted
// write leaves only a leftover temp dir, and because rollups are regenerable a missing
// one is simply rebuilt from raw on the next pass.
func WriteRollupBlock(resDir string, resolution, coveredThrough, sourceResolution int64, series []RollupSeriesData) (*RollupBlock, error) {
	// Drop empty series and sort by canonical key for deterministic output and IDs.
	filtered := make([]RollupSeriesData, 0, len(series))
	for _, s := range series {
		if len(s.Windows) > 0 {
			filtered = append(filtered, s)
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("no rollup windows to write")
	}
	sort.Slice(filtered, func(i, j int) bool {
		return seriesKey(filtered[i].Name, filtered[i].Labels) < seriesKey(filtered[j].Name, filtered[j].Labels)
	})

	id := generateULID()
	finalDir := filepath.Join(resDir, id)
	tmpDir := filepath.Join(resDir, "."+id+".tmp")
	chunksDir := filepath.Join(tmpDir, "chunks")
	if err := os.MkdirAll(chunksDir, 0o755); err != nil {
		return nil, fmt.Errorf("create temp rollup dir: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(tmpDir)
		}
	}()

	var (
		chunkData  []byte
		indexData  []byte
		minTime    = int64(math.MaxInt64)
		maxTime    = int64(math.MinInt64)
		numWindows int64
		rawSamples int64
		bSeries    []rollupBlockSeries
	)

	for sid, s := range filtered {
		if err := validateSeriesFields(s.Name, s.Labels); err != nil {
			return nil, fmt.Errorf("series %q: %w", s.Name, err)
		}
		windows := s.Windows
		// Defensive: ensure ascending centres so the per-column streams are monotonic.
		if !sort.SliceIsSorted(windows, func(i, j int) bool { return windows[i].Timestamp < windows[j].Timestamp }) {
			windows = append([]RollupSample(nil), windows...)
			sort.Slice(windows, func(i, j int) bool { return windows[i].Timestamp < windows[j].Timestamp })
		}

		bs := rollupBlockSeries{
			id:      uint64(sid + 1),
			name:    s.Name,
			labels:  s.Labels,
			minTime: windows[0].Timestamp,
			maxTime: windows[len(windows)-1].Timestamp,
			windows: len(windows),
		}
		for col := 0; col < numRollupCols; col++ {
			enc := compress.NewEncoder()
			for _, w := range windows {
				enc.Write(w.Timestamp, rollupColumnValue(w, col))
			}
			compressed := enc.Bytes()
			bs.cols[col] = chunkRef{offset: uint64(len(chunkData)), length: uint32(len(compressed))}
			chunkData = append(chunkData, compressed...)
		}
		bSeries = append(bSeries, bs)
		indexData = append(indexData, encodeRollupIndexEntry(bs)...)

		numWindows += int64(len(windows))
		for _, w := range windows {
			rawSamples += int64(w.Count)
		}
		if bs.minTime < minTime {
			minTime = bs.minTime
		}
		if bs.maxTime > maxTime {
			maxTime = bs.maxTime
		}
	}

	if err := writeFileSync(filepath.Join(chunksDir, "000001"), chunkData); err != nil {
		return nil, fmt.Errorf("write rollup chunks: %w", err)
	}
	if err := writeFileSync(filepath.Join(tmpDir, "index"), indexData); err != nil {
		return nil, fmt.Errorf("write rollup index: %w", err)
	}

	meta := RollupBlockMeta{
		ULID:           id,
		MinTime:        minTime,
		MaxTime:        maxTime,
		Resolution:     resolution,
		CoveredThrough: coveredThrough,
		Stats: RollupStats{
			NumSeries:  len(bSeries),
			NumWindows: numWindows,
			RawSamples: rawSamples,
		},
		Source: RollupSourceMeta{SourceResolution: sourceResolution},
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal rollup meta: %w", err)
	}
	if err := writeFileSync(filepath.Join(tmpDir, "meta.json"), metaJSON); err != nil {
		return nil, fmt.Errorf("write rollup meta: %w", err)
	}

	if err := fsyncDir(chunksDir); err != nil {
		return nil, fmt.Errorf("fsync rollup chunks dir: %w", err)
	}
	if err := fsyncDir(tmpDir); err != nil {
		return nil, fmt.Errorf("fsync rollup temp dir: %w", err)
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return nil, fmt.Errorf("commit rollup block: %w", err)
	}
	committed = true
	if err := fsyncDir(resDir); err != nil {
		return nil, fmt.Errorf("fsync rollup res dir: %w", err)
	}

	return OpenRollupBlock(finalDir)
}

func rollupColumnValue(w RollupSample, col int) float64 {
	switch col {
	case rollupColMin:
		return w.Min
	case rollupColMax:
		return w.Max
	case rollupColSum:
		return w.Sum
	case rollupColCount:
		return float64(w.Count)
	default:
		return w.Avg
	}
}

// OpenRollupBlock opens a rollup block from disk and builds its in-memory label index.
func OpenRollupBlock(dir string) (*RollupBlock, error) {
	metaData, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("read rollup meta: %w", err)
	}
	var meta RollupBlockMeta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return nil, fmt.Errorf("parse rollup meta: %w", err)
	}
	chunks, err := os.ReadFile(filepath.Join(dir, "chunks", "000001"))
	if err != nil {
		return nil, fmt.Errorf("read rollup chunks: %w", err)
	}
	indexData, err := os.ReadFile(filepath.Join(dir, "index"))
	if err != nil {
		return nil, fmt.Errorf("read rollup index: %w", err)
	}
	series, err := decodeRollupIndex(indexData)
	if err != nil {
		return nil, fmt.Errorf("decode rollup index: %w", err)
	}

	b := &RollupBlock{
		dir:    dir,
		meta:   meta,
		series: series,
		chunks: chunks,
		index:  make(map[string]map[string][]int),
	}
	for i, s := range series {
		b.addToIndex("__name__", s.name, i)
		for k, v := range s.labels {
			b.addToIndex(k, v, i)
		}
	}
	return b, nil
}

func (b *RollupBlock) addToIndex(label, value string, idx int) {
	if b.index[label] == nil {
		b.index[label] = make(map[string][]int)
	}
	b.index[label][value] = append(b.index[label][value], idx)
}

// Meta returns the block's metadata.
func (b *RollupBlock) Meta() RollupBlockMeta { return b.meta }

// Dir returns the block's directory.
func (b *RollupBlock) Dir() string { return b.dir }

// Resolution returns the block's rollup window size in milliseconds.
func (b *RollupBlock) Resolution() int64 { return b.meta.Resolution }

// ChunkBytes returns the compressed size of the block's aggregate columns on disk.
func (b *RollupBlock) ChunkBytes() int64 { return int64(len(b.chunks)) }

// Overlaps reports whether the block's window centres intersect [minT, maxT].
func (b *RollupBlock) Overlaps(minT, maxT int64) bool {
	return b.meta.MinTime <= maxT && b.meta.MaxTime >= minT
}

// Query returns, for each matching series, the chosen aggregate column as points
// (window centre → aggregate value) within [minTime, maxTime]. The avg column is the
// series value at coarse resolution; min/max/sum/count are available for future
// function-aware selection.
func (b *RollupBlock) Query(matchers []LabelMatcher, minTime, maxTime int64, col int) []QueryResult {
	if !b.Overlaps(minTime, maxTime) {
		return nil
	}
	if col < 0 || col >= numRollupCols {
		col = rollupColAvg
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	idxs := b.resolveMatchers(matchers)
	if len(idxs) == 0 {
		return nil
	}

	var results []QueryResult
	for _, idx := range idxs {
		s := b.series[idx]
		if s.maxTime < minTime || s.minTime > maxTime {
			continue
		}
		ref := s.cols[col]
		if int(ref.offset)+int(ref.length) > len(b.chunks) {
			continue
		}
		dec := compress.NewDecoder(b.chunks[ref.offset : ref.offset+uint64(ref.length)])

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
			results = append(results, QueryResult{Name: s.name, Labels: labels, Points: points})
		}
	}
	return results
}

// SeriesInRange reconstructs every series' full rollup windows (all five aggregates)
// whose centre falls in [minTime, maxTime]. The downsampler consumes this to chain a
// finer tier into a coarser one.
func (b *RollupBlock) SeriesInRange(minTime, maxTime int64) []RollupSeriesData {
	if !b.Overlaps(minTime, maxTime) {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	var out []RollupSeriesData
	for _, s := range b.series {
		if s.maxTime < minTime || s.minTime > maxTime {
			continue
		}
		cols, ok := b.decodeColumns(s)
		if !ok {
			continue
		}
		windows := make([]RollupSample, 0, len(cols[rollupColAvg]))
		n := len(cols[rollupColMin])
		for i := 0; i < n; i++ {
			ts := cols[rollupColMin][i].Timestamp
			if ts < minTime || ts > maxTime {
				continue
			}
			windows = append(windows, RollupSample{
				Timestamp: ts,
				Min:       cols[rollupColMin][i].Value,
				Max:       cols[rollupColMax][i].Value,
				Sum:       cols[rollupColSum][i].Value,
				Count:     int(cols[rollupColCount][i].Value),
				Avg:       cols[rollupColAvg][i].Value,
			})
		}
		if len(windows) > 0 {
			labels := make(map[string]string, len(s.labels))
			for k, v := range s.labels {
				labels[k] = v
			}
			out = append(out, RollupSeriesData{Name: s.name, Labels: labels, Windows: windows})
		}
	}
	return out
}

// decodeColumns decodes all five aggregate streams for a series. They share the same
// window-centre timestamps and length by construction.
func (b *RollupBlock) decodeColumns(s rollupBlockSeries) ([numRollupCols][]Point, bool) {
	var cols [numRollupCols][]Point
	for col := 0; col < numRollupCols; col++ {
		ref := s.cols[col]
		if int(ref.offset)+int(ref.length) > len(b.chunks) {
			return cols, false
		}
		dec := compress.NewDecoder(b.chunks[ref.offset : ref.offset+uint64(ref.length)])
		pts := make([]Point, 0, s.windows)
		for dec.Next() {
			ts, val := dec.Values()
			pts = append(pts, Point{Timestamp: ts, Value: val})
		}
		cols[col] = pts
	}
	return cols, true
}

// LabelNames returns the sorted label names present in this block's index.
func (b *RollupBlock) LabelNames() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	names := make([]string, 0, len(b.index))
	for name := range b.index {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (b *RollupBlock) resolveMatchers(matchers []LabelMatcher) []int {
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

// Binary index encoding for rollup series. Layout per entry:
// SeriesID(8) + NameLen(2)+Name + NumLabels(2)+[KeyLen(2)+Key+ValLen(2)+Val]... +
// 5×(ChunkOffset(8)+ChunkLen(4)) + MinTime(8) + MaxTime(8) + NumWindows(4)
func encodeRollupIndexEntry(s rollupBlockSeries) []byte {
	size := 8 + 2 + len(s.name) + 2
	for k, v := range s.labels {
		size += 2 + len(k) + 2 + len(v)
	}
	size += numRollupCols*(8+4) + 8 + 8 + 4

	buf := make([]byte, size)
	off := 0
	binary.BigEndian.PutUint64(buf[off:], s.id)
	off += 8
	binary.BigEndian.PutUint16(buf[off:], uint16(len(s.name)))
	off += 2
	copy(buf[off:], s.name)
	off += len(s.name)

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
	for col := 0; col < numRollupCols; col++ {
		binary.BigEndian.PutUint64(buf[off:], s.cols[col].offset)
		off += 8
		binary.BigEndian.PutUint32(buf[off:], s.cols[col].length)
		off += 4
	}
	binary.BigEndian.PutUint64(buf[off:], uint64(s.minTime))
	off += 8
	binary.BigEndian.PutUint64(buf[off:], uint64(s.maxTime))
	off += 8
	binary.BigEndian.PutUint32(buf[off:], uint32(s.windows))
	return buf
}

func decodeRollupIndex(data []byte) ([]rollupBlockSeries, error) {
	var series []rollupBlockSeries
	off := 0
	for off < len(data) {
		if off+10 > len(data) {
			return nil, fmt.Errorf("rollup index truncated at offset %d", off)
		}
		var s rollupBlockSeries
		s.id = binary.BigEndian.Uint64(data[off:])
		off += 8
		nameLen := int(binary.BigEndian.Uint16(data[off:]))
		off += 2
		if off+nameLen > len(data) {
			return nil, fmt.Errorf("rollup name truncated")
		}
		s.name = string(data[off : off+nameLen])
		off += nameLen

		if off+2 > len(data) {
			return nil, fmt.Errorf("rollup labels truncated")
		}
		numLabels := int(binary.BigEndian.Uint16(data[off:]))
		off += 2
		s.labels = make(map[string]string, numLabels)
		for i := 0; i < numLabels; i++ {
			if off+2 > len(data) {
				return nil, fmt.Errorf("rollup label key truncated")
			}
			kLen := int(binary.BigEndian.Uint16(data[off:]))
			off += 2
			if off+kLen > len(data) {
				return nil, fmt.Errorf("rollup label key data truncated")
			}
			k := string(data[off : off+kLen])
			off += kLen
			if off+2 > len(data) {
				return nil, fmt.Errorf("rollup label value truncated")
			}
			vLen := int(binary.BigEndian.Uint16(data[off:]))
			off += 2
			if off+vLen > len(data) {
				return nil, fmt.Errorf("rollup label value data truncated")
			}
			v := string(data[off : off+vLen])
			off += vLen
			s.labels[k] = v
		}

		colBytes := numRollupCols * (8 + 4)
		if off+colBytes+20 > len(data) {
			return nil, fmt.Errorf("rollup chunk metadata truncated")
		}
		for col := 0; col < numRollupCols; col++ {
			s.cols[col].offset = binary.BigEndian.Uint64(data[off:])
			off += 8
			s.cols[col].length = binary.BigEndian.Uint32(data[off:])
			off += 4
		}
		s.minTime = int64(binary.BigEndian.Uint64(data[off:]))
		off += 8
		s.maxTime = int64(binary.BigEndian.Uint64(data[off:]))
		off += 8
		s.windows = int(binary.BigEndian.Uint32(data[off:]))
		off += 4

		series = append(series, s)
	}
	return series, nil
}
