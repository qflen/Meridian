package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Hint is a write buffered for a replica that was unreachable when the write reached
// quorum on its live replicas. Replaying it on the replica's return fills the gap the
// replica missed — including an interior gap read-repair cannot fix, because replay
// applies through the out-of-order-tolerant backfill path. See ADR-029.
type Hint struct {
	Target string       // ring node (storage address) the buffered write is destined for
	Series []TimeSeries // the series + samples that node missed
}

// hintPayload is the on-disk form of one hint: a single JSON record per file.
type hintPayload struct {
	Target string       `json:"target"`
	Series []TimeSeries `json:"series"`
}

// hintFile is the in-memory index entry for one persisted hint.
type hintFile struct {
	seq     uint64
	path    string
	samples int
}

// HintStore is a durable, bounded, per-target buffer of writes destined for unreachable
// replicas (hinted handoff, ADR-029). Each hint is one file under dir, named by a global
// monotonic sequence so replay is FIFO per target; a hint is deleted only after the
// target acknowledges its backfill. Each target is bounded to maxSamples buffered
// samples — drop-oldest past the cap (counted) — so a long outage cannot grow the buffer
// without bound, while the most recent hint is always retained. All methods are safe for
// concurrent use.
type HintStore struct {
	dir        string
	maxSamples int

	mu       sync.Mutex
	seq      uint64
	byTarget map[string][]hintFile

	pending  atomic.Int64 // buffered samples across all targets (gauge)
	records  atomic.Int64 // buffered hint records across all targets (gauge)
	dropped  atomic.Int64 // samples dropped because a target hit its cap (counter)
	replayed atomic.Int64 // samples replayed and acknowledged since startup (counter)
}

// NewHintStore opens (creating if needed) a hint store rooted at dir and rebuilds its
// in-memory index from any hints left by a previous run, so a process restart resumes
// replaying them. Leftover temp files from an interrupted write are removed. maxSamples
// is the per-target buffered-sample cap.
func NewHintStore(dir string, maxSamplesPerNode int) (*HintStore, error) {
	if maxSamplesPerNode < 1 {
		maxSamplesPerNode = 1
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create hint dir: %w", err)
	}
	s := &HintStore{
		dir:        dir,
		maxSamples: maxSamplesPerNode,
		byTarget:   make(map[string][]hintFile),
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("scan hint dir: %w", err)
	}
	type loaded struct {
		f      hintFile
		target string
	}
	var all []loaded
	var maxSeq uint64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(name, ".tmp") {
			os.Remove(filepath.Join(dir, name)) // interrupted write; never committed
			continue
		}
		if !strings.HasSuffix(name, ".hint") {
			continue
		}
		var seq uint64
		if _, err := fmt.Sscanf(name, "%020d.hint", &seq); err != nil {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var p hintPayload
		if err := json.Unmarshal(data, &p); err != nil {
			os.Remove(path) // corrupt hint: drop it so it never wedges replay
			continue
		}
		all = append(all, loaded{f: hintFile{seq: seq, path: path, samples: countSamples(p.Series)}, target: p.Target})
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].f.seq < all[j].f.seq })
	for _, l := range all {
		s.byTarget[l.target] = append(s.byTarget[l.target], l.f)
		s.pending.Add(int64(l.f.samples))
		s.records.Add(1)
	}
	s.seq = maxSeq

	// Re-enforce caps after load in case maxSamples shrank since the hints were written.
	s.mu.Lock()
	for t := range s.byTarget {
		s.enforceCapLocked(t)
	}
	s.mu.Unlock()
	return s, nil
}

// Add durably buffers a hint of series destined for target and enforces the per-target
// cap. The series are deep-copied so a caller reusing its slices cannot mutate a
// buffered hint.
func (s *HintStore) Add(target string, series []TimeSeries) error {
	n := countSamples(series)
	cp := cloneSeries(series)
	data, err := json.Marshal(hintPayload{Target: target, Series: cp})
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	seq := s.seq
	path := filepath.Join(s.dir, fmt.Sprintf("%020d.hint", seq))
	if err := writeFileAtomic(s.dir, path, data); err != nil {
		s.seq-- // nothing persisted; reuse the sequence number
		return err
	}
	s.byTarget[target] = append(s.byTarget[target], hintFile{seq: seq, path: path, samples: n})
	s.pending.Add(int64(n))
	s.records.Add(1)
	s.enforceCapLocked(target)
	return nil
}

// enforceCapLocked drops the oldest hints for target until its buffered samples are at
// or below the cap, always retaining at least the most recent hint. Caller holds s.mu.
func (s *HintStore) enforceCapLocked(target string) {
	files := s.byTarget[target]
	sum := 0
	for _, f := range files {
		sum += f.samples
	}
	drop := 0
	for sum > s.maxSamples && len(files)-drop > 1 {
		f := files[drop]
		os.Remove(f.path)
		sum -= f.samples
		s.pending.Add(int64(-f.samples))
		s.records.Add(-1)
		s.dropped.Add(int64(f.samples))
		drop++
	}
	if drop > 0 {
		s.byTarget[target] = append([]hintFile(nil), files[drop:]...)
	}
}

// Targets returns the targets that currently have at least one buffered hint.
func (s *HintStore) Targets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.byTarget))
	for t := range s.byTarget {
		out = append(out, t)
	}
	return out
}

// Pending returns the number of samples currently buffered for target (0 once it has
// fully caught up). The lifecycle uses a zero here as the "no backlog" signal.
func (s *HintStore) Pending(target string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := 0
	for _, f := range s.byTarget[target] {
		sum += f.samples
	}
	return sum
}

// Drain replays target's buffered hints in FIFO order through send, deleting each only
// after send reports success (the target acknowledged the backfill). It stops at the
// first failure so order is preserved and the rest retry on the next pass. It returns
// how many records were replayed and whether the snapshot fully drained without a send
// failure (the signal to promote a catching-up node to Active). send is called without
// the store lock held, so it may do network I/O.
func (s *HintStore) Drain(target string, send func(Hint) bool) (int, bool) {
	s.mu.Lock()
	snapshot := append([]hintFile(nil), s.byTarget[target]...)
	s.mu.Unlock()

	replayed := 0
	for _, f := range snapshot {
		data, err := os.ReadFile(f.path)
		if err != nil {
			// Dropped over cap (or already replayed) since the snapshot — it's gone.
			continue
		}
		var p hintPayload
		if err := json.Unmarshal(data, &p); err != nil {
			s.remove(target, f, false) // corrupt: drop so it never wedges the queue
			continue
		}
		if !send(Hint{Target: p.Target, Series: p.Series}) {
			return replayed, false // stop on first failure; preserve order, retry next pass
		}
		s.remove(target, f, true)
		replayed++
	}
	return replayed, true
}

// remove deletes a hint from the index and disk. When replayed is true its samples are
// added to the replayed counter; either way they leave the pending gauge.
func (s *HintStore) remove(target string, f hintFile, replayed bool) {
	s.mu.Lock()
	files := s.byTarget[target]
	for i, cur := range files {
		if cur.seq != f.seq {
			continue
		}
		s.byTarget[target] = append(files[:i], files[i+1:]...)
		if len(s.byTarget[target]) == 0 {
			delete(s.byTarget, target)
		}
		s.pending.Add(int64(-cur.samples))
		s.records.Add(-1)
		if replayed {
			s.replayed.Add(int64(cur.samples))
		}
		break
	}
	s.mu.Unlock()
	os.Remove(f.path)
}

// PendingSamples returns the total buffered samples across all targets (a gauge).
func (s *HintStore) PendingSamples() int64 { return s.pending.Load() }

// PendingRecords returns the total buffered hint records across all targets (a gauge).
func (s *HintStore) PendingRecords() int64 { return s.records.Load() }

// Dropped returns the cumulative samples dropped because a target hit its cap (counter).
func (s *HintStore) Dropped() int64 { return s.dropped.Load() }

// Replayed returns the cumulative samples replayed and acknowledged (counter).
func (s *HintStore) Replayed() int64 { return s.replayed.Load() }

func countSamples(series []TimeSeries) int {
	n := 0
	for _, ts := range series {
		n += len(ts.Samples)
	}
	if n < 1 {
		n = 1 // an empty batch still occupies a slot, mirroring the queue cost model
	}
	return n
}

// cloneSeries deep-copies a series slice (labels and samples) so a buffered hint is
// independent of the caller's backing arrays.
func cloneSeries(series []TimeSeries) []TimeSeries {
	out := make([]TimeSeries, len(series))
	for i, ts := range series {
		c := TimeSeries{Name: ts.Name}
		if len(ts.Labels) > 0 {
			c.Labels = append([]Label(nil), ts.Labels...)
		}
		if len(ts.Samples) > 0 {
			c.Samples = append([]Sample(nil), ts.Samples...)
		}
		out[i] = c
	}
	return out
}

// writeFileAtomic writes data to a temp file in dir, fsyncs it, and atomically renames
// it into place, so a crash never leaves a partially written hint — a reader sees the
// whole hint or none of it. The directory is fsynced best-effort so the rename is durable.
func writeFileAtomic(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "hint-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
