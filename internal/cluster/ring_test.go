package cluster

import (
	"fmt"
	"math"
	"testing"
)

func TestRingAddAndGetNode(t *testing.T) {
	ring := NewRing(256)

	ring.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})

	node := ring.GetNode("cpu_usage{host=web-01}")
	if node == nil {
		t.Fatal("expected a node")
	}
	if node.State != NodeActive {
		t.Fatalf("expected active node")
	}
}

func TestRingReplication(t *testing.T) {
	ring := NewRing(256)

	ring.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})

	nodes := ring.GetNodes("test_metric", 3)
	if len(nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(nodes))
	}

	// All nodes should be unique
	seen := make(map[string]bool)
	for _, n := range nodes {
		if seen[n.ID] {
			t.Fatalf("duplicate node: %s", n.ID)
		}
		seen[n.ID] = true
	}
}

func TestRingReplicationExceedsNodes(t *testing.T) {
	ring := NewRing(256)
	ring.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})

	// Request 3 replicas but only 2 nodes
	nodes := ring.GetNodes("key", 3)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes (capped), got %d", len(nodes))
	}
}

func TestRingDistribution(t *testing.T) {
	ring := NewRing(256)
	ring.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})

	counts := make(map[string]int)
	nKeys := 10000
	for i := 0; i < nKeys; i++ {
		key := fmt.Sprintf("metric_%d{host=host-%d}", i%100, i)
		node := ring.GetNode(key)
		counts[node.ID]++
	}

	// Each node should get roughly 1/3 of keys
	expected := float64(nKeys) / 3.0
	for nodeID, count := range counts {
		deviation := math.Abs(float64(count)-expected) / expected
		t.Logf("node %s: %d keys (%.1f%% deviation)", nodeID, count, deviation*100)
		if deviation > 0.20 {
			t.Errorf("node %s has too much deviation: %d keys (expected ~%.0f)", nodeID, count, expected)
		}
	}
}

func TestRingRemoveNode(t *testing.T) {
	ring := NewRing(256)
	ring.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})

	// Record initial assignments
	key := "test_key"
	nodesBefore := ring.GetNodes(key, 1)

	ring.RemoveNode("node-2")

	nodesAfter := ring.GetNodes(key, 1)
	if len(nodesAfter) != 1 {
		t.Fatalf("expected 1 node after removal")
	}

	// If the primary was node-2, it should have changed
	if nodesBefore[0].ID == "node-2" && nodesAfter[0].ID == "node-2" {
		t.Fatal("key should have been reassigned after node removal")
	}

	if ring.NodeCount() != 2 {
		t.Fatalf("expected 2 nodes, got %d", ring.NodeCount())
	}
}

func TestRingKeyStability(t *testing.T) {
	ring := NewRing(256)
	ring.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})

	// Record assignments for 1000 keys
	nKeys := 1000
	assignments := make(map[string]string)
	for i := 0; i < nKeys; i++ {
		key := fmt.Sprintf("key_%d", i)
		assignments[key] = ring.GetNode(key).ID
	}

	// Add a new node
	ring.AddNode(Node{ID: "node-4", Addr: "host4:8080", State: NodeActive})

	// Count how many keys changed assignment
	changed := 0
	for i := 0; i < nKeys; i++ {
		key := fmt.Sprintf("key_%d", i)
		newNode := ring.GetNode(key).ID
		if newNode != assignments[key] {
			changed++
		}
	}

	changeRate := float64(changed) / float64(nKeys)
	t.Logf("key reassignment after adding node-4: %d/%d (%.1f%%)", changed, nKeys, changeRate*100)
	// With consistent hashing, roughly 1/N keys should move (N = new node count = 4)
	// Allow up to 40% reassignment
	if changeRate > 0.40 {
		t.Errorf("too many keys reassigned: %.1f%%", changeRate*100)
	}
}

func TestRingEmpty(t *testing.T) {
	ring := NewRing(256)
	node := ring.GetNode("key")
	if node != nil {
		t.Fatal("expected nil for empty ring")
	}
	nodes := ring.GetNodes("key", 3)
	if len(nodes) != 0 {
		t.Fatalf("expected 0 nodes for empty ring, got %d", len(nodes))
	}
}

func TestRingSingleNode(t *testing.T) {
	ring := NewRing(256)
	ring.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})

	// Requesting 3 replicas from a 1-node ring yields exactly that one node.
	nodes := ring.GetNodes("any_key", 3)
	if len(nodes) != 1 || nodes[0].ID != "node-1" {
		t.Fatalf("expected [node-1], got %v", nodes)
	}
	if n := ring.GetNode("any_key"); n == nil || n.ID != "node-1" {
		t.Fatalf("expected primary node-1, got %v", n)
	}
}

// TestRingFiltersDeadNodes verifies that a Dead node is excluded from every key's
// replica set, and that a Leaving node is excluded too — routing must never target a
// node that cannot serve the key (Appendix A4).
func TestRingFiltersDeadNodes(t *testing.T) {
	ring := NewRing(256)
	ring.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})

	ring.SetState("node-2", NodeDead)
	ring.SetState("node-3", NodeLeaving)

	for i := 0; i < 2000; i++ {
		key := fmt.Sprintf("series_%d", i)
		for _, n := range ring.GetNodes(key, 3) {
			if n.ID == "node-2" {
				t.Fatalf("dead node-2 routed for key %q", key)
			}
			if n.ID == "node-3" {
				t.Fatalf("leaving node-3 routed for key %q", key)
			}
		}
	}

	// With two of three nodes unavailable, every key resolves to the single live node.
	nodes := ring.GetNodes("series_0", 3)
	if len(nodes) != 1 || nodes[0].ID != "node-1" {
		t.Fatalf("expected only live node-1, got %v", nodes)
	}

	live := ring.LiveNodes()
	if len(live) != 1 || live[0].ID != "node-1" {
		t.Fatalf("expected LiveNodes=[node-1], got %v", live)
	}
}

// TestRingDeadThenActive verifies a node returning from Dead→Active is routed to
// again — the property that lets a recovered replica resume receiving writes.
func TestRingDeadThenActive(t *testing.T) {
	ring := NewRing(256)
	ring.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})

	// Find a key whose replica set includes node-2 while all are healthy.
	var key string
	for i := 0; i < 10000; i++ {
		k := fmt.Sprintf("k_%d", i)
		for _, n := range ring.GetNodes(k, 3) {
			if n.ID == "node-2" {
				key = k
				break
			}
		}
		if key != "" {
			break
		}
	}
	if key == "" {
		t.Fatal("could not find a key routed to node-2")
	}

	ring.SetState("node-2", NodeDead)
	if containsNode(ring.GetNodes(key, 3), "node-2") {
		t.Fatal("dead node-2 should not be routed")
	}

	ring.SetState("node-2", NodeActive)
	if !containsNode(ring.GetNodes(key, 3), "node-2") {
		t.Fatal("revived node-2 should be routed again")
	}
}

// TestRingDeterministicAcrossInsertionOrder proves the ring layout is a function of
// its membership, not of the order nodes were added — required so independent clients
// (ingestor and querier) agree on replica placement.
func TestRingDeterministicAcrossInsertionOrder(t *testing.T) {
	build := func(ids []string) *Ring {
		r := NewRing(256)
		for _, id := range ids {
			r.AddNode(Node{ID: id, Addr: id + ":8080", State: NodeActive})
		}
		return r
	}
	a := build([]string{"node-1", "node-2", "node-3"})
	b := build([]string{"node-3", "node-1", "node-2"})

	for i := 0; i < 5000; i++ {
		key := fmt.Sprintf("metric_%d{host=h-%d}", i%50, i)
		na := a.GetNodes(key, 3)
		nb := b.GetNodes(key, 3)
		if len(na) != len(nb) {
			t.Fatalf("replica-set size differs for %q: %d vs %d", key, len(na), len(nb))
		}
		for j := range na {
			if na[j].ID != nb[j].ID {
				t.Fatalf("replica order differs for %q at %d: %s vs %s", key, j, na[j].ID, nb[j].ID)
			}
		}
	}
}

// TestRingHash64Bit guards against a regression to a 32-bit ring hash: across many
// keys the high 32 bits must vary, which a uint32-truncated hash could never produce.
func TestRingHash64Bit(t *testing.T) {
	highBitsSeen := false
	distinct := make(map[uint64]bool)
	for i := 0; i < 1000; i++ {
		h := hashKey(fmt.Sprintf("key_%d", i))
		distinct[h] = true
		if h>>32 != 0 {
			highBitsSeen = true
		}
	}
	if !highBitsSeen {
		t.Fatal("ring hash never set bits above 32 — looks truncated to 32 bits")
	}
	if len(distinct) < 990 {
		t.Fatalf("ring hash collided heavily: %d distinct of 1000", len(distinct))
	}
}

// TestRingPreferenceListIncludesDead proves PreferenceList returns the natural N owners
// of a key INCLUDING ones that are Dead/Leaving/Joining — exactly where GetNodes would
// substitute a live fallback — so hinted handoff can diff the two and hint the owner a
// write could not reach. See ADR-029.
func TestRingPreferenceListIncludesDead(t *testing.T) {
	ring := NewRing(256)
	ring.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-4", Addr: "host4:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-5", Addr: "host5:8080", State: NodeActive})

	// Find a key whose natural 3-owner set includes node-2, then kill node-2.
	var key string
	for i := 0; i < 10000; i++ {
		k := fmt.Sprintf("k_%d", i)
		if containsNode(ring.PreferenceList(k, 3), "node-2") {
			key = k
			break
		}
	}
	if key == "" {
		t.Fatal("could not find a key whose preference list includes node-2")
	}

	pref := ring.PreferenceList(key, 3)
	if !containsNode(pref, "node-2") {
		t.Fatalf("preference list must contain node-2 while it is active, got %v", pref)
	}

	ring.SetState("node-2", NodeDead)

	// PreferenceList still returns the natural owners (incl. the now-dead node-2)...
	pref = ring.PreferenceList(key, 3)
	if len(pref) != 3 || !containsNode(pref, "node-2") {
		t.Fatalf("preference list must still include dead node-2 (natural owner), got %v", pref)
	}
	// ...while GetNodes drops node-2 and substitutes a live fallback (still 3 live).
	live := ring.GetNodes(key, 3)
	if containsNode(live, "node-2") {
		t.Fatalf("GetNodes must not route to dead node-2, got %v", live)
	}
	if len(live) != 3 {
		t.Fatalf("GetNodes should fill 3 live replicas (a fallback for node-2), got %d", len(live))
	}

	// The hint target is exactly the natural owner GetNodes skipped: pref \ live.
	liveSet := map[string]bool{}
	for _, n := range live {
		liveSet[n.ID] = true
	}
	var missed []string
	for _, n := range pref {
		if !liveSet[n.ID] {
			missed = append(missed, n.ID)
		}
	}
	if len(missed) != 1 || missed[0] != "node-2" {
		t.Fatalf("expected the single missed owner to be node-2, got %v", missed)
	}
}

// TestRingExcludesJoining proves a node in the Joining (catching-up) state is kept out
// of both live write routing (GetNodes) and read scatter (LiveNodes) but remains a
// natural owner in PreferenceList — so a catching-up node receives no live traffic yet
// still accrues hints until it is promoted to Active. See ADR-029.
func TestRingExcludesJoining(t *testing.T) {
	ring := NewRing(256)
	ring.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	ring.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})

	ring.SetState("node-2", NodeJoining)

	for i := 0; i < 2000; i++ {
		key := fmt.Sprintf("series_%d", i)
		if containsNode(ring.GetNodes(key, 3), "node-2") {
			t.Fatalf("joining node-2 must not be routed for live writes (key %q)", key)
		}
	}
	for _, n := range ring.LiveNodes() {
		if n.ID == "node-2" {
			t.Fatal("joining node-2 must not appear in LiveNodes")
		}
	}
	// But it is still a natural owner for hinting.
	includes := false
	for i := 0; i < 2000; i++ {
		if containsNode(ring.PreferenceList(fmt.Sprintf("series_%d", i), 3), "node-2") {
			includes = true
			break
		}
	}
	if !includes {
		t.Fatal("joining node-2 must still be a natural owner in PreferenceList")
	}
}

// TestRingState exercises the State getter that drives the Dead → Joining → Active
// catch-up transition.
func TestRingState(t *testing.T) {
	ring := NewRing(256)
	ring.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})

	if st, ok := ring.State("node-1"); !ok || st != NodeActive {
		t.Fatalf("expected (active,true), got (%q,%v)", st, ok)
	}
	ring.SetState("node-1", NodeJoining)
	if st, _ := ring.State("node-1"); st != NodeJoining {
		t.Fatalf("expected joining, got %q", st)
	}
	if _, ok := ring.State("ghost"); ok {
		t.Fatal("unregistered node must report ok=false")
	}
}

func containsNode(nodes []Node, id string) bool {
	for _, n := range nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}
