package cluster

// NodeState represents the lifecycle state of a cluster node.
//
// The health monitor flips a node between NodeActive and NodeDead based on /health
// reachability. NodeJoining is the catching-up state used by hinted handoff (ADR-029):
// a node returning from Dead with a backlog of buffered hints enters it, replays the
// hints through the out-of-order-tolerant backfill path, and is promoted to NodeActive
// only once caught up — so it never receives live writes (or serves reads) while still
// holding a gap. NodeLeaving drives graceful departure during a rebalance (ADR-031): a node
// being removed migrates its ranges to their new owners while still serving, then enters
// NodeLeaving (out of routing) so its shed data can be GC'd before it is dropped from the
// ring. See ADR-022, ADR-029, and ADR-031.
type NodeState string

const (
	// NodeJoining indicates the node is catching up (replaying hinted-handoff backfill)
	// before it rejoins live routing; it is kept out of read/write routing until promoted.
	NodeJoining NodeState = "joining"
	// NodeActive indicates the node is actively serving requests.
	NodeActive NodeState = "active"
	// NodeLeaving indicates the node is gracefully leaving the cluster: kept out of routing
	// while a rebalance sheds its now-un-owned data before it is removed (ADR-031).
	NodeLeaving NodeState = "leaving"
	// NodeDead indicates the node is unreachable.
	NodeDead NodeState = "dead"
)

// excludedFromRouting reports whether a node in this state must be kept out of the live
// read/write routing sets: the unreachable (Dead), the gracefully departing (Leaving),
// and the catching-up (Joining, still replaying hinted-handoff backfill). Only Active
// nodes serve live traffic; GetNodes and LiveNodes skip the rest. See ADR-029.
func (s NodeState) excludedFromRouting() bool {
	return s == NodeDead || s == NodeLeaving || s == NodeJoining
}

// Node represents a single cluster member.
type Node struct {
	ID    string
	Addr  string
	State NodeState
}
