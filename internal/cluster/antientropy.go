package cluster

import (
	"sort"
	"strings"
)

// HashKey maps a key onto the 64-bit ring exactly as the ring's internal placement
// does. Anti-entropy (ADR-030) needs to decide, on a storage node that is itself
// ring-agnostic, which series fall inside a given hash arc; exporting the ring hash
// keeps that classification identical to how writes were routed in the first place
// (the same SHA-256 prefix of MetricKey(name, labels)). Diverging from it would let a
// series be reconciled under an arc no replica actually owns.
func HashKey(key string) uint64 { return hashKey(key) }

// HashArc is a half-open hash range (Start, End] on the ring's 64-bit keyspace: the
// set of positions a single ring segment owns. A key with HashKey(key) in the arc maps
// to the segment's end boundary. Start < End is a normal arc; Start > End wraps the
// 2^64 boundary (the segment straddling the origin); Start == End is the degenerate
// whole-ring arc of a one-position ring.
type HashArc struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
}

// Contains reports whether a ring position falls in the arc, honouring the wrap at the
// 2^64 boundary.
func (a HashArc) Contains(h uint64) bool {
	switch {
	case a.Start < a.End:
		return h > a.Start && h <= a.End
	case a.Start > a.End:
		return h > a.Start || h <= a.End
	default:
		// One distinct boundary: the segment owns the whole ring.
		return true
	}
}

// ReplicaGroup is a set of co-located replicas and the hash arcs they jointly own.
// Replicas is the natural preferred-owner set (sorted node IDs, all states) shared by
// every arc in Arcs — the unit anti-entropy reconciles: nodes in the same group should
// hold byte-identical data for those arcs, so a divergence between them is a missed
// write to repair. Arcs is non-contiguous in general (a replica set recurs around the
// ring), which is why it is a list.
type ReplicaGroup struct {
	Replicas []string
	Arcs     []HashArc
}

// ReplicaGroups partitions the ring into groups of arcs that share the same replica
// set — the first `replication` distinct nodes clockwise from each arc, the same walk
// PreferenceList does for a single key, but computed once per arc and ignoring node
// state so the grouping is stable across transient up/down churn. Arcs whose replica
// set is identical are coalesced into one group, so the number of groups is governed by
// the cluster's distinct replica sets (O(nodes) for a small RF) rather than by the
// virtual-node count. Anti-entropy sweeps these groups, comparing each group's replicas
// against one another (ADR-030).
//
// The returned slice is deterministic (groups ordered by their sorted replica set), so
// a round-robin sweep over it visits every arc set in a stable rotation.
func (r *Ring) ReplicaGroups(replication int) []ReplicaGroup {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n := len(r.ring)
	if n == 0 || replication <= 0 {
		return nil
	}

	groups := make(map[string]*ReplicaGroup)
	for i := 0; i < n; i++ {
		start := r.ring[(i-1+n)%n].hash
		end := r.ring[i].hash
		// A zero-width arc (two virtual nodes colliding on the same 64-bit hash) owns no
		// positions, so it contributes nothing — skip it, unless the whole ring is a
		// single position (n == 1), where (start == end) is the degenerate full arc.
		if start == end && n > 1 {
			continue
		}

		// Natural owners: walk clockwise from i collecting distinct node IDs, capped at
		// the replication factor (or the node count, whichever is smaller).
		seen := make(map[string]bool)
		reps := make([]string, 0, replication)
		for j := 0; j < n && len(reps) < replication; j++ {
			id := r.ring[(i+j)%n].nodeID
			if seen[id] {
				continue
			}
			seen[id] = true
			reps = append(reps, id)
		}

		sortedReps := append([]string(nil), reps...)
		sort.Strings(sortedReps)
		key := strings.Join(sortedReps, "\x00")

		g, ok := groups[key]
		if !ok {
			g = &ReplicaGroup{Replicas: sortedReps}
			groups[key] = g
		}
		g.Arcs = append(g.Arcs, HashArc{Start: start, End: end})
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]ReplicaGroup, 0, len(keys))
	for _, k := range keys {
		out = append(out, *groups[k])
	}
	return out
}
