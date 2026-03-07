package service

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/meridiandb/meridian/internal/cluster"
)

// rebalStats holds the rebalance counters (ADR-031). All are cumulative.
type rebalStats struct {
	migrations    atomic.Int64 // arc-group → new-owner transfers acknowledged
	samplesMoved  atomic.Int64 // samples pushed to new owners
	bytesMoved    atomic.Int64 // bytes of backfill bodies pushed
	gcRuns        atomic.Int64 // drop-range calls a loser acknowledged
	seriesDropped atomic.Int64 // series GC'd by those drops
	samplesGCed   atomic.Int64 // samples GC'd by those drops
	nodesJoined   atomic.Int64 // JoinNode calls
	nodesLeft     atomic.Int64 // LeaveNode calls
	skipped       atomic.Int64 // changes deferred (no reachable source, a new owner unconfirmed, or no quorum)
}

// RebalanceStats is a snapshot of the rebalance counters, for metrics (ADR-031).
type RebalanceStats struct {
	Migrations    int64
	SamplesMoved  int64
	BytesMoved    int64
	GCRuns        int64
	SeriesDropped int64
	SamplesGCed   int64
	NodesJoined   int64
	NodesLeft     int64
	Skipped       int64
}

// RebalanceStats returns a snapshot of the rebalance counters (ADR-031).
func (c *StorageClient) RebalanceStats() RebalanceStats {
	return RebalanceStats{
		Migrations:    c.rebal.migrations.Load(),
		SamplesMoved:  c.rebal.samplesMoved.Load(),
		BytesMoved:    c.rebal.bytesMoved.Load(),
		GCRuns:        c.rebal.gcRuns.Load(),
		SeriesDropped: c.rebal.seriesDropped.Load(),
		SamplesGCed:   c.rebal.samplesGCed.Load(),
		NodesJoined:   c.rebal.nodesJoined.Load(),
		NodesLeft:     c.rebal.nodesLeft.Load(),
		Skipped:       c.rebal.skipped.Load(),
	}
}

// RebalanceOptions configures a rebalance pass (ADR-031).
type RebalanceOptions struct {
	// Lookback bounds how far back data is migrated and GC'd. 0 reconciles all history; a
	// finite value bounds the per-pass read cost on large datasets.
	Lookback time.Duration
	// MaxBytesPerRound soft-caps the bytes transferred in one migrate phase (the throughput
	// rate limit). 0 is unlimited. When the cap is hit the pass stops scheduling further
	// transfers and reports incomplete, so a node is not promoted/removed until a later pass
	// finishes — the un-migrated arcs keep their data on the existing owners meanwhile.
	MaxBytesPerRound int64
}

// JoinNode brings a new storage node into the cluster and migrates to it the hash ranges it
// now owns, reusing the backfill transfer path, then promotes it to Active and GCs the
// displaced owners' now-un-owned copies (ADR-031). The sequence is the safety contract:
//
//  1. The node is added as Joining — kept out of live read/write routing — so reads and writes
//     keep flowing to the existing owners while it catches up.
//  2. Each affected arc's data is read from a current owner and pushed to the joiner; only
//     when every target owner has acknowledged does the join count as complete.
//  3. The joiner is promoted to Active (now serving), which drops the displaced owner out of
//     each affected arc's routing set, so the subsequent GC of that owner cannot be undone by
//     read-repair re-adding the data.
//  4. The displaced owners drop the arcs — but only after the new owners are confirmed holders
//     at quorum, never the last copy.
//
// If migration cannot complete this pass (a source or a new owner is unreachable, or the byte
// cap is hit) the node is left Joining and nothing is GC'd; the data stays on its existing
// owners and a later call finishes the move. Safe to retry. Synchronous; intended for an admin
// action or a controlled scale-out.
func (c *StorageClient) JoinNode(ctx context.Context, addr string, opts RebalanceOptions) RebalanceStats {
	c.setMigrating(addr)

	c.addJoining(addr)
	after := c.ring.Clone()
	before := after.Clone()
	before.RemoveNode(addr) // the joiner did not own anything before it joined
	changes := cluster.PlacementDelta(before, after, c.rf)

	outcomes, complete := c.migratePhase(ctx, changes, opts)
	if complete {
		c.ring.SetState(addr, cluster.NodeActive) // caught up: serve live traffic and become an owner
		c.gcPhase(ctx, outcomes)
		c.clearMigrating(addr) // hand the node back to the health monitor
	}
	// On an incomplete pass the node is left Joining and still flagged migrating, so the health
	// monitor cannot promote a node that has not received all its ranges; a later JoinNode call
	// resumes and finishes the move. Meanwhile its data stays on the existing owners.
	c.rebal.nodesJoined.Add(1)
	return c.RebalanceStats()
}

// LeaveNode gracefully removes a storage node: it migrates the node's hash ranges to their new
// owners (reusing the backfill transfer) while the node is still Active and serving, so reads
// stay complete throughout, then marks it Leaving (out of routing, its data now on the new
// owners), GCs its shed ranges, and finally drops it from the ring (ADR-031). The node is
// removed only once every new owner has confirmed receipt at quorum; if migration cannot
// complete this pass the node is left Leaving and a later call finishes it. Safe to retry.
func (c *StorageClient) LeaveNode(ctx context.Context, addr string, opts RebalanceOptions) RebalanceStats {
	before := c.ring.Clone()
	before.SetState(addr, cluster.NodeActive) // treat as a full owner regardless of current state
	after := before.Clone()
	after.SetState(addr, cluster.NodeLeaving) // target placement: the leaver owns nothing
	changes := cluster.PlacementDelta(before, after, c.rf)

	outcomes, complete := c.migratePhase(ctx, changes, opts) // migrate while addr still serves reads
	c.ring.SetState(addr, cluster.NodeLeaving)               // now exclude addr from routing
	c.gcPhase(ctx, outcomes)                                 // shed the leaver's data (out of routing → no read-repair re-add)
	if complete {
		c.removeNode(addr)
	}
	c.rebal.nodesLeft.Add(1)
	return c.RebalanceStats()
}

// GCReturnedNode reclaims the over-replication a node's absence created: while it was Dead,
// degraded writes for its ranges landed on fallback replicas, so on its return those fallbacks
// hold copies they do not own (ADR-031). This GCs them once the natural owners are confirmed
// holders at quorum — never the last copy. It does NOT migrate: the returned node catches up
// through hinted handoff / anti-entropy / read-repair, and the fallbacks' data is already on
// the always-up natural owners. The node must already be back in routing (Active or catching
// up) when this is called.
func (c *StorageClient) GCReturnedNode(ctx context.Context, addr string, opts RebalanceOptions) RebalanceStats {
	before := c.ring.Clone()
	before.SetState(addr, cluster.NodeDead) // the degraded window, as if it were still down
	after := c.ring.Clone()
	after.SetState(addr, cluster.NodeActive) // the returned, reclaimed placement
	changes := cluster.PlacementDelta(before, after, c.rf)

	// No migration: every change is GC-eligible immediately (the fallbacks' data is safe on
	// the natural owners that never went down).
	outcomes := make([]migrateOutcome, len(changes))
	for i, ch := range changes {
		outcomes[i] = migrateOutcome{change: ch, migrated: true}
	}
	c.gcPhase(ctx, outcomes)
	return c.RebalanceStats()
}

// migrateOutcome pairs an ownership change with whether its data reached every new owner this
// pass, so the GC phase only sheds an old copy once the new owners are in place.
type migrateOutcome struct {
	change   cluster.OwnershipChange
	migrated bool
}

// migratePhase pushes each change's arc data from a current owner to its new owners, reusing
// the backfill transfer. It returns a per-change outcome and whether every change fully
// migrated (the gate JoinNode/LeaveNode use before promoting/removing a node). It is sequential
// — that, plus the optional byte cap, is the migration rate limit.
func (c *StorageClient) migratePhase(ctx context.Context, changes []cluster.OwnershipChange, opts RebalanceOptions) ([]migrateOutcome, bool) {
	start, end := rebalSpan(opts)
	var bytesThisRound int64
	outcomes := make([]migrateOutcome, 0, len(changes))
	all := true
	for _, ch := range changes {
		select {
		case <-ctx.Done():
			return outcomes, false
		default:
		}
		mo := migrateOutcome{change: ch, migrated: true}
		if len(ch.Added) > 0 {
			if opts.MaxBytesPerRound > 0 && bytesThisRound >= opts.MaxBytesPerRound {
				mo.migrated = false
				all = false
				c.rebal.skipped.Add(1)
				outcomes = append(outcomes, mo)
				continue
			}
			moved, ok := c.migrateChange(ctx, ch, start, end)
			bytesThisRound += moved
			mo.migrated = ok
			if !ok {
				all = false
				c.rebal.skipped.Add(1)
			}
		}
		outcomes = append(outcomes, mo)
	}
	return outcomes, all
}

// migrateChange reads one change's arcs from a reachable current owner and pushes them to each
// new owner through the out-of-order-tolerant backfill endpoint. It returns the bytes pushed
// and whether every new owner acknowledged (the receipt confirmation GC depends on). Backfill
// is idempotent gap-fill, so a re-pushed arc a new owner already holds is a no-op.
func (c *StorageClient) migrateChange(ctx context.Context, ch cluster.OwnershipChange, start, end int64) (int64, bool) {
	src := c.pickSource(ch)
	if src == "" {
		return 0, false // no reachable holder to migrate from this pass
	}
	series, ok := c.fetchRange(ctx, src, arcsToWire(ch.Arcs), start, end)
	if !ok {
		return 0, false
	}
	body, err := json.Marshal(WriteRequest{TimeSeries: series})
	if err != nil {
		return 0, false
	}
	nsamples := countWireSamples(series)
	var bytes int64
	allAcked := true
	for _, gainer := range ch.Added {
		if c.postBackfill(ctx, gainer, body) {
			c.rebal.migrations.Add(1)
			c.rebal.samplesMoved.Add(nsamples)
			c.rebal.bytesMoved.Add(int64(len(body)))
			bytes += int64(len(body))
		} else {
			allAcked = false
		}
	}
	return bytes, allAcked
}

// gcPhase drops each change's shed arcs from its losing owners, but only once the new owners
// are confirmed holders at quorum and never the last copy (ADR-031). A change whose migration
// did not complete this pass is skipped (its data stays on the existing owners). It is called
// after the ring has been flipped to the target placement so a loser is already out of each
// arc's routing set — read-repair therefore cannot re-add what the drop removes.
func (c *StorageClient) gcPhase(ctx context.Context, outcomes []migrateOutcome) {
	for _, mo := range outcomes {
		select {
		case <-ctx.Done():
			return
		default:
		}
		ch := mo.change
		if len(ch.Removed) == 0 || !mo.migrated {
			continue
		}
		// Confirmed holders = target owners currently live. After a join promotion / leave
		// transition these are exactly the new placement, holding the migrated data.
		confirmed := 0
		for _, id := range ch.After {
			if c.isLive(id) {
				confirmed++
			}
		}
		quorum := c.w
		if quorum > len(ch.After) {
			quorum = len(ch.After)
		}
		if quorum < 1 {
			quorum = 1
		}
		// Quorum of confirmed holders AND at least one (the hard last-copy guard) before any drop.
		if confirmed < quorum || confirmed == 0 {
			c.rebal.skipped.Add(1)
			continue
		}
		for _, loser := range ch.Removed {
			if containsStr(ch.After, loser) {
				continue // never drop from a node that is itself a target owner
			}
			if res, ok := c.dropRange(ctx, loser, ch.Arcs); ok {
				c.rebal.gcRuns.Add(1)
				c.rebal.seriesDropped.Add(int64(res.SeriesDropped))
				c.rebal.samplesGCed.Add(res.SamplesDropped)
			}
		}
	}
}

// pickSource returns a reachable node from a change's Before set that holds the arcs' data:
// preferring a staying owner that is Active (it both holds the data and serves), then any
// Active before-owner, then a Leaving one (still up, still holding it). A Joining or Dead
// before-owner is never a source — the first has not caught up, the second is unreachable.
func (c *StorageClient) pickSource(ch cluster.OwnershipChange) string {
	var active, leaving string
	for _, id := range ch.Before {
		st, ok := c.ring.State(id)
		if !ok {
			continue
		}
		switch st {
		case cluster.NodeActive:
			if containsStr(ch.After, id) {
				return id // staying owner: the best source
			}
			if active == "" {
				active = id
			}
		case cluster.NodeLeaving:
			if leaving == "" {
				leaving = id
			}
		}
	}
	if active != "" {
		return active
	}
	return leaving
}

// dropRange tells a storage node to drop the series in the given arcs (the rebalance GC),
// returning what it removed and whether it acknowledged.
func (c *StorageClient) dropRange(ctx context.Context, addr string, arcs []cluster.HashArc) (DropResponse, bool) {
	body, err := json.Marshal(DropRequest{Ranges: arcsToWire(arcs)})
	if err != nil {
		return DropResponse{}, false
	}
	resp, ok := c.postInternal(ctx, addr, "/api/internal/rebalance/drop", body)
	if !ok {
		return DropResponse{}, false
	}
	defer resp.Body.Close()
	var dr DropResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return DropResponse{}, false
	}
	return dr, true
}

// addJoining registers addr as a joining node (kept out of routing until promoted) and adds it
// to the scatter address list. Idempotent.
func (c *StorageClient) addJoining(addr string) {
	c.membMu.Lock()
	exists := false
	for _, a := range c.addrs {
		if a == addr {
			exists = true
			break
		}
	}
	if !exists {
		c.addrs = append(c.addrs, addr)
	}
	c.membMu.Unlock()
	c.ring.AddNode(cluster.Node{ID: addr, Addr: addr, State: cluster.NodeJoining})
}

// removeNode drops addr from the ring and the scatter address list (a fully departed node).
func (c *StorageClient) removeNode(addr string) {
	c.membMu.Lock()
	kept := make([]string, 0, len(c.addrs))
	for _, a := range c.addrs {
		if a != addr {
			kept = append(kept, a)
		}
	}
	c.addrs = kept
	delete(c.migrating, addr)
	c.membMu.Unlock()
	c.ring.RemoveNode(addr)
}

// setMigrating / clearMigrating / isMigrating mark a node as being migrated into, so the health
// monitor's applyReachable does not promote it to Active out from under the rebalancer (which
// owns the Joining→Active transition for a node it is filling). See ADR-031 and ADR-029.
func (c *StorageClient) setMigrating(addr string) {
	c.membMu.Lock()
	c.migrating[addr] = true
	c.membMu.Unlock()
}

func (c *StorageClient) clearMigrating(addr string) {
	c.membMu.Lock()
	delete(c.migrating, addr)
	c.membMu.Unlock()
}

func (c *StorageClient) isMigrating(addr string) bool {
	c.membMu.RLock()
	defer c.membMu.RUnlock()
	return c.migrating[addr]
}

// isLive reports whether a node is currently Active (serving live traffic).
func (c *StorageClient) isLive(id string) bool {
	st, ok := c.ring.State(id)
	return ok && st == cluster.NodeActive
}

// rebalSpan computes the [start, end] sample span a rebalance pass migrates/GCs: all history
// by default, or the trailing Lookback window when set. The end is open-ended so freshly
// written points are included.
func rebalSpan(opts RebalanceOptions) (int64, int64) {
	end := int64(1) << 62
	start := int64(0)
	if opts.Lookback > 0 {
		start = time.Now().UnixMilli() - opts.Lookback.Milliseconds()
	}
	return start, end
}

func countWireSamples(series []TimeSeries) int64 {
	var n int64
	for _, ts := range series {
		n += int64(len(ts.Samples))
	}
	return n
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
