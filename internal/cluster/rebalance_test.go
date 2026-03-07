package cluster

import (
	"fmt"
	"testing"
)

// ringWith builds a ring of the given virtual-node count seeded with active nodes by ID.
func ringWith(vnodes int, ids ...string) *Ring {
	r := NewRing(vnodes)
	for _, id := range ids {
		r.AddNode(Node{ID: id, Addr: id, State: NodeActive})
	}
	return r
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// arcCount totals the arcs across a set of ownership changes.
func arcCount(changes []OwnershipChange) int {
	n := 0
	for _, c := range changes {
		n += len(c.Arcs)
	}
	return n
}

// TestPlacementDelta_NoChangeWhenIdentical proves a ring diffed against an exact clone
// reports no ownership movement — the idempotent steady state a reconcile must converge to.
func TestPlacementDelta_NoChangeWhenIdentical(t *testing.T) {
	r := ringWith(64, "a", "b", "c")
	if changes := PlacementDelta(r, r.Clone(), 3); len(changes) != 0 {
		t.Fatalf("identical rings must show no change, got %d change(s) over %d arc(s)", len(changes), arcCount(changes))
	}
}

// TestPlacementDelta_NodeJoinGainsArcs proves that adding a node makes it a target owner of
// some arcs (Added), displacing exactly one prior owner per affected arc (Removed), while the
// rest of the ring is untouched — the join half of rebalancing (ADR-031).
func TestPlacementDelta_NodeJoinGainsArcs(t *testing.T) {
	before := ringWith(64, "a", "b", "c")
	after := before.Clone()
	after.AddNode(Node{ID: "d", Addr: "d", State: NodeJoining}) // a joiner is a target owner

	changes := PlacementDelta(before, after, 3)
	if len(changes) == 0 {
		t.Fatal("adding a node must move some arcs")
	}

	for _, c := range changes {
		if !contains(c.After, "d") {
			t.Errorf("changed arc must list the new node d as an owner-after: %+v", c)
		}
		if contains(c.Before, "d") {
			t.Errorf("the new node cannot have been an owner-before: %+v", c)
		}
		if len(c.Added) != 1 || c.Added[0] != "d" {
			t.Errorf("only d should be added, got %v", c.Added)
		}
		if len(c.Removed) != 1 {
			t.Errorf("exactly one prior owner should be displaced, got %v", c.Removed)
		}
		if len(c.After) != 3 || len(c.Before) != 3 {
			t.Errorf("RF=3 owner sets expected, got before=%v after=%v", c.Before, c.After)
		}
	}

	// A join never moves the entire ring: only arcs near d's virtual nodes change.
	if got := arcCount(changes); got >= 64*4 {
		t.Errorf("a single join should move a fraction of arcs, moved %d", got)
	}
}

// TestPlacementDelta_NodeLeaveShedsArcs proves a node marked Leaving stops being a target
// owner (Removed) and the next clockwise survivor takes over (Added) — the leave half.
func TestPlacementDelta_NodeLeaveShedsArcs(t *testing.T) {
	before := ringWith(64, "a", "b", "c", "d")
	after := before.Clone()
	after.SetState("d", NodeLeaving)

	changes := PlacementDelta(before, after, 3)
	if len(changes) == 0 {
		t.Fatal("a leaving node must move its arcs to survivors")
	}
	for _, c := range changes {
		if !contains(c.Before, "d") {
			t.Errorf("changed arc must have listed the leaver d as an owner-before: %+v", c)
		}
		if contains(c.After, "d") {
			t.Errorf("the leaver must not be a target owner: %+v", c)
		}
		if len(c.Removed) != 1 || c.Removed[0] != "d" {
			t.Errorf("only d should be removed, got %v", c.Removed)
		}
		if len(c.Added) != 1 {
			t.Errorf("exactly one survivor should take over, got %v", c.Added)
		}
	}
}

// TestPlacementDelta_ReturnFromDeadIdentifiesOverReplication proves that a node coming back
// from Dead reclaims its arcs (Added) and the fallback that covered for it is flagged for
// over-replication GC (Removed) — without any migration being implied. (ADR-031)
func TestPlacementDelta_ReturnFromDeadIdentifiesOverReplication(t *testing.T) {
	live := ringWith(64, "a", "b", "c", "d")
	before := live.Clone()
	before.SetState("a", NodeDead) // while a was down, a fallback held a's arcs
	after := live.Clone()          // a is active again

	changes := PlacementDelta(before, after, 3)
	if len(changes) == 0 {
		t.Fatal("a node returning from Dead must reveal the over-replication to GC")
	}
	for _, c := range changes {
		if !contains(c.Added, "a") {
			t.Errorf("returning node a must reclaim ownership (Added): %+v", c)
		}
		if len(c.Removed) != 1 || c.Removed[0] == "a" {
			t.Errorf("exactly one fallback (not a) should be flagged for GC, got %v", c.Removed)
		}
	}
}

// TestPlacementDelta_PermutedOrderIsNotAChange proves the diff compares owner SETS, not
// clockwise order: re-adding the same nodes (which can reshuffle virtual-node order) is not
// reported as movement when the resulting owner sets are unchanged.
func TestPlacementDelta_RebuiltRingIsStable(t *testing.T) {
	before := ringWith(128, "a", "b", "c")
	after := NewRing(128)
	// Add in a different insertion order; the layout is a deterministic function of the
	// member set (sortRing tie-breaks on ID), so the owner sets must match exactly.
	for _, id := range []string{"c", "a", "b"} {
		after.AddNode(Node{ID: id, Addr: id, State: NodeActive})
	}
	if changes := PlacementDelta(before, after, 3); len(changes) != 0 {
		t.Fatalf("the same member set must yield no ownership change regardless of insertion order, got %d", len(changes))
	}
}

// TestClone_Independent proves a clone does not alias the original: mutating one leaves the
// other untouched, the property the before/after snapshot diff relies on.
func TestClone_Independent(t *testing.T) {
	orig := ringWith(16, "a", "b", "c")
	clone := orig.Clone()
	clone.SetState("a", NodeLeaving)
	clone.RemoveNode("b")

	if st, _ := orig.State("a"); st != NodeActive {
		t.Errorf("mutating the clone changed the original's state: a=%s", st)
	}
	if orig.NodeCount() != 3 {
		t.Errorf("removing from the clone changed the original's membership: count=%d", orig.NodeCount())
	}
	if clone.NodeCount() != 2 {
		t.Errorf("clone removal did not take: count=%d", clone.NodeCount())
	}
}

// TestPlacementDelta_DegenerateInputs proves the diff is robust to empty rings and a
// non-positive replication factor.
func TestPlacementDelta_DegenerateInputs(t *testing.T) {
	empty := NewRing(8)
	if changes := PlacementDelta(empty, empty.Clone(), 3); len(changes) != 0 {
		t.Errorf("empty rings must show no change, got %d", len(changes))
	}
	r := ringWith(8, "a", "b")
	if changes := PlacementDelta(r, r.Clone(), 0); changes != nil {
		t.Errorf("replication<=0 must return nil, got %v", changes)
	}
}

// TestPlacementDelta_ArcsCoverChangedKeyspace is a sanity check that the reported arcs
// actually contain keys whose ownership changed, by sampling keys and confirming the diff's
// classification matches a direct owner walk on each ring.
func TestPlacementDelta_ArcsMatchDirectWalk(t *testing.T) {
	before := ringWith(32, "a", "b", "c")
	after := before.Clone()
	after.AddNode(Node{ID: "d", Addr: "d", State: NodeJoining})
	changes := PlacementDelta(before, after, 3)

	// For every arc in every change, the arc's end position must classify (via a direct walk)
	// to exactly the Before/After owner sets the change advertises.
	for _, c := range changes {
		for _, arc := range c.Arcs {
			bw := ownersAtUnlocked(before, arc.End, 3)
			aw := ownersAtUnlocked(after, arc.End, 3)
			if !equalStringSets(bw, c.Before) || !equalStringSets(aw, c.After) {
				t.Fatalf("arc %+v classified as before=%v after=%v but change says before=%v after=%v",
					arc, bw, aw, c.Before, c.After)
			}
		}
	}
	_ = fmt.Sprint // keep fmt imported for potential debugging
}

// ownersAtUnlocked is a test helper that takes the lock and computes owners at a position,
// mirroring what PlacementDelta does internally.
func ownersAtUnlocked(r *Ring, h uint64, replication int) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ownersAtLocked(h, replication)
}
