package cluster

// NodeState represents the lifecycle state of a cluster node.
type NodeState string

const (
	// NodeJoining indicates the node is joining the cluster.
	NodeJoining NodeState = "joining"
	// NodeActive indicates the node is actively serving requests.
	NodeActive NodeState = "active"
	// NodeLeaving indicates the node is gracefully leaving the cluster.
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
