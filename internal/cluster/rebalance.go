package cluster

import (
	"sort"
	"strings"
)

// departing reports whether a node in this state is leaving the cluster for good — Dead or
// Leaving — and so must not be counted among an arc's target owners when planning a
// rebalance (ADR-031). It is deliberately distinct from excludedFromRouting, which also
// excludes a Joining node: a joining node IS a target owner (it is being brought in) even
// though it is kept out of live read/write routing until it has caught up. A leaving node is
// the mirror image — still serving until its data has moved, but never a target owner.
func (s NodeState) departing() bool { return s == NodeDead || s == NodeLeaving }

// OwnershipChange is a set of hash arcs whose target owner set changed between two ring
// states — the unit a rebalance acts on (ADR-031). Before is where the arcs' data currently
// lives (the migration source set); After is where it must end up (the target owners). Added
// (After \ Before) are owners that must receive the data; Removed (Before \ After) are owners
// that may drop it once the new owners confirm receipt at quorum. Arcs is the
// (generally non-contiguous) set of arcs sharing this exact transition, coalesced so the
// number of changes tracks the cluster's distinct transitions, not the virtual-node count.
type OwnershipChange struct {
	Before  []string  // owner IDs before the change (sorted) — the data's current holders
	After   []string  // owner IDs after the change (sorted) — the target owners
	Added   []string  // After \ Before — must receive the arcs' data (sorted)
	Removed []string  // Before \ After — may GC the arcs' data once handed off (sorted)
	Arcs    []HashArc // the hash arcs this transition covers
}

// PlacementDelta computes how arc ownership differs between two ring states for a given
// replication factor — the heart of rebalance planning (ADR-031). An arc's owners are the
// first `replication` distinct non-departing (Active or Joining) nodes clockwise from it: the
// same walk PreferenceList/ReplicaGroups do, but excluding nodes on their way out (Dead,
// Leaving) so a departing node is never a target owner while it still appears as a source.
//
// The two rings are compared at the finest common granularity — the union of their virtual-
// node boundaries — so an arc one ring splits and the other does not is still classified
// consistently: no boundary falls strictly inside a compared arc, so every key in it shares a
// single owner set per ring. Arcs whose (before, after) owner sets are equal are dropped (no
// change); the rest are grouped by their exact transition signature and returned in a
// deterministic order. The caller decides which transitions to act on — a join or leave
// migrates then GCs, while a node merely returning from Dead only GCs the over-replication
// its absence created.
//
// before and after must be distinct ring snapshots (e.g. via Clone); passing the same ring
// returns no changes.
func PlacementDelta(before, after *Ring, replication int) []OwnershipChange {
	if replication <= 0 || before == after {
		return nil
	}
	before.mu.RLock()
	defer before.mu.RUnlock()
	after.mu.RLock()
	defer after.mu.RUnlock()

	// Union of every virtual-node boundary hash from both rings, deduplicated and sorted —
	// the finest granularity at which both rings classify a key consistently.
	boundSet := make(map[uint64]struct{}, len(before.ring)+len(after.ring))
	for _, e := range before.ring {
		boundSet[e.hash] = struct{}{}
	}
	for _, e := range after.ring {
		boundSet[e.hash] = struct{}{}
	}
	if len(boundSet) == 0 {
		return nil
	}
	bounds := make([]uint64, 0, len(boundSet))
	for h := range boundSet {
		bounds = append(bounds, h)
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })

	groups := make(map[string]*OwnershipChange)
	var order []string

	n := len(bounds)
	for i := 0; i < n; i++ {
		start := bounds[(i-1+n)%n]
		end := bounds[i]
		// A zero-width arc (the only-one-distinct-boundary degenerate case) owns nothing.
		if start == end && n > 1 {
			continue
		}
		bOwners := before.ownersAtLocked(end, replication)
		aOwners := after.ownersAtLocked(end, replication)
		if equalStringSets(bOwners, aOwners) {
			continue
		}
		sb := append([]string(nil), bOwners...)
		sa := append([]string(nil), aOwners...)
		sort.Strings(sb)
		sort.Strings(sa)
		key := strings.Join(sb, "\x00") + "\x01" + strings.Join(sa, "\x00")
		g, ok := groups[key]
		if !ok {
			g = &OwnershipChange{
				Before:  sb,
				After:   sa,
				Added:   setDiff(sa, sb),
				Removed: setDiff(sb, sa),
			}
			groups[key] = g
			order = append(order, key)
		}
		g.Arcs = append(g.Arcs, HashArc{Start: start, End: end})
	}

	sort.Strings(order)
	out := make([]OwnershipChange, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	return out
}

// ownersAtLocked returns up to `replication` distinct non-departing node IDs responsible for
// ring position h, walking clockwise from the first virtual node at or after h (wrapping at
// the ring's end). Caller holds r.mu. The result preserves clockwise order and may be shorter
// than replication when fewer eligible nodes exist.
func (r *Ring) ownersAtLocked(h uint64, replication int) []string {
	if len(r.ring) == 0 {
		return nil
	}
	idx := sort.Search(len(r.ring), func(i int) bool { return r.ring[i].hash >= h })
	if idx >= len(r.ring) {
		idx = 0
	}
	seen := make(map[string]bool)
	out := make([]string, 0, replication)
	for i := 0; i < len(r.ring) && len(out) < replication; i++ {
		e := r.ring[(idx+i)%len(r.ring)]
		if seen[e.nodeID] {
			continue
		}
		seen[e.nodeID] = true
		node, ok := r.nodes[e.nodeID]
		if !ok || node.State.departing() {
			continue
		}
		out = append(out, e.nodeID)
	}
	return out
}

// Clone returns an independent deep copy of the ring — the same virtual-node layout and the
// same node records (state included) — so a rebalance can snapshot the ring before a
// membership change and diff it against the ring afterwards without holding a lock across the
// change. See ADR-031.
func (r *Ring) Clone() *Ring {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cp := &Ring{
		virtualNodes: r.virtualNodes,
		nodes:        make(map[string]Node, len(r.nodes)),
		ring:         make([]ringEntry, len(r.ring)),
	}
	for id, n := range r.nodes {
		cp.nodes[id] = n
	}
	copy(cp.ring, r.ring)
	return cp
}

// equalStringSets reports whether a and b contain the same elements, ignoring order and
// assuming each is already free of duplicates (as owner walks are).
func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, x := range a {
		set[x] = struct{}{}
	}
	for _, x := range b {
		if _, ok := set[x]; !ok {
			return false
		}
	}
	return true
}

// setDiff returns the elements of a absent from b (both treated as sets), preserving a's
// order. The result is nil when every element of a is in b.
func setDiff(a, b []string) []string {
	if len(a) == 0 {
		return nil
	}
	bset := make(map[string]struct{}, len(b))
	for _, x := range b {
		bset[x] = struct{}{}
	}
	var out []string
	for _, x := range a {
		if _, ok := bset[x]; !ok {
			out = append(out, x)
		}
	}
	return out
}
