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
	hash   uint64
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

// AddNode adds a node to the ring with virtual node entries. Re-adding an existing
// node ID updates its record (e.g. its state) without duplicating ring entries.
func (r *Ring) AddNode(node Node) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.nodes[node.ID]; exists {
		r.nodes[node.ID] = node
		return
	}
	r.nodes[node.ID] = node

	for i := 0; i < r.virtualNodes; i++ {
		key := fmt.Sprintf("%s-%d", node.ID, i)
		hash := hashKey(key)
		r.ring = append(r.ring, ringEntry{hash: hash, nodeID: node.ID})
	}

	r.sortRing()
}

// sortRing orders ring entries by hash, breaking ties on nodeID so the ring layout
// is a deterministic function of its membership — independent of insertion order.
// Without the tie-break, two virtual nodes colliding on the same 64-bit hash would
// resolve in append order, making replica sets depend on the sequence of AddNode
// calls rather than on the set of members.
func (r *Ring) sortRing() {
	sort.Slice(r.ring, func(i, j int) bool {
		if r.ring[i].hash != r.ring[j].hash {
			return r.ring[i].hash < r.ring[j].hash
		}
		return r.ring[i].nodeID < r.ring[j].nodeID
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

// SetState updates the lifecycle state of an already-registered node. It is the hook
// the health monitor uses to mark nodes Active/Dead/Joining so routing excludes the
// non-serving ones. Unknown node IDs are ignored.
func (r *Ring) SetState(id string, state NodeState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[id]; ok {
		n.State = state
		r.nodes[id] = n
	}
}

// State returns the lifecycle state of a registered node and whether it is registered.
// It lets the health monitor and the hinted-handoff replay loop read a node's current
// state to drive the Dead → Joining → Active catch-up transition. See ADR-029.
func (r *Ring) State(id string) (NodeState, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[id]
	if !ok {
		return "", false
	}
	return n.State, true
}

// GetNodes returns up to replication distinct nodes responsible for the given key,
// walking the ring clockwise from the key's hash. Nodes excluded from routing — Dead,
// Leaving, or Joining (catching up via hinted handoff) — are skipped, so the result
// holds only replicas that can currently serve the key. The returned slice can
// therefore be shorter than replication when fewer live nodes exist — callers compare
// its length against the write/read quorum. To find the natural preferred owners
// including the excluded ones (so a missed owner can be hinted), use PreferenceList.
func (r *Ring) GetNodes(key string, replication int) []Node {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.ring) == 0 || replication <= 0 {
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
		if seen[entry.nodeID] {
			continue
		}
		seen[entry.nodeID] = true
		node, ok := r.nodes[entry.nodeID]
		if !ok || node.State.excludedFromRouting() {
			continue
		}
		result = append(result, node)
	}

	return result
}

// PreferenceList returns up to replication distinct nodes responsible for the key,
// walking the ring clockwise from the key's hash WITHOUT skipping any state — the
// natural preferred owners, including nodes that are Dead, Leaving, or Joining. It is
// the counterpart to GetNodes (which returns only the live, routable replicas, possibly
// substituting fallbacks for excluded owners): hinted handoff diffs the two to find a
// natural owner a write could not reach and buffers a hint for it. See ADR-029.
func (r *Ring) PreferenceList(key string, replication int) []Node {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.ring) == 0 || replication <= 0 {
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
		if seen[entry.nodeID] {
			continue
		}
		seen[entry.nodeID] = true
		if node, ok := r.nodes[entry.nodeID]; ok {
			result = append(result, node)
		}
	}
	return result
}

// GetNode returns the primary live node for the given key, or nil if none is live.
func (r *Ring) GetNode(key string) *Node {
	nodes := r.GetNodes(key, 1)
	if len(nodes) == 0 {
		return nil
	}
	return &nodes[0]
}

// Nodes returns all registered physical nodes regardless of state.
func (r *Ring) Nodes() []Node {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		result = append(result, n)
	}
	return result
}

// LiveNodes returns every node eligible to serve live traffic — i.e. not Dead, Leaving,
// or Joining (catching up) — the set a scatter read may safely query. The order is
// unspecified.
func (r *Ring) LiveNodes() []Node {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		if n.State.excludedFromRouting() {
			continue
		}
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

// hashKey maps a key onto the 64-bit ring. Sixty-four bits make a same-position
// collision between two physical nodes astronomically unlikely, so a key's replica
// set is determined by ring position alone (with sortRing's nodeID tie-break as the
// deterministic fallback if a collision ever did occur).
func hashKey(key string) uint64 {
	h := sha256.Sum256([]byte(key))
	return binary.BigEndian.Uint64(h[:8])
}
