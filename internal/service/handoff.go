package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/meridiandb/meridian/internal/cluster"
)

// SetHintStore enables hinted handoff on the write path (ADR-029): a write that cannot
// reach a natural replica — it is Dead, catching up, or its live write failed — while
// the live quorum still holds buffers a durable hint for that replica instead of leaving
// it silently stale until read-repair. A nil store (the default) disables hinted handoff,
// so the gateway/querier/compactor clients behave exactly as ADR-022. Set once at
// startup, before the health monitor and replay loop run.
func (c *StorageClient) SetHintStore(h *HintStore) { c.hints = h }

// HintStore returns the configured hint store, or nil when hinted handoff is disabled.
func (c *StorageClient) HintStore() *HintStore { return c.hints }

// NodeState reports the ring lifecycle state of a storage node ("active", "joining",
// "leaving", "dead"), or "" if the address is not in the ring. It surfaces the
// catching-up state hinted handoff drives, for metrics, tests, and topology views.
func (c *StorageClient) NodeState(addr string) string {
	if st, ok := c.ring.State(addr); ok {
		return string(st)
	}
	return ""
}

// applyReachable sets the ring state for a node that just answered /health. Without
// hinted handoff it is simply Active (ADR-022). With hinted handoff a node returning
// from Dead with a backlog enters Joining — catching up, excluded from live writes and
// reads until its hints replay — while a backlog-free return goes straight to Active. An
// already-Active node is never demoted by a transient hint (its backlog replays in the
// background while it keeps serving), and a Joining node whose backlog has drained is
// promoted. See ADR-029.
func (c *StorageClient) applyReachable(addr string) {
	// The rebalancer owns the Joining→Active promotion for a node it is migrating into, so
	// liveness must not promote it early (it would route reads/writes to a node that does not
	// yet hold its ranges). See ADR-031.
	if c.isMigrating(addr) {
		return
	}
	if c.hints == nil {
		c.ring.SetState(addr, cluster.NodeActive)
		return
	}
	cur, _ := c.ring.State(addr)
	switch cur {
	case cluster.NodeActive:
		// Keep serving; any transient hint replays in the background.
	case cluster.NodeJoining:
		if c.hints.Pending(addr) == 0 {
			c.ring.SetState(addr, cluster.NodeActive) // caught up
		}
	default: // Dead, Leaving, or unset
		if c.hints.Pending(addr) > 0 {
			c.ring.SetState(addr, cluster.NodeJoining) // returned with a backlog → catch up first
		} else {
			c.ring.SetState(addr, cluster.NodeActive)
		}
	}
}

// StartHintReplay runs the hinted-handoff replay loop until ctx is cancelled: every
// interval it replays buffered hints to each reachable target through the backfill path
// and promotes a caught-up node to Active. It is a no-op when hinted handoff is disabled.
func (c *StorageClient) StartHintReplay(ctx context.Context, interval time.Duration) {
	if c.hints == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.ReplayHintsOnce(ctx)
			}
		}
	}()
}

// ReplayHintsOnce runs one replay pass: for each target with buffered hints that is
// reachable (not Dead/Leaving), it drains the target's hints through the out-of-order-
// tolerant backfill endpoint in FIFO order, deleting each on the target's ack. A Joining
// target whose backlog fully drains is promoted to Active — it has caught up and may
// resume receiving live writes. Exposed (not just the loop) so tests can drive replay
// deterministically.
func (c *StorageClient) ReplayHintsOnce(ctx context.Context) {
	if c.hints == nil {
		return
	}
	for _, target := range c.hints.Targets() {
		st, ok := c.ring.State(target)
		if !ok || st == cluster.NodeDead || st == cluster.NodeLeaving {
			continue // not reachable for catch-up right now
		}
		_, fullyDrained := c.hints.Drain(target, func(h Hint) bool {
			return c.sendBackfill(ctx, target, h.Series)
		})
		// A clean pass means the node has absorbed the backlog as of the pass start; the
		// small tail that may have arrived during the pass replays next tick to the now-
		// Active node, so there is no reason to keep excluding it from live traffic.
		if fullyDrained && st == cluster.NodeJoining {
			c.ring.SetState(target, cluster.NodeActive)
		}
	}
}

// sendBackfill posts buffered series to a replica's out-of-order-tolerant backfill
// endpoint, returning whether they were applied (HTTP 200). Replay uses this rather than
// the normal write endpoint so historical samples are accepted instead of rejected as
// out of order (ADR-015); the body reuses the write wire shape.
func (c *StorageClient) sendBackfill(ctx context.Context, addr string, series []TimeSeries) bool {
	body, err := json.Marshal(WriteRequest{TimeSeries: series})
	if err != nil {
		return false
	}
	return c.postBackfill(ctx, addr, body)
}

// postBackfill POSTs an already-marshaled WriteRequest body to a replica's backfill
// endpoint, returning whether it was applied (HTTP 200). Splitting the POST from the
// marshaling lets anti-entropy (ADR-030) reuse the exact transfer path while measuring
// the bytes it sent, without encoding the body twice.
func (c *StorageClient) postBackfill(ctx context.Context, addr string, body []byte) bool {
	url := fmt.Sprintf("http://%s/api/internal/backfill", addr)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
