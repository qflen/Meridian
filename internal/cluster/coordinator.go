package cluster

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Coordinator handles shard routing, replication, and read repair.
type Coordinator struct {
	mu                sync.RWMutex
	ring              *Ring
	localNodeID       string
	replicationFactor int
}

// NewCoordinator creates a new cluster coordinator.
func NewCoordinator(localNodeID string, replicationFactor int, virtualNodes int) *Coordinator {
	return &Coordinator{
		ring:              NewRing(virtualNodes),
		localNodeID:       localNodeID,
		replicationFactor: replicationFactor,
	}
}

// AddNode registers a node with the coordinator's hash ring.
func (c *Coordinator) AddNode(node Node) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ring.AddNode(node)
}

// RemoveNode removes a node from the coordinator's hash ring.
func (c *Coordinator) RemoveNode(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ring.RemoveNode(id)
}

// RouteWrite determines which nodes should receive a write for the given metric key.
func (c *Coordinator) RouteWrite(metricName string, labels map[string]string) []Node {
	key := MetricKey(metricName, labels)
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ring.GetNodes(key, c.replicationFactor)
}

// RouteRead determines which nodes to query for the given metric key.
func (c *Coordinator) RouteRead(metricName string, labels map[string]string) []Node {
	key := MetricKey(metricName, labels)
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ring.GetNodes(key, c.replicationFactor)
}

// IsLocalWrite returns true if the local node is one of the write targets.
func (c *Coordinator) IsLocalWrite(metricName string, labels map[string]string) bool {
	nodes := c.RouteWrite(metricName, labels)
	for _, n := range nodes {
		if n.ID == c.localNodeID {
			return true
		}
	}
	return false
}

// AllNodes returns all nodes in the cluster.
func (c *Coordinator) AllNodes() []Node {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ring.Nodes()
}

// Ring returns the underlying hash ring.
func (c *Coordinator) Ring() *Ring {
	return c.ring
}

// MetricKey computes the hash key for a metric name and labels.
func MetricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%s=%s", k, labels[k])
	}
	sb.WriteByte('}')
	return sb.String()
}
