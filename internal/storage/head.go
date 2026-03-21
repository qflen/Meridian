package storage

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
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
	h.minTime.Store(0)
	h.maxTime.Store(0)
	return h
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
	s := &MemSeries{
		ID:     id,
		Name:   name,
		Labels: labels,
	}
	h.series[id] = s
	h.seriesByKey[key] = id

	// Index by __name__ and all labels
	h.index.Add(id, "__name__", name)
	for k, v := range labels {
		h.index.Add(id, k, v)
	}

	return s, true
}

// Ingest adds a single sample to the head block.
func (h *HeadBlock) Ingest(seriesID uint64, ts int64, val float64) {
	h.mu.RLock()
	s, ok := h.series[seriesID]
	h.mu.RUnlock()
	if !ok {
		return
	}

	s.mu.Lock()
	s.Timestamps = append(s.Timestamps, ts)
	s.Values = append(s.Values, val)
	s.mu.Unlock()

	h.numSamples.Add(1)

	// Update min/max time atomically
	for {
		cur := h.minTime.Load()
		if cur != 0 && cur <= ts {
			break
		}
		if h.minTime.CompareAndSwap(cur, ts) {
			break
		}
	}
	for {
		cur := h.maxTime.Load()
		if cur >= ts {
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
	h.minTime.Store(0)
	h.maxTime.Store(0)
	h.numSamples.Store(0)
}

// SeriesInfo contains metadata about a series.
type SeriesInfo struct {
	ID          uint64
	Name        string
	Labels      map[string]string
	SampleCount int
}

// SeriesInfos returns metadata for all series.
func (h *HeadBlock) SeriesInfos() []SeriesInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()

	infos := make([]SeriesInfo, 0, len(h.series))
	for _, s := range h.series {
		s.mu.Lock()
		infos = append(infos, SeriesInfo{
			ID:          s.ID,
			Name:        s.Name,
			Labels:      s.Labels,
			SampleCount: len(s.Timestamps),
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
