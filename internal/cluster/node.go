package cluster

// NodeState represents the lifecycle state of a cluster node.
//
// Only NodeActive and NodeDead are currently assigned — the health monitor flips a
// node between them based on /health reachability. NodeJoining and NodeLeaving are
// reserved for graceful membership changes (hinted handoff, rebalancing) that are not
// yet implemented; GetNodes already excludes NodeLeaving defensively so the state is
// safe to set once handoff lands, but nothing sets it today. See ADR-022.
type NodeState string

const (
	// NodeJoining indicates the node is joining the cluster. Reserved; not yet assigned.
	NodeJoining NodeState = "joining"
	// NodeActive indicates the node is actively serving requests.
	NodeActive NodeState = "active"
	// NodeLeaving indicates the node is gracefully leaving the cluster. Reserved; not yet assigned.
	NodeLeaving NodeState = "leaving"
	// NodeDead indicates the node is unreachable.
	NodeDead NodeState = "dead"
)

// Node represents a single cluster member.
type Node struct {
	ID    string
	Addr  string
	State NodeState
}
