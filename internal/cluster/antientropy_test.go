package cluster

import (
	"fmt"
	"sort"
	"testing"
)

func sortedIDs(nodes []Node) []string {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	sort.Strings(ids)
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHashArcContains(t *testing.T) {
	normal := HashArc{Start: 10, End: 20}
	if normal.Contains(10) || !normal.Contains(11) || !normal.Contains(20) || normal.Contains(21) {
		t.Fatalf("normal arc (10,20] membership wrong")
	}
	// Wrapping arc: owns (max-5, max] and [0, 5].
	wrap := HashArc{Start: ^uint64(0) - 5, End: 5}
	if !wrap.Contains(0) || !wrap.Contains(5) || wrap.Contains(6) {
		t.Fatalf("wrap arc low side membership wrong")
	}
	if !wrap.Contains(^uint64(0)) || !wrap.Contains(^uint64(0)-4) || wrap.Contains(^uint64(0)-5) {
		t.Fatalf("wrap arc high side membership wrong")
	}
	// Degenerate single-position arc owns everything.
	full := HashArc{Start: 7, End: 7}
	if !full.Contains(0) || !full.Contains(7) || !full.Contains(^uint64(0)) {
		t.Fatalf("degenerate arc should own the whole ring")
	}
}

func TestHashKeyMatchesRouting(t *testing.T) {
	// HashKey must agree with the placement the ring uses internally: a key routes to
	// the first ring boundary at or clockwise of HashKey(key).
	r := NewRing(64)
	for _, id := range []string{"a", "b", "c"} {
		r.AddNode(Node{ID: id, Addr: id, State: NodeActive})
	}
	groups := r.ReplicaGroups(2)
	for i := 0; i < 500; i++ {
		key := MetricKey(fmt.Sprintf("metric_%d", i), map[string]string{"host": fmt.Sprintf("h%d", i%17)})
		h := HashKey(key)
		// Exactly one group must own this position.
		owning := 0
		var got ReplicaGroup
		for _, g := range groups {
			for _, arc := range g.Arcs {
				if arc.Contains(h) {
					owning++
					got = g
					break
				}
			}
		}
		if owning != 1 {
			t.Fatalf("key %q (h=%d) owned by %d groups, want exactly 1", key, h, owning)
		}
		want := sortedIDs(r.PreferenceList(key, 2))
		if !equalStrings(got.Replicas, want) {
			t.Fatalf("key %q: group replicas %v, want PreferenceList %v", key, got.Replicas, want)
		}
	}
}

func TestReplicaGroupsFullReplication(t *testing.T) {
	// With RF == node count every arc is owned by every node, so there is exactly one
	// group covering the whole ring.
	r := NewRing(128)
	for _, id := range []string{"n1", "n2", "n3"} {
		r.AddNode(Node{ID: id, Addr: id, State: NodeActive})
	}
	groups := r.ReplicaGroups(3)
	if len(groups) != 1 {
		t.Fatalf("RF=3 over 3 nodes: got %d groups, want 1", len(groups))
	}
	if !equalStrings(groups[0].Replicas, []string{"n1", "n2", "n3"}) {
		t.Fatalf("group replicas = %v, want all three nodes", groups[0].Replicas)
	}

	// Arcs must tile the whole keyspace exactly once. Probe many positions.
	for i := 0; i < 1000; i++ {
		h := HashKey(fmt.Sprintf("probe-%d", i))
		hits := 0
		for _, arc := range groups[0].Arcs {
			if arc.Contains(h) {
				hits++
			}
		}
		if hits != 1 {
			t.Fatalf("position %d covered by %d arcs, want exactly 1 (arcs must tile)", h, hits)
		}
	}
}

func TestReplicaGroupsDeterministic(t *testing.T) {
	r := NewRing(64)
	for _, id := range []string{"x", "y", "z", "w"} {
		r.AddNode(Node{ID: id, Addr: id, State: NodeActive})
	}
	a := r.ReplicaGroups(2)
	b := r.ReplicaGroups(2)
	if len(a) != len(b) {
		t.Fatalf("non-deterministic group count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !equalStrings(a[i].Replicas, b[i].Replicas) {
			t.Fatalf("group %d replicas differ between calls: %v vs %v", i, a[i].Replicas, b[i].Replicas)
		}
	}
	// Node state must not change the grouping (groups are the natural owners).
	r.SetState("y", NodeDead)
	c := r.ReplicaGroups(2)
	if len(c) != len(a) {
		t.Fatalf("group count changed when a node went down: %d vs %d", len(c), len(a))
	}
}

func TestReplicaGroupsEmpty(t *testing.T) {
	r := NewRing(16)
	if g := r.ReplicaGroups(3); g != nil {
		t.Fatalf("empty ring should yield no groups, got %v", g)
	}
}
