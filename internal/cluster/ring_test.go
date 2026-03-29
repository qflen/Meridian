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
