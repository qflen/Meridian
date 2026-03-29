// Package cluster implements consistent hash ring sharding and cluster coordination.
package cluster

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
)

// Ring implements a consistent hash ring with virtual nodes for even distribution.
type Ring struct {
	mu           sync.RWMutex
	virtualNodes int
	nodes        map[string]Node
	ring         []ringEntry
}

type ringEntry struct {
	hash   uint32
	nodeID string
}

// NewRing creates a consistent hash ring with the specified number of virtual nodes per physical node.
func NewRing(virtualNodes int) *Ring {
	if virtualNodes <= 0 {
		virtualNodes = 256
	}
	return &Ring{
		virtualNodes: virtualNodes,
		nodes:        make(map[string]Node),
	}
}

// AddNode adds a node to the ring with virtual node entries.
func (r *Ring) AddNode(node Node) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nodes[node.ID] = node

	for i := 0; i < r.virtualNodes; i++ {
		key := fmt.Sprintf("%s-%d", node.ID, i)
		hash := hashKey(key)
		r.ring = append(r.ring, ringEntry{hash: hash, nodeID: node.ID})
	}

	sort.Slice(r.ring, func(i, j int) bool {
		return r.ring[i].hash < r.ring[j].hash
	})
}

// RemoveNode removes a node and its virtual entries from the ring.
func (r *Ring) RemoveNode(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.nodes, id)

	filtered := make([]ringEntry, 0, len(r.ring))
	for _, e := range r.ring {
		if e.nodeID != id {
			filtered = append(filtered, e)
		}
	}
	r.ring = filtered
}

// GetNodes returns the N nodes responsible for the given key (for replication).
func (r *Ring) GetNodes(key string, replication int) []Node {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.ring) == 0 {
		return nil
	}

	hash := hashKey(key)
	idx := sort.Search(len(r.ring), func(i int) bool {
		return r.ring[i].hash >= hash
	})
	if idx >= len(r.ring) {
		idx = 0
	}

	seen := make(map[string]bool)
	var result []Node

	for i := 0; i < len(r.ring) && len(result) < replication; i++ {
		entry := r.ring[(idx+i)%len(r.ring)]
		if !seen[entry.nodeID] {
			seen[entry.nodeID] = true
			if node, ok := r.nodes[entry.nodeID]; ok {
				result = append(result, node)
			}
		}
	}

	return result
}

// GetNode returns the primary node for the given key.
func (r *Ring) GetNode(key string) *Node {
	nodes := r.GetNodes(key, 1)
	if len(nodes) == 0 {
		return nil
	}
	return &nodes[0]
}

// Nodes returns all registered physical nodes.
func (r *Ring) Nodes() []Node {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		result = append(result, n)
	}
	return result
}

// NodeCount returns the number of physical nodes in the ring.
func (r *Ring) NodeCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.nodes)
}

func hashKey(key string) uint32 {
	h := sha256.Sum256([]byte(key))
	return binary.BigEndian.Uint32(h[:4])
}
