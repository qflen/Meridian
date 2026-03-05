package storage

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/meridiandb/meridian/internal/compress"
)

const (
	// Field-size limits. Metric names and label keys/values are length-prefixed
	// with a uint16 in both WAL frames (wal.go) and block index entries (block.go),
	// so they must fit in 65535 bytes to round-trip without silent truncation.
	// These are enforced at ingest; oversized input is rejected, never truncated.
	MaxMetricNameLength = 4096
	MaxLabelNameLength  = 1024
	MaxLabelValueLength = 1<<16 - 1
	maxFieldLen         = 1<<16 - 1 // hard uint16 limit for length-prefixed fields
)

// ingestResult reports the outcome of an ordered append to the head.
type ingestResult int

const (
	ingestAccepted   ingestResult = iota // appended to the series
	ingestDuplicate                      // identical to the series' last sample; deduplicated
	ingestOutOfOrder                     // older than the series' last sample, or a conflicting value at the same ts
)

// HeadBlock is the in-memory active write buffer. All incoming samples go here first.
type HeadBlock struct {
	mu         sync.RWMutex
	series     map[uint64]*MemSeries
	seriesByKey map[string]uint64 // "name{sorted_labels}" → seriesID
	index      *InvertedIndex
	minTime    atomic.Int64
	maxTime    atomic.Int64
	numSamples atomic.Int64
	nextID     atomic.Uint64
}

// MemSeries holds the in-memory samples for a single time series.
type MemSeries struct {
	ID     uint64
	Name   string
	Labels map[string]string

	mu         sync.Mutex
	Timestamps []int64
	Values     []float64
}

// InvertedIndex maps label name/value pairs to sorted sets of series IDs.
type InvertedIndex struct {
	mu       sync.RWMutex
	postings map[string]map[string][]uint64 // label → value → sorted seriesID list
}

// LabelMatcher selects series by label.
type LabelMatcher struct {
	Name  string
	Value string
	Type  MatchType
}

// MatchType specifies how a label matcher compares values.
type MatchType int

const (
	// MatchEqual matches series where the label equals the value.
	MatchEqual MatchType = iota
	// MatchNotEqual matches series where the label does not equal the value.
	MatchNotEqual
	// MatchRegexp matches series where the label matches the regex.
	MatchRegexp
	// MatchNotRegexp matches series where the label does not match the regex.
	MatchNotRegexp
)

// NewHeadBlock creates a new empty head block.
func NewHeadBlock() *HeadBlock {
	h := &HeadBlock{
		series:      make(map[uint64]*MemSeries),
		seriesByKey: make(map[string]uint64),
		index:       NewInvertedIndex(),
	}
	// Sentinels so "no data" is distinct from a real sample at ts==0. Callers must
	// gate on SampleCount()>0 before trusting MinTime/MaxTime.
	h.minTime.Store(math.MaxInt64)
	h.maxTime.Store(math.MinInt64)
	return h
}

// copyLabels returns an independent copy of a labels map so that a caller reusing
// or pooling its map cannot mutate the stored series or the inverted index.
func copyLabels(labels map[string]string) map[string]string {
	cp := make(map[string]string, len(labels))
	for k, v := range labels {
		cp[k] = v
	}
	return cp
}

// GetOrCreateSeries returns an existing series or creates a new one.
func (h *HeadBlock) GetOrCreateSeries(name string, labels map[string]string) (*MemSeries, bool) {
	key := seriesKey(name, labels)

	h.mu.RLock()
	if id, ok := h.seriesByKey[key]; ok {
		s := h.series[id]
		h.mu.RUnlock()
		return s, false
	}
	h.mu.RUnlock()

	h.mu.Lock()
	defer h.mu.Unlock()

	// Double-check under write lock
	if id, ok := h.seriesByKey[key]; ok {
		return h.series[id], false
	}

	id := h.nextID.Add(1)
	cp := copyLabels(labels)
	s := &MemSeries{
		ID:     id,
		Name:   name,
		Labels: cp,
	}
	h.series[id] = s
	h.seriesByKey[key] = id

	// Index by __name__ and all labels
	h.index.Add(id, "__name__", name)
	for k, v := range cp {
		h.index.Add(id, k, v)
	}

	return s, true
}

// getOrCreateSeriesWithID recreates a series under an explicit ID during WAL replay.
// Sample frames reference their series by the ID they were logged with, so replay
// must restore that exact ID rather than minting a fresh one. nextID is kept ahead
// of every restored ID so post-replay creations never collide.
func (h *HeadBlock) getOrCreateSeriesWithID(id uint64, name string, labels map[string]string) *MemSeries {
	key := seriesKey(name, labels)

	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.seriesByKey[key]; ok {
		return h.series[existing]
	}

	cp := copyLabels(labels)
	s := &MemSeries{ID: id, Name: name, Labels: cp}
	h.series[id] = s
	h.seriesByKey[key] = id

	h.index.Add(id, "__name__", name)
	for k, v := range cp {
		h.index.Add(id, k, v)
	}

	for {
		cur := h.nextID.Load()
		if cur >= id {
			break
		}
		if h.nextID.CompareAndSwap(cur, id) {
			break
		}
	}

	return s
}

// Ingest appends a single sample to the head block in timestamp order, enforcing a
// monotonic-per-series policy: a sample older than the series' last is rejected, an
// exact duplicate of the last sample is deduplicated, and a conflicting value at the
// last timestamp is rejected. The returned status lets callers account for drops.
// Enforcing order here is what keeps each series' timestamps sorted, so block/head
// time bounds and range checks stay correct.
func (h *HeadBlock) Ingest(seriesID uint64, ts int64, val float64) ingestResult {
	h.mu.RLock()
	s, ok := h.series[seriesID]
	h.mu.RUnlock()
	if !ok {
		return ingestOutOfOrder
	}

	s.mu.Lock()
	if n := len(s.Timestamps); n > 0 {
		last := s.Timestamps[n-1]
		switch {
		case ts < last:
			s.mu.Unlock()
			return ingestOutOfOrder
		case ts == last:
			if s.Values[n-1] == val {
				s.mu.Unlock()
				return ingestDuplicate
			}
			s.mu.Unlock()
			return ingestOutOfOrder
		}
	}
	s.Timestamps = append(s.Timestamps, ts)
	s.Values = append(s.Values, val)
	s.mu.Unlock()

	// Update bounds before publishing the count so that an observer seeing
	// SampleCount()>0 always sees consistent min/max.
	h.updateBounds(ts)
	h.numSamples.Add(1)
	return ingestAccepted
}

// Backfill inserts a sample into the head out of order, tolerating a timestamp older
// than the series' last — the catch-up path hinted-handoff replay applies through
// (ADR-029). Where Ingest enforces the live in-order policy of ADR-015 (an older sample
// is rejected and counted), Backfill binary-searches the insertion point so the series'
// timestamps stay sorted, and fills only gaps: a timestamp already present is left
// untouched (idempotent convergence), never overwritten. This is exactly the interior
// gap read-repair cannot fill — read-repair writes through the in-order Ingest path, so
// a point older than a replica's last is rejected there. Returns ingestAccepted on
// insert, ingestDuplicate when the timestamp was already present.
func (h *HeadBlock) Backfill(seriesID uint64, ts int64, val float64) ingestResult {
	h.mu.RLock()
	s, ok := h.series[seriesID]
	h.mu.RUnlock()
	if !ok {
		return ingestOutOfOrder // unknown series id; the caller creates the series first
	}

	s.mu.Lock()
	n := len(s.Timestamps)
	// Fast path: at or newer than the last sample appends, exactly like Ingest.
	if n == 0 || ts > s.Timestamps[n-1] {
		s.Timestamps = append(s.Timestamps, ts)
		s.Values = append(s.Values, val)
		s.mu.Unlock()
		h.updateBounds(ts)
		h.numSamples.Add(1)
		return ingestAccepted
	}
	// Interior (or leading) position: find where ts belongs.
	i := sort.Search(n, func(i int) bool { return s.Timestamps[i] >= ts })
	if i < n && s.Timestamps[i] == ts {
		s.mu.Unlock()
		return ingestDuplicate // gap-fill only: never overwrite an existing point
	}
	// Insert at i, keeping the timestamp and value slices sorted and aligned.
	s.Timestamps = append(s.Timestamps, 0)
	copy(s.Timestamps[i+1:], s.Timestamps[i:])
	s.Timestamps[i] = ts
	s.Values = append(s.Values, 0)
	copy(s.Values[i+1:], s.Values[i:])
	s.Values[i] = val
	s.mu.Unlock()

	h.updateBounds(ts)
	h.numSamples.Add(1)
	return ingestAccepted
}

// updateBounds widens the head's [min,max] timestamp range to include ts.
func (h *HeadBlock) updateBounds(ts int64) {
	for {
		cur := h.minTime.Load()
		if ts >= cur {
			break
		}
		if h.minTime.CompareAndSwap(cur, ts) {
			break
		}
	}
	for {
		cur := h.maxTime.Load()
		if ts <= cur {
			break
		}
		if h.maxTime.CompareAndSwap(cur, ts) {
			break
		}
	}
}

// Query returns all series matching the given label matchers within the time range.
func (h *HeadBlock) Query(matchers []LabelMatcher, minTime, maxTime int64) []*MemSeries {
	ids := h.index.Resolve(matchers)
	if len(ids) == 0 {
		return nil
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	var result []*MemSeries
	for _, id := range ids {
		s, ok := h.series[id]
		if !ok {
			continue
		}
		// Check if the series has data in the requested time range
		s.mu.Lock()
		if len(s.Timestamps) > 0 && s.Timestamps[0] <= maxTime && s.Timestamps[len(s.Timestamps)-1] >= minTime {
			result = append(result, s)
		}
		s.mu.Unlock()
	}
	return result
}

// AllSeries returns all series in the head block.
func (h *HeadBlock) AllSeries() []*MemSeries {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make([]*MemSeries, 0, len(h.series))
	for _, s := range h.series {
		result = append(result, s)
	}
	return result
}

// SeriesCount returns the number of active series.
func (h *HeadBlock) SeriesCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.series)
}

// SeriesKeys returns the canonical "name{sorted_labels}" key for every series in the
// head. The format matches Block.SeriesKeys so distinct series can be counted across
// the head and all blocks.
func (h *HeadBlock) SeriesKeys() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	keys := make([]string, 0, len(h.seriesByKey))
	for k := range h.seriesByKey {
		keys = append(keys, k)
	}
	return keys
}

// SampleCount returns the total number of samples in the head.
func (h *HeadBlock) SampleCount() int64 {
	return h.numSamples.Load()
}

// MinTime returns the earliest timestamp in the head.
func (h *HeadBlock) MinTime() int64 {
	return h.minTime.Load()
}

// MaxTime returns the latest timestamp in the head.
func (h *HeadBlock) MaxTime() int64 {
	return h.maxTime.Load()
}

// Reset clears the head block for reuse.
func (h *HeadBlock) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.series = make(map[uint64]*MemSeries)
	h.seriesByKey = make(map[string]uint64)
	h.index = NewInvertedIndex()
	h.minTime.Store(math.MaxInt64)
	h.maxTime.Store(math.MinInt64)
	h.numSamples.Store(0)
}

// CompressedSize returns what the current head would occupy on disk if Gorilla-encoded
// right now. It re-runs the encoder over every series, so callers should avoid hot loops.
func (h *HeadBlock) CompressedSize() int64 {
	series := h.AllSeries()
	var total int64
	for _, s := range series {
		s.mu.Lock()
		n := len(s.Timestamps)
		if n == 0 {
			s.mu.Unlock()
			continue
		}
		enc := compress.NewEncoder()
		for i := 0; i < n; i++ {
			enc.Write(s.Timestamps[i], s.Values[i])
		}
		total += int64(len(enc.Bytes()))
		s.mu.Unlock()
	}
	return total
}

// SeriesInfo contains metadata about a series.
type SeriesInfo struct {
	ID          uint64
	Name        string
	Labels      map[string]string
	SampleCount int
	LastValue   float64
	// LastTS is the timestamp (Unix ms) of the most recent sample, or 0 if the
	// series has none. It lets a 1 Hz consumer (e.g. the broadcast/anomaly path)
	// tell a fresh sample from a re-read of the same point.
	LastTS int64
}

// SeriesInfos returns metadata for all series.
func (h *HeadBlock) SeriesInfos() []SeriesInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()

	infos := make([]SeriesInfo, 0, len(h.series))
	for _, s := range h.series {
		s.mu.Lock()
		var lastVal float64
		var lastTS int64
		if len(s.Values) > 0 {
			lastVal = s.Values[len(s.Values)-1]
		}
		if len(s.Timestamps) > 0 {
			lastTS = s.Timestamps[len(s.Timestamps)-1]
		}
		infos = append(infos, SeriesInfo{
			ID:          s.ID,
			Name:        s.Name,
			Labels:      s.Labels,
			SampleCount: len(s.Timestamps),
			LastValue:   lastVal,
			LastTS:      lastTS,
		})
		s.mu.Unlock()
	}
	return infos
}

func seriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	key := name + "{"
	for i, k := range keys {
		if i > 0 {
			key += ","
		}
		key += k + "=" + labels[k]
	}
	key += "}"
	return key
}

// NewInvertedIndex creates a new empty inverted index.
func NewInvertedIndex() *InvertedIndex {
	return &InvertedIndex{
		postings: make(map[string]map[string][]uint64),
	}
}

// Add indexes a series ID under the given label name and value.
func (idx *InvertedIndex) Add(seriesID uint64, labelName, labelValue string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if idx.postings[labelName] == nil {
		idx.postings[labelName] = make(map[string][]uint64)
	}
	ids := idx.postings[labelName][labelValue]

	// Insert in sorted order (binary search)
	pos := sort.Search(len(ids), func(i int) bool { return ids[i] >= seriesID })
	if pos < len(ids) && ids[pos] == seriesID {
		return // already indexed
	}
	ids = append(ids, 0)
	copy(ids[pos+1:], ids[pos:])
	ids[pos] = seriesID
	idx.postings[labelName][labelValue] = ids
}

// Resolve finds series IDs that match all the given matchers (AND semantics).
func (idx *InvertedIndex) Resolve(matchers []LabelMatcher) []uint64 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if len(matchers) == 0 {
		// Return all series IDs
		return idx.allIDs()
	}

	var result []uint64
	first := true

	for _, m := range matchers {
		var ids []uint64
		switch m.Type {
		case MatchEqual:
			if vals, ok := idx.postings[m.Name]; ok {
				if posting, ok := vals[m.Value]; ok {
					ids = posting
				}
			}
		case MatchNotEqual:
			ids = idx.matchNotEqual(m.Name, m.Value)
		case MatchRegexp:
			ids = idx.matchRegexp(m.Name, m.Value)
		case MatchNotRegexp:
			ids = idx.matchNotRegexp(m.Name, m.Value)
		}

		if first {
			result = make([]uint64, len(ids))
			copy(result, ids)
			first = false
		} else {
			result = intersectSorted(result, ids)
		}

		if len(result) == 0 {
			return nil
		}
	}

	return result
}

// LabelNames returns all known label names.
func (idx *InvertedIndex) LabelNames() []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	names := make([]string, 0, len(idx.postings))
	for name := range idx.postings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LabelValues returns all known values for a label name.
func (idx *InvertedIndex) LabelValues(name string) []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	vals, ok := idx.postings[name]
	if !ok {
		return nil
	}
	result := make([]string, 0, len(vals))
	for v := range vals {
		result = append(result, v)
	}
	sort.Strings(result)
	return result
}

func (idx *InvertedIndex) allIDs() []uint64 {
	seen := make(map[uint64]bool)
	for _, vals := range idx.postings {
		for _, ids := range vals {
			for _, id := range ids {
				seen[id] = true
			}
		}
	}
	result := make([]uint64, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func (idx *InvertedIndex) matchNotEqual(name, value string) []uint64 {
	vals, ok := idx.postings[name]
	if !ok {
		return idx.allIDs()
	}
	var result []uint64
	for v, ids := range vals {
		if v != value {
			result = mergeSorted(result, ids)
		}
	}
	return result
}

func (idx *InvertedIndex) matchRegexp(name, pattern string) []uint64 {
	vals, ok := idx.postings[name]
	if !ok {
		return nil
	}

	re, err := compileAnchored(pattern)
	if err != nil {
		return nil
	}

	var result []uint64
	for v, ids := range vals {
		if re.MatchString(v) {
			result = mergeSorted(result, ids)
		}
	}
	return result
}

func (idx *InvertedIndex) matchNotRegexp(name, pattern string) []uint64 {
	vals, ok := idx.postings[name]
	if !ok {
		return idx.allIDs()
	}

	re, err := compileAnchored(pattern)
	if err != nil {
		return idx.allIDs()
	}

	var result []uint64
	for v, ids := range vals {
		if !re.MatchString(v) {
			result = mergeSorted(result, ids)
		}
	}
	return result
}

func intersectSorted(a, b []uint64) []uint64 {
	result := make([]uint64, 0, min(len(a), len(b)))
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

func mergeSorted(a, b []uint64) []uint64 {
	if len(a) == 0 {
		return b
	}
	result := make([]uint64, 0, len(a)+len(b))
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// compileAnchored wraps the pattern with ^ and $ anchors for full-string matching.
func compileAnchored(pattern string) (regexpCompiled, error) {
	return compileRegexp(fmt.Sprintf("^(?:%s)$", pattern))
}
