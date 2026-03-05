package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
)

// HashRange is a half-open hash range (Lo, Hi] on the ring's 64-bit keyspace. It is the
// storage layer's mirror of cluster.HashArc, kept here so the TSDB can classify its own
// series by ring position without importing the cluster package — the ring hash itself
// is injected (see RangeDigest's hashOf parameter). Lo > Hi wraps the 2^64 boundary;
// Lo == Hi covers the whole ring (a one-position ring).
type HashRange struct {
	Lo uint64
	Hi uint64
}

// Contains reports whether a ring position falls in the range, honouring the wrap at
// the 2^64 boundary.
func (r HashRange) Contains(h uint64) bool {
	switch {
	case r.Lo < r.Hi:
		return h > r.Lo && h <= r.Hi
	case r.Lo > r.Hi:
		return h > r.Lo || h <= r.Hi
	default:
		return true
	}
}

func inAnyRange(h uint64, ranges []HashRange) bool {
	for _, r := range ranges {
		if r.Contains(h) {
			return true
		}
	}
	return false
}

// WindowDigest is one leaf of a range digest: a fixed-width time window and a content
// hash over every (in-range series, sample) the node holds in that window. Two replicas
// that hold byte-identical data for the window produce the same Hash; any difference —
// an extra sample, a changed value, a missing series — changes it. Count is the total
// samples folded in, a cheap signal of which side is behind.
type WindowDigest struct {
	Start int64  `json:"start"`
	Hash  string `json:"hash"`
	Count int64  `json:"count"`
}

// MerkleDigest is a compact summary of a node's data over a set of hash ranges and a
// time span, bucketed into fixed windows. Root is the Merkle root over the per-window
// leaves: equal roots mean the two sides already agree over the whole span (the common
// case — anti-entropy stops there and transfers nothing), and only when roots differ do
// the leaves localise the divergence to specific windows. See ADR-030.
type MerkleDigest struct {
	Root   string         `json:"root"`
	Window int64          `json:"window"`
	Leaves []WindowDigest `json:"leaves"`
}

// floorWindow aligns a timestamp to the start of its window using floor division, so
// the bucketing is identical on every replica regardless of the requested span and
// correct for the negative timestamps the TSDB also accepts (Go's integer division
// truncates toward zero, which would misalign negatives).
func floorWindow(ts, window int64) int64 {
	q := ts / window
	if ts%window != 0 && (ts < 0) != (window < 0) {
		q--
	}
	return q * window
}

// RangeDigest builds a Merkle digest over the series this node holds whose ring
// position (hashOf of the series' canonical key) falls in any of the given ranges, for
// samples in [start, end], bucketed into windows of `window` ms. hashOf is injected by
// the caller (the storage node passes cluster.HashKey) so the classification matches
// how writes were routed, without the storage layer depending on the ring. The cost is
// bounded by the data in the requested ranges and span — the caller (anti-entropy)
// rate-limits how much it asks for per round. See ADR-030.
func (db *TSDB) RangeDigest(ranges []HashRange, start, end, window int64, hashOf func(string) uint64) (MerkleDigest, error) {
	if window <= 0 {
		return MerkleDigest{}, fmt.Errorf("range digest: window must be > 0, got %d", window)
	}

	series, err := db.collectInRange(ranges, start, end, hashOf)
	if err != nil {
		return MerkleDigest{}, err
	}

	// Per window: the set of per-series content hashes and the total sample count.
	type bucket struct {
		hashes [][]byte
		count  int64
	}
	buckets := make(map[int64]*bucket)

	for _, rs := range series {
		key := resultSeriesKey(rs)
		pts := rs.Points // already sorted ascending by TSDB.Query
		for i := 0; i < len(pts); {
			ws := floorWindow(pts[i].Timestamp, window)
			// Hash this series' contiguous run of points within window ws.
			h := sha256.New()
			h.Write([]byte(key))
			h.Write([]byte{0})
			var n int64
			for i < len(pts) && floorWindow(pts[i].Timestamp, window) == ws {
				var buf [16]byte
				binary.BigEndian.PutUint64(buf[0:8], uint64(pts[i].Timestamp))
				binary.BigEndian.PutUint64(buf[8:16], math.Float64bits(pts[i].Value))
				h.Write(buf[:])
				n++
				i++
			}
			b := buckets[ws]
			if b == nil {
				b = &bucket{}
				buckets[ws] = b
			}
			b.hashes = append(b.hashes, h.Sum(nil))
			b.count += n
		}
	}

	leaves := make([]WindowDigest, 0, len(buckets))
	for ws, b := range buckets {
		// Order-independent over series: sort the per-series hashes before folding so
		// the leaf does not depend on query iteration order.
		sort.Slice(b.hashes, func(i, j int) bool { return bytes.Compare(b.hashes[i], b.hashes[j]) < 0 })
		content := sha256.New()
		for _, hsh := range b.hashes {
			content.Write(hsh)
		}
		// Bind the window start into the leaf hash so a window cannot be confused with a
		// neighbour that happens to hold identical values.
		leaf := sha256.New()
		var wb [8]byte
		binary.BigEndian.PutUint64(wb[:], uint64(ws))
		leaf.Write(wb[:])
		leaf.Write(content.Sum(nil))
		leaves = append(leaves, WindowDigest{Start: ws, Hash: hex.EncodeToString(leaf.Sum(nil)), Count: b.count})
	}
	sort.Slice(leaves, func(i, j int) bool { return leaves[i].Start < leaves[j].Start })

	return MerkleDigest{Root: merkleRoot(leaves), Window: window, Leaves: leaves}, nil
}

// RangeExport returns the raw samples this node holds whose ring position falls in any
// of the given ranges, for samples in [start, end]. It is the read half of an
// anti-entropy transfer: the coordinator reads a divergent window from each replica and
// pushes whatever a peer is missing back through the out-of-order-tolerant backfill
// path. Same classification as RangeDigest (hashOf injected). See ADR-030.
func (db *TSDB) RangeExport(ranges []HashRange, start, end int64, hashOf func(string) uint64) ([]ResultSeries, error) {
	return db.collectInRange(ranges, start, end, hashOf)
}

// collectInRange runs a match-all query over [start, end] and keeps only the series
// whose canonical key hashes into one of the ranges. The match-all query reuses the
// normal head+block merge, so an exported/digested series is exactly what a reader
// would see.
func (db *TSDB) collectInRange(ranges []HashRange, start, end int64, hashOf func(string) uint64) ([]ResultSeries, error) {
	ss, err := db.Query(context.Background(), nil, start, end)
	if err != nil {
		return nil, err
	}
	out := ss[:0]
	for _, rs := range ss {
		if inAnyRange(hashOf(resultSeriesKey(rs)), ranges) {
			out = append(out, rs)
		}
	}
	return out, nil
}

// resultSeriesKey rebuilds a series' canonical "name{sorted_labels}" key from a query
// result, dropping the synthetic __name__ label the query adds, so the key matches both
// the in-memory seriesKey and cluster.MetricKey (and therefore hashes to the same ring
// position writes were routed by).
func resultSeriesKey(rs ResultSeries) string {
	if len(rs.Labels) == 0 {
		return seriesKey(rs.Name, nil)
	}
	labels := make(map[string]string, len(rs.Labels))
	for k, v := range rs.Labels {
		if k != "__name__" {
			labels[k] = v
		}
	}
	return seriesKey(rs.Name, labels)
}

// merkleRoot folds the leaf hashes into a single Merkle root by repeated pairwise
// hashing (an odd node is promoted unchanged). An empty leaf set hashes to the SHA-256
// of nothing, a stable sentinel so two empty ranges compare equal.
func merkleRoot(leaves []WindowDigest) string {
	if len(leaves) == 0 {
		sum := sha256.Sum256(nil)
		return hex.EncodeToString(sum[:])
	}
	level := make([][]byte, len(leaves))
	for i, l := range leaves {
		b, err := hex.DecodeString(l.Hash)
		if err != nil {
			// Leaf hashes are produced here as hex, so this is unreachable; fall back to
			// hashing the raw string rather than panicking.
			sum := sha256.Sum256([]byte(l.Hash))
			b = sum[:]
		}
		level[i] = b
	}
	for len(level) > 1 {
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 < len(level) {
				h := sha256.New()
				h.Write(level[i])
				h.Write(level[i+1])
				next = append(next, h.Sum(nil))
			} else {
				next = append(next, level[i])
			}
		}
		level = next
	}
	return hex.EncodeToString(level[0])
}
