package cluster

import (
	"testing"
)

func TestCoordinatorRouteWrite(t *testing.T) {
	c := NewCoordinator("node-1", 3, 256)
	c.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	c.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	c.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})

	nodes := c.RouteWrite("cpu_usage", map[string]string{"host": "web-01"})
	if len(nodes) != 3 {
		t.Fatalf("expected 3 write targets, got %d", len(nodes))
	}
}

func TestCoordinatorIsLocalWrite(t *testing.T) {
	c := NewCoordinator("node-1", 3, 256)
	c.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	c.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	c.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})

	// With replication factor 3 and 3 nodes, every metric is local
	isLocal := c.IsLocalWrite("metric", map[string]string{"host": "a"})
	if !isLocal {
		t.Fatal("with RF=3 and 3 nodes, all writes should be local")
	}
}

func TestCoordinatorNodeFailure(t *testing.T) {
	c := NewCoordinator("node-1", 2, 256)
	c.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	c.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	c.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})

	// Route before failure
	nodesBefore := c.RouteWrite("metric_x", map[string]string{"a": "b"})
	if len(nodesBefore) != 2 {
		t.Fatalf("expected 2 write targets, got %d", len(nodesBefore))
	}

	// Remove one node (simulating failure)
	c.RemoveNode("node-2")

	nodesAfter := c.RouteWrite("metric_x", map[string]string{"a": "b"})
	if len(nodesAfter) != 2 {
		t.Fatalf("expected 2 write targets after failure, got %d", len(nodesAfter))
	}

	// Verify no dead node in the result
	for _, n := range nodesAfter {
		if n.ID == "node-2" {
			t.Fatal("dead node should not be in write targets")
		}
	}
}

func TestCoordinatorReadFromReplicas(t *testing.T) {
	c := NewCoordinator("node-1", 3, 256)
	c.AddNode(Node{ID: "node-1", Addr: "host1:8080", State: NodeActive})
	c.AddNode(Node{ID: "node-2", Addr: "host2:8080", State: NodeActive})
	c.AddNode(Node{ID: "node-3", Addr: "host3:8080", State: NodeActive})

	readNodes := c.RouteRead("metric", map[string]string{"host": "web-01"})
	writeNodes := c.RouteWrite("metric", map[string]string{"host": "web-01"})

	// Read and write should target the same nodes
	if len(readNodes) != len(writeNodes) {
		t.Fatalf("read/write node count mismatch: %d vs %d", len(readNodes), len(writeNodes))
	}
}

func TestMetricKey(t *testing.T) {
	key := MetricKey("cpu_usage", map[string]string{"host": "web-01", "region": "us"})
	expected := "cpu_usage{host=web-01,region=us}"
	if key != expected {
		t.Fatalf("got %q, want %q", key, expected)
	}

	// No labels
	key2 := MetricKey("up", nil)
	if key2 != "up" {
		t.Fatalf("got %q, want %q", key2, "up")
	}
}
