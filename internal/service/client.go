package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/meridiandb/meridian/internal/cluster"
	"github.com/meridiandb/meridian/internal/storage"
)

// StorageClient communicates with storage service nodes over HTTP. It routes writes
// and reads through a consistent-hash ring built from the configured storage nodes:
// a series is written to its N ring replicas and succeeds at W acks; reads take R
// responses, merge, and asynchronously read-repair stale replicas. See ADR-022.
type StorageClient struct {
	// membMu guards addrs and migrating, both mutated at runtime by the rebalancer when a
	// node joins or leaves (ADR-031); every scatter that ranges addrs snapshots it first so
	// a membership change cannot race a fan-out. The ring has its own lock.
	membMu    sync.RWMutex
	addrs     []string
	migrating map[string]bool // nodes the rebalancer is migrating into; health must not promote them

	client *http.Client

	ring *cluster.Ring
	rf   int // N — replication factor
	w    int // W — write quorum
	r    int // R — read quorum

	// nodeRes caches each node's advertised rollup tier availability, discovered
	// alongside health. RollupResolutions intersects it over the live nodes so the
	// resolution planner only picks a tier the cluster can serve. Guarded by resMu.
	resMu   sync.RWMutex
	nodeRes map[string]ResolutionsResponse

	// hints, when non-nil, enables hinted handoff (ADR-029): a write that cannot reach
	// a natural replica buffers a durable hint replayed on the replica's return. Set
	// once at startup via SetHintStore; nil leaves the ADR-022 behaviour unchanged. The
	// handoff methods live in handoff.go.
	hints *HintStore

	// ae holds the anti-entropy counters (ADR-030); aeCursor is the round-robin position
	// over the ring's replica groups. Both are owned by the single background sweep
	// goroutine (and tests driving AntiEntropyRound), so aeCursor needs no lock. The
	// anti-entropy methods live in antientropy.go.
	ae       aeStats
	aeCursor int

	// rebal holds the rebalance counters (ADR-031). The membership-change migration/GC
	// methods live in rebalance.go.
	rebal rebalStats
}

// ReplicationOptions configures the replication behaviour of a StorageClient.
type ReplicationOptions struct {
	ReplicationFactor int // N
	WriteQuorum       int // W
	ReadQuorum        int // R
	VirtualNodes      int // ring virtual nodes per physical node
}

// NewStorageClient creates a client for the given storage node addresses with default
// replication: N = min(3, #nodes) and a majority quorum for both reads and writes. It
// is used by services that only fan out aggregate reads (gateway, compactor); the
// ingestor and querier pass explicit options via NewReplicatedStorageClient.
func NewStorageClient(addrs []string) *StorageClient {
	n := len(addrs)
	if n > 3 {
		n = 3
	}
	if n < 1 {
		n = 1
	}
	return NewReplicatedStorageClient(addrs, ReplicationOptions{
		ReplicationFactor: n,
		WriteQuorum:       n/2 + 1,
		ReadQuorum:        n/2 + 1,
		VirtualNodes:      256,
	})
}

// NewReplicatedStorageClient builds a client whose ring is seeded with the given
// storage addresses (each its own physical node, keyed by address). The effective
// replication factor is capped at the number of nodes, and the quorums are clamped
// into [1, N] so a cluster smaller than the configured N degrades gracefully rather
// than rejecting every write — validation of the configured W+R>N happens at config
// load time (ClusterConfig.Validate).
func NewReplicatedStorageClient(addrs []string, opts ReplicationOptions) *StorageClient {
	ring := cluster.NewRing(opts.VirtualNodes)
	for _, addr := range addrs {
		ring.AddNode(cluster.Node{ID: addr, Addr: addr, State: cluster.NodeActive})
	}

	n := opts.ReplicationFactor
	if n > len(addrs) {
		n = len(addrs)
	}
	if n < 1 {
		n = 1
	}
	return &StorageClient{
		addrs:     append([]string(nil), addrs...),
		migrating: make(map[string]bool),
		client:    &http.Client{Timeout: 30 * time.Second},
		ring:      ring,
		rf:        n,
		w:         clampInt(opts.WriteQuorum, 1, n),
		r:         clampInt(opts.ReadQuorum, 1, n),
		nodeRes:   make(map[string]ResolutionsResponse),
	}
}

// snapshotAddrs returns a copy of the configured storage addresses under the membership
// lock, so a scatter fan-out reads a stable list even as the rebalancer adds or removes a
// node concurrently (ADR-031).
func (c *StorageClient) snapshotAddrs() []string {
	c.membMu.RLock()
	defer c.membMu.RUnlock()
	return append([]string(nil), c.addrs...)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ReplicationFactor reports the effective replication factor (N).
func (c *StorageClient) ReplicationFactor() int { return c.rf }

// WriteQuorum reports the effective write quorum (W).
func (c *StorageClient) WriteQuorum() int { return c.w }

// ReadQuorum reports the effective read quorum (R).
func (c *StorageClient) ReadQuorum() int { return c.r }

// Replicas returns the addresses of the live replicas that own the given series key,
// in ring order. It backs cluster-topology introspection and tests.
func (c *StorageClient) Replicas(name string, labels map[string]string) []string {
	nodes := c.ring.GetNodes(ringKey(name, labels), c.rf)
	addrs := make([]string, len(nodes))
	for i, n := range nodes {
		addrs[i] = n.Addr
	}
	return addrs
}

// RefreshHealth probes every storage node's /health once and updates its ring state so
// routing immediately excludes nodes that have gone away and re-includes ones that have
// returned. An unreachable node is marked Dead; a reachable one is resolved by
// applyReachable, which — when hinted handoff is on — routes a node returning with a
// backlog through the Joining (catching-up) state before Active (ADR-029), and otherwise
// just marks it Active (ADR-022). For a healthy node it also refreshes the cached rollup
// tier availability used by the resolution planner, so discovery rides the same cycle as
// liveness and never lags it. Probes run concurrently.
func (c *StorageClient) RefreshHealth() {
	var wg sync.WaitGroup
	for _, addr := range c.snapshotAddrs() {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			if _, ok := HealthCheck(addr); ok {
				c.applyReachable(addr)
				if res, ok := c.fetchResolutions(addr); ok {
					c.resMu.Lock()
					c.nodeRes[addr] = res
					c.resMu.Unlock()
				}
			} else {
				c.ring.SetState(addr, cluster.NodeDead)
			}
		}(addr)
	}
	wg.Wait()
}

// fetchResolutions reads a node's advertised rollup tier availability. A failure (node
// briefly unreachable, no rollup endpoint) leaves the last-known entry in place; the
// intersection in RollupResolutions simply ignores a live node it has not discovered yet.
func (c *StorageClient) fetchResolutions(addr string) (ResolutionsResponse, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://%s/api/internal/resolutions", addr)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return ResolutionsResponse{}, false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return ResolutionsResponse{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return ResolutionsResponse{}, false
	}
	var rr ResolutionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return ResolutionsResponse{}, false
	}
	return rr, true
}

// StartHealthMonitor runs RefreshHealth immediately and then every interval until ctx
// is cancelled, keeping ring node-state in sync with reachability.
func (c *StorageClient) StartHealthMonitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go func() {
		c.RefreshHealth()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.RefreshHealth()
			}
		}
	}()
}

// Addrs returns the configured storage node addresses (a snapshot, since membership can
// change at runtime via the rebalancer).
func (c *StorageClient) Addrs() []string {
	return c.snapshotAddrs()
}

// statusError drains resp.Body and returns a non-nil error when the response is not
// 200 OK. A storage node that returns 500 must surface as an error rather than being
// decoded into a zero-valued struct and reported as success-with-0.
func statusError(resp *http.Response, action string) error {
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("%s: unexpected status %d", action, resp.StatusCode)
	}
	return nil
}

// Write replicates each series to its N ring replicas and succeeds only when every
// series reaches its write quorum W. For a series key it computes the live replica set
// via the ring, batches the series destined for each node into one request, fans the
// batches out concurrently, and then verifies each series collected at least W acks.
// Fewer than W live replicas (or W acks) is a quorum failure surfaced as an error, not
// a silent partial write. The returned SamplesIngested counts logical samples that met
// quorum (once, not per replica). The request is all-or-nothing: if any series cannot
// reach quorum the whole call errors and nothing is sent.
//
// When hinted handoff is enabled (SetHintStore), a natural owner the write could not
// reach — it is down, catching up, or its write failed — has the series buffered as a
// durable hint and replayed on its return, so it fully converges (including an interior
// gap read-repair cannot fix) rather than staying stale until the next read. See ADR-029.
func (c *StorageClient) Write(ctx context.Context, req WriteRequest) (*WriteResponse, error) {
	if len(req.TimeSeries) == 0 {
		return &WriteResponse{}, nil
	}

	type seriesPlan struct {
		key      string
		samples  int
		replicas []string   // live replica addresses this series was sent to
		ts       TimeSeries // retained to hint a natural owner that missed the write
		pref     []string   // natural owner addresses (incl. down) for hint targeting; nil if handoff off
	}

	// Plan placement for every series first; bail before sending if any series lacks
	// a quorum of live replicas, so a quorum failure never leaves a partial write.
	plans := make([]seriesPlan, len(req.TimeSeries))
	perNode := make(map[string][]TimeSeries)
	for i, ts := range req.TimeSeries {
		key := ringKey(ts.Name, labelSliceToMap(ts.Labels))
		nodes := c.ring.GetNodes(key, c.rf)
		if len(nodes) < c.w {
			return nil, fmt.Errorf("write quorum unavailable for series %q: %d live replica(s) < W=%d", key, len(nodes), c.w)
		}
		addrs := make([]string, len(nodes))
		for j, n := range nodes {
			addrs[j] = n.Addr
			perNode[n.Addr] = append(perNode[n.Addr], ts)
		}
		plan := seriesPlan{key: key, samples: len(ts.Samples), replicas: addrs}
		// For hinted handoff, also record the natural owners (including any that are down
		// or catching up) so a write that quorum reached on the live set can still hint the
		// owners it missed. GetNodes already substituted live fallbacks for those owners.
		if c.hints != nil {
			plan.ts = ts
			pref := c.ring.PreferenceList(key, c.rf)
			pa := make([]string, len(pref))
			for j, n := range pref {
				pa[j] = n.Addr
			}
			plan.pref = pa
		}
		plans[i] = plan
	}

	// Fan out one batched write per target node, concurrently.
	type nodeResult struct {
		addr string
		ok   bool
	}
	results := make(chan nodeResult, len(perNode))
	for addr, series := range perNode {
		go func(addr string, series []TimeSeries) {
			results <- nodeResult{addr: addr, ok: c.writeToNode(ctx, addr, series)}
		}(addr, series)
	}
	okNodes := make(map[string]bool, len(perNode))
	for range perNode {
		nr := <-results
		if nr.ok {
			okNodes[nr.addr] = true
		}
	}

	// Require W acks per series.
	var ingested int64
	var hintsByTarget map[string][]TimeSeries
	for _, p := range plans {
		acks := 0
		for _, addr := range p.replicas {
			if okNodes[addr] {
				acks++
			}
		}
		if acks < c.w {
			return nil, fmt.Errorf("write quorum not met for series %q: %d/%d replica acks < W=%d", p.key, acks, len(p.replicas), c.w)
		}
		ingested += int64(p.samples)
		// Hinted handoff: any natural owner the write did not reach (it is down, catching
		// up, or its write failed) needs the series buffered so it converges on return.
		// A natural owner that acked is in okNodes; everything else in the preference list
		// is a missed owner. Collected now, flushed only after every series met quorum.
		for _, owner := range p.pref {
			if !okNodes[owner] {
				if hintsByTarget == nil {
					hintsByTarget = make(map[string][]TimeSeries)
				}
				hintsByTarget[owner] = append(hintsByTarget[owner], p.ts)
			}
		}
	}

	// Buffer hints only after confirming quorum for every series: the write is
	// all-or-nothing, so a quorum failure returns above and buffers nothing. Buffering
	// is best-effort — a failed hint write is logged, not surfaced, since the quorum
	// write already succeeded and the gap is recoverable later by read-repair. (ADR-029.)
	for target, series := range hintsByTarget {
		if err := c.hints.Add(target, series); err != nil {
			log.Printf("hinted handoff: buffering hint for %s failed: %v", target, err)
		}
	}

	return &WriteResponse{SamplesIngested: ingested}, nil
}

// writeToNode POSTs a batch of series to one storage node, returning whether the node
// acknowledged it (HTTP 200). The body is drained so the connection can be reused.
func (c *StorageClient) writeToNode(ctx context.Context, addr string, series []TimeSeries) bool {
	body, err := json.Marshal(WriteRequest{TimeSeries: series})
	if err != nil {
		return false
	}
	url := fmt.Sprintf("http://%s/api/internal/write", addr)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// Query serves a read from a quorum of replicas. Because a label-matcher query spans
// many series (each with its own ring replica set), it scatters to all live nodes —
// a superset of any matched series' replicas — and merges the responses, deduping by
// (series, timestamp). It then enforces a read quorum of R: globally (at least R live
// nodes must respond) and per returned series (at least R of that series' replicas
// must have responded); too few is a quorum failure surfaced as an error rather than
// silent partial data. Finally it asynchronously read-repairs: any responding replica
// missing points relative to the merged truth is sent those points. With W+R>N the
// read set overlaps every write set, giving read-your-writes.
func (c *StorageClient) Query(ctx context.Context, matchers []storage.LabelMatcher, start, end int64) (storage.SeriesSet, error) {
	live := c.ring.LiveNodes()
	if len(live) < c.r {
		return nil, fmt.Errorf("read quorum unavailable: %d live storage node(s) < R=%d", len(live), c.r)
	}

	matcherJSON := make([]MatcherJSON, len(matchers))
	for i, m := range matchers {
		matcherJSON[i] = StorageToMatcher(m)
	}
	body, err := json.Marshal(QueryRequest{Matchers: matcherJSON, Start: start, End: end})
	if err != nil {
		return nil, err
	}

	type nodeResp struct {
		addr string
		data []SeriesResult
		ok   bool
	}
	ch := make(chan nodeResp, len(live))
	for _, n := range live {
		go func(addr string) {
			data, _, ok := c.queryNode(ctx, addr, body)
			ch <- nodeResp{addr: addr, data: data, ok: ok}
		}(n.Addr)
	}

	// Merge responses into the canonical truth, and remember each node's per-series
	// points so we can diff them for read-repair.
	responded := make(map[string]bool, len(live))
	merged := make(map[string]*storage.ResultSeries)
	perNode := make(map[string]map[string][]storage.Point, len(live))
	for range live {
		nr := <-ch
		if !nr.ok {
			continue
		}
		responded[nr.addr] = true
		nodePoints := make(map[string][]storage.Point, len(nr.data))
		for _, sr := range nr.data {
			key := seriesKey(sr.Name, sr.Labels)
			points := make([]storage.Point, len(sr.Points))
			for i, p := range sr.Points {
				points[i] = storage.Point{Timestamp: p.Timestamp, Value: p.Value}
			}
			nodePoints[key] = points
			if existing, ok := merged[key]; ok {
				existing.Points = mergePoints(existing.Points, points)
			} else {
				merged[key] = &storage.ResultSeries{
					Name:   sr.Name,
					Labels: sr.Labels,
					Points: append([]storage.Point(nil), points...),
				}
			}
		}
		perNode[nr.addr] = nodePoints
	}

	if len(responded) < c.r {
		return nil, fmt.Errorf("read quorum not met: %d/%d live storage node(s) responded (R=%d)", len(responded), len(live), c.r)
	}

	// Per-series read quorum + read-repair planning. Repairs are batched per target so
	// a node receives one write regardless of how many series it must catch up on.
	repairs := make(map[string][]TimeSeries)
	for key, rs := range merged {
		replicas := c.ring.GetNodes(ringKey(rs.Name, rs.Labels), c.rf)
		respondedReplicas := 0
		for _, n := range replicas {
			if responded[n.Addr] {
				respondedReplicas++
			}
		}
		if respondedReplicas < c.r {
			return nil, fmt.Errorf("read quorum not met for series %q: %d responding replica(s) < R=%d", key, respondedReplicas, c.r)
		}
		for _, n := range replicas {
			nodePoints, ok := perNode[n.Addr]
			if !ok {
				continue // replica didn't respond; nothing read from it to diff against
			}
			if missing := pointsMissing(rs.Points, nodePoints[key]); len(missing) > 0 {
				repairs[n.Addr] = append(repairs[n.Addr], seriesToWrite(rs.Name, rs.Labels, missing))
			}
		}
	}
	if len(repairs) > 0 {
		go c.readRepair(repairs)
	}

	ss := make(storage.SeriesSet, 0, len(merged))
	for _, rs := range merged {
		sort.Slice(rs.Points, func(i, j int) bool {
			return rs.Points[i].Timestamp < rs.Points[j].Timestamp
		})
		ss = append(ss, *rs)
	}
	return ss, nil
}

// RollupResolutions reports the rollup resolutions (ms, ascending) the cluster can serve
// for query planning: the intersection of the live nodes' advertised tiers, so the planner
// only ever picks a resolution every discovered live node can serve. In steady state every
// storage node runs the same downsampling cascade, so the intersection is the full tier
// set; a node mid-downsample or just back from Dead narrows it, and the per-series merge in
// QueryResolution reconciles any residual skew. A live node whose tiers have not been
// discovered yet does not veto the intersection (the merge handles it). Implements
// query.ResolutionDataSource, the same capability *storage.TSDB exposes to the monolith.
func (c *StorageClient) RollupResolutions() []int64 {
	return c.intersectLiveResolutions(func(r ResolutionsResponse) []int64 { return r.Resolutions })
}

// RollupIncreaseResolutions reports the resolutions whose counter-increase column is
// complete across the cluster (the intersection over live nodes), i.e. the tiers from which
// rate() can be served. A tier where any live node lacks the column is excluded, so rate()
// falls back to raw rather than under-counting.
func (c *StorageClient) RollupIncreaseResolutions() []int64 {
	return c.intersectLiveResolutions(func(r ResolutionsResponse) []int64 { return r.IncreaseResolutions })
}

// intersectLiveResolutions returns the resolutions present on every discovered live node,
// per the selector pick. Live nodes whose availability is not yet cached are skipped rather
// than treated as having none, so incomplete discovery degrades to more per-series raw
// fallback (still correct) instead of forcing the whole cluster to raw.
func (c *StorageClient) intersectLiveResolutions(pick func(ResolutionsResponse) []int64) []int64 {
	live := c.ring.LiveNodes()
	c.resMu.RLock()
	defer c.resMu.RUnlock()
	counts := make(map[int64]int)
	known := 0
	for _, n := range live {
		rr, ok := c.nodeRes[n.Addr]
		if !ok {
			continue // tiers not discovered for this live node; don't let it veto
		}
		known++
		seen := make(map[int64]bool)
		for _, res := range pick(rr) {
			if res > 0 && !seen[res] {
				seen[res] = true
				counts[res]++
			}
		}
	}
	if known == 0 {
		return nil
	}
	out := make([]int64, 0, len(counts))
	for res, cnt := range counts {
		if cnt == known {
			out = append(out, res)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// QueryResolution serves a coarse read from a quorum of replicas: it asks every live node
// for the chosen resolution and aggregate column, then merges the responses by (series,
// window-timestamp). It is the cluster counterpart of TSDB.QueryResolution and the method
// that closes the asymmetry between the monolith and cluster query paths — the engine calls
// it (via query.ResolutionDataSource) exactly as it calls the monolith's TSDB.
//
// A resolution of 0 delegates to the raw Query, which keeps its read-repair. For a coarse
// read, read-repair is deliberately skipped: rollups are node-local derivations — each node
// downsamples its own raw data — so cross-node convergence is handled at the raw level
// (replication + read-repair). Repairing coarse windows across nodes would fight the local
// downsamplers and is unnecessary, since every replica regenerates its own rollups from its
// own converging raw. See ADR-011/ADR-022.
//
// Because a node missing the requested tier serves a finer one (reporting what it served),
// replicas of one series can disagree on resolution. Windows of different widths cannot be
// merged, so such a series falls back to a raw read (the convergence layer) for correctness;
// this is transient skew only and never returns mixed-width data. Quorum is enforced
// globally (≥R live nodes responded) and per series (≥R of its replicas responded), exactly
// as the raw path does.
func (c *StorageClient) QueryResolution(ctx context.Context, matchers []storage.LabelMatcher, start, end, resolution int64, agg storage.RollupAggregate) (storage.SeriesSet, error) {
	if resolution <= 0 {
		return c.Query(ctx, matchers, start, end)
	}

	live := c.ring.LiveNodes()
	if len(live) < c.r {
		return nil, fmt.Errorf("read quorum unavailable: %d live storage node(s) < R=%d", len(live), c.r)
	}

	matcherJSON := make([]MatcherJSON, len(matchers))
	for i, m := range matchers {
		matcherJSON[i] = StorageToMatcher(m)
	}
	body, err := json.Marshal(QueryRequest{
		Matchers:   matcherJSON,
		Start:      start,
		End:        end,
		Resolution: resolution,
		Aggregate:  AggregateToWire(agg),
	})
	if err != nil {
		return nil, err
	}

	type nodeResp struct {
		addr     string
		data     []SeriesResult
		servedMs int64
		ok       bool
	}
	ch := make(chan nodeResp, len(live))
	for _, n := range live {
		go func(addr string) {
			data, servedMs, ok := c.queryNode(ctx, addr, body)
			ch <- nodeResp{addr: addr, data: data, servedMs: servedMs, ok: ok}
		}(n.Addr)
	}

	// Gather each series' points keyed by the resolution the replica served, so a node
	// that served a finer tier than requested (or raw) is reconciled rather than blindly
	// interleaved with coarser windows. Replicas that served the same resolution merge by
	// timestamp — their windows are identical node-local derivations of the converged raw,
	// so the merge just dedups them (a stale replica contributes a subset a fresh one fills).
	responded := make(map[string]bool, len(live))
	byRes := make(map[string]map[int64]*storage.ResultSeries)
	for range live {
		nr := <-ch
		if !nr.ok {
			continue
		}
		responded[nr.addr] = true
		for _, sr := range nr.data {
			key := seriesKey(sr.Name, sr.Labels)
			points := make([]storage.Point, len(sr.Points))
			for i, p := range sr.Points {
				points[i] = storage.Point{Timestamp: p.Timestamp, Value: p.Value}
			}
			perRes := byRes[key]
			if perRes == nil {
				perRes = make(map[int64]*storage.ResultSeries)
				byRes[key] = perRes
			}
			if existing, ok := perRes[nr.servedMs]; ok {
				existing.Points = mergePoints(existing.Points, points)
			} else {
				perRes[nr.servedMs] = &storage.ResultSeries{
					Name:   sr.Name,
					Labels: sr.Labels,
					Points: append([]storage.Point(nil), points...),
				}
			}
		}
	}

	if len(responded) < c.r {
		return nil, fmt.Errorf("read quorum not met: %d/%d live storage node(s) responded (R=%d)", len(responded), len(live), c.r)
	}

	// Reconcile per series; a series whose replicas disagreed on resolution falls back to
	// raw. No read-repair on the coarse path (see the method doc).
	ss := make(storage.SeriesSet, 0, len(byRes))
	var mixed []storage.ResultSeries
	for key, perRes := range byRes {
		var sample *storage.ResultSeries
		for _, rs := range perRes {
			sample = rs
			break
		}
		replicas := c.ring.GetNodes(ringKey(sample.Name, sample.Labels), c.rf)
		respondedReplicas := 0
		for _, n := range replicas {
			if responded[n.Addr] {
				respondedReplicas++
			}
		}
		if respondedReplicas < c.r {
			return nil, fmt.Errorf("read quorum not met for series %q: %d responding replica(s) < R=%d", key, respondedReplicas, c.r)
		}

		if len(perRes) == 1 {
			sort.Slice(sample.Points, func(i, j int) bool { return sample.Points[i].Timestamp < sample.Points[j].Timestamp })
			ss = append(ss, *sample)
			continue
		}
		mixed = append(mixed, *sample) // replicas served different resolutions → raw fallback
	}

	if len(mixed) > 0 {
		rawSS, err := c.queryRawSeries(ctx, mixed, start, end)
		if err != nil {
			return nil, err
		}
		ss = append(ss, rawSS...)
	}

	return ss, nil
}

// queryRawSeries reads the given series from raw via the normal quorum raw path (which
// keeps read-repair), pinning each series with exact-match label matchers. It backs the
// per-series raw fallback when a coarse read found replicas that served different
// resolutions for a series; raw is the cross-node convergence layer, so this is always
// correct, just heavier, and only triggers during transient tier-availability skew.
func (c *StorageClient) queryRawSeries(ctx context.Context, series []storage.ResultSeries, start, end int64) (storage.SeriesSet, error) {
	var out storage.SeriesSet
	for _, s := range series {
		ss, err := c.Query(ctx, exactMatchers(s.Name, s.Labels), start, end)
		if err != nil {
			return nil, err
		}
		out = append(out, ss...)
	}
	return out, nil
}

// exactMatchers builds equality matchers that pin exactly one series by its name and full
// label set (the metric name is carried as __name__).
func exactMatchers(name string, labels map[string]string) []storage.LabelMatcher {
	matchers := make([]storage.LabelMatcher, 0, len(labels)+1)
	matchers = append(matchers, storage.LabelMatcher{Name: "__name__", Value: name, Type: storage.MatchEqual})
	for k, v := range labels {
		if k == "__name__" {
			continue
		}
		matchers = append(matchers, storage.LabelMatcher{Name: k, Value: v, Type: storage.MatchEqual})
	}
	return matchers
}

// queryNode runs the query against one storage node, returning its series, the resolution
// the node actually served (0 for a raw read), and whether the node responded successfully
// (HTTP 200, decodable body). The served resolution lets a coarse read reconcile replicas
// that served different tiers; the raw read path ignores it.
func (c *StorageClient) queryNode(ctx context.Context, addr string, body []byte) ([]SeriesResult, int64, bool) {
	url := fmt.Sprintf("http://%s/api/internal/query", addr)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, 0, false
	}
	var qr QueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		return nil, 0, false
	}
	return qr.Data, qr.ResolutionMs, true
}

// readRepair writes the missing points back to stale replicas in the background, under
// its own timeout so it outlives the originating request. Repairs are best-effort:
// failures are dropped and corrected on a later read. Because storage rejects
// out-of-order samples, a replica converges for points newer than its last — the usual
// case of a replica that missed a contiguous window while down. (Filling an interior
// gap needs hinted handoff; see ADR-022.)
func (c *StorageClient) readRepair(byAddr map[string][]TimeSeries) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for addr, series := range byAddr {
		wg.Add(1)
		go func(addr string, series []TimeSeries) {
			defer wg.Done()
			c.writeToNode(ctx, addr, series)
		}(addr, series)
	}
	wg.Wait()
}

// FetchBlocks retrieves block metadata from all storage nodes.
func (c *StorageClient) FetchBlocks(ctx context.Context) ([]BlockInfo, error) {
	type result struct {
		blocks []BlockInfo
		err    error
	}
	addrs := c.snapshotAddrs()
	results := make(chan result, len(addrs))
	for _, addr := range addrs {
		go func(addr string) {
			url := fmt.Sprintf("http://%s/api/internal/blocks", addr)
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			resp, err := c.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			if err := statusError(resp, "fetch blocks "+addr); err != nil {
				results <- result{err: err}
				return
			}
			var blocks []BlockInfo
			if err := json.NewDecoder(resp.Body).Decode(&blocks); err != nil {
				results <- result{err: err}
				return
			}
			results <- result{blocks: blocks}
		}(addr)
	}

	var all []BlockInfo
	for range addrs {
		r := <-results
		if r.err == nil {
			all = append(all, r.blocks...)
		}
	}
	return all, nil
}

// FetchStats retrieves stats from all storage nodes and aggregates them.
func (c *StorageClient) FetchStats(ctx context.Context) (*AggregatedStats, error) {
	type result struct {
		stats StatsResponse
		err   error
	}
	addrs := c.snapshotAddrs()
	results := make(chan result, len(addrs))
	for _, addr := range addrs {
		go func(addr string) {
			url := fmt.Sprintf("http://%s/api/internal/stats", addr)
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			resp, err := c.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			if err := statusError(resp, "fetch stats "+addr); err != nil {
				results <- result{err: err}
				return
			}
			var s StatsResponse
			if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
				results <- result{err: err}
				return
			}
			results <- result{stats: s}
		}(addr)
	}

	agg := &AggregatedStats{}
	for range addrs {
		r := <-results
		if r.err != nil {
			continue
		}
		agg.TotalSamples += r.stats.TotalSamples
		agg.TotalSeries += r.stats.TotalSeries
		agg.BlockCount += r.stats.BlockCount
		agg.StorageBytesRaw += r.stats.StorageBytesRaw
		agg.StorageBytesDisk += r.stats.StorageBytesDisk
		agg.HeadSamples += r.stats.HeadSamples
		agg.HeadSeries += r.stats.HeadSeries
		agg.WALSize += r.stats.WALSize
		agg.IngestionRate += r.stats.IngestionRate
	}
	return agg, nil
}

// AggregatedStats holds stats merged from all storage nodes.
type AggregatedStats struct {
	TotalSamples     int64
	TotalSeries      int
	BlockCount       int
	StorageBytesRaw  int64
	StorageBytesDisk int64
	HeadSamples      int64
	HeadSeries       int
	WALSize          int64
	// IngestionRate is the cluster-wide samples/sec rate: the sum of each storage
	// node's windowed rate (rates are additive across nodes).
	IngestionRate int64
}

// HealthCheck probes a service's /health endpoint.
func HealthCheck(addr string) (string, bool) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://%s/health", addr))
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", false
	}
	var h struct {
		NodeID string `json:"node_id"`
		Status string `json:"status"`
		Role   string `json:"role"`
	}
	json.NewDecoder(resp.Body).Decode(&h)
	return h.NodeID, h.Status == "ok"
}

// FetchSeries retrieves series metadata from all storage nodes.
func (c *StorageClient) FetchSeries(ctx context.Context) ([]SeriesInfo, error) {
	type result struct {
		series []SeriesInfo
		err    error
	}
	addrs := c.snapshotAddrs()
	results := make(chan result, len(addrs))
	for _, addr := range addrs {
		go func(addr string) {
			url := fmt.Sprintf("http://%s/api/internal/series", addr)
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			resp, err := c.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			if err := statusError(resp, "fetch series "+addr); err != nil {
				results <- result{err: err}
				return
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				results <- result{err: err}
				return
			}
			var wrapper struct {
				Data []SeriesInfo `json:"data"`
			}
			json.Unmarshal(body, &wrapper)
			results <- result{series: wrapper.Data}
		}(addr)
	}

	var all []SeriesInfo
	for range addrs {
		r := <-results
		if r.err == nil {
			all = append(all, r.series...)
		}
	}
	return all, nil
}

// SeriesInfo mirrors storage.SeriesInfo for JSON transport.
type SeriesInfo struct {
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels"`
	SampleCount int               `json:"samples_count"`
	LastValue   float64           `json:"last_value"`
	// LastTS is the most-recent sample timestamp (Unix ms), carried so the gateway
	// broadcast/anomaly path can dedup re-reads of a slow series.
	LastTS int64 `json:"last_ts"`
}

// FetchLabels retrieves label names from all storage nodes and deduplicates.
func (c *StorageClient) FetchLabels(ctx context.Context) ([]string, error) {
	type result struct {
		labels []string
		err    error
	}
	addrs := c.snapshotAddrs()
	results := make(chan result, len(addrs))
	for _, addr := range addrs {
		go func(addr string) {
			url := fmt.Sprintf("http://%s/api/internal/labels", addr)
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			resp, err := c.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			if err := statusError(resp, "fetch labels "+addr); err != nil {
				results <- result{err: err}
				return
			}
			var wrapper struct {
				Data []string `json:"data"`
			}
			json.NewDecoder(resp.Body).Decode(&wrapper)
			results <- result{labels: wrapper.Data}
		}(addr)
	}

	seen := make(map[string]bool)
	for range addrs {
		r := <-results
		if r.err == nil {
			for _, l := range r.labels {
				seen[l] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// FetchLabelValues retrieves values for a label from all storage nodes.
func (c *StorageClient) FetchLabelValues(ctx context.Context, name string) ([]string, error) {
	type result struct {
		values []string
		err    error
	}
	addrs := c.snapshotAddrs()
	results := make(chan result, len(addrs))
	for _, addr := range addrs {
		go func(addr string) {
			url := fmt.Sprintf("http://%s/api/internal/label/%s/values", addr, name)
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			resp, err := c.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			if err := statusError(resp, "fetch label values "+addr); err != nil {
				results <- result{err: err}
				return
			}
			var wrapper struct {
				Data []string `json:"data"`
			}
			json.NewDecoder(resp.Body).Decode(&wrapper)
			results <- result{values: wrapper.Data}
		}(addr)
	}

	seen := make(map[string]bool)
	for range addrs {
		r := <-results
		if r.err == nil {
			for _, v := range r.values {
				seen[v] = true
			}
		}
	}
	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}
	sort.Strings(values)
	return values, nil
}

// DeleteBlock tells a specific storage node to delete a block.
func (c *StorageClient) DeleteBlock(ctx context.Context, addr, ulid string) error {
	url := fmt.Sprintf("http://%s/api/internal/blocks/%s", addr, ulid)
	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("delete block %s on %s: status %d", ulid, addr, resp.StatusCode)
	}
	return nil
}

// ringKey is the consistent-hash key for a series. It excludes the synthetic
// "__name__" label (the name is carried separately, and storage reads echo it back in
// the label set) so the key a write computes from []Label and the key a read computes
// from the returned label map are identical for the same logical series.
func ringKey(name string, labels map[string]string) string {
	if _, ok := labels["__name__"]; ok {
		stripped := make(map[string]string, len(labels))
		for k, v := range labels {
			if k != "__name__" {
				stripped[k] = v
			}
		}
		labels = stripped
	}
	return cluster.MetricKey(name, labels)
}

// labelSliceToMap converts wire labels to a map, dropping any "__name__" entry so the
// result matches what ringKey strips on the read side.
func labelSliceToMap(labels []Label) map[string]string {
	m := make(map[string]string, len(labels))
	for _, l := range labels {
		if l.Name == "__name__" {
			continue
		}
		m[l.Name] = l.Value
	}
	return m
}

// pointsMissing returns the points in truth whose timestamp is absent from have. Both
// slices must be sorted ascending by timestamp (storage returns sorted points and
// mergePoints preserves order).
func pointsMissing(truth, have []storage.Point) []storage.Point {
	var missing []storage.Point
	i := 0
	for _, p := range truth {
		for i < len(have) && have[i].Timestamp < p.Timestamp {
			i++
		}
		if i < len(have) && have[i].Timestamp == p.Timestamp {
			continue
		}
		missing = append(missing, p)
	}
	return missing
}

// seriesToWrite packages a series' points into a wire TimeSeries for read-repair,
// dropping "__name__" from the labels (storage re-derives it from the name).
func seriesToWrite(name string, labels map[string]string, points []storage.Point) TimeSeries {
	lbls := make([]Label, 0, len(labels))
	for k, v := range labels {
		if k == "__name__" {
			continue
		}
		lbls = append(lbls, Label{Name: k, Value: v})
	}
	samples := make([]Sample, len(points))
	for i, p := range points {
		samples[i] = Sample{TimestampMs: p.Timestamp, Value: p.Value}
	}
	return TimeSeries{Name: name, Labels: lbls, Samples: samples}
}

func seriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	key := name + "{"
	for i, k := range keys {
		if i > 0 {
			key += ","
		}
		key += k + "=" + labels[k]
	}
	key += "}"
	return key
}

func mergePoints(a, b []storage.Point) []storage.Point {
	result := make([]storage.Point, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].Timestamp < b[j].Timestamp {
			result = append(result, a[i])
			i++
		} else if a[i].Timestamp > b[j].Timestamp {
			result = append(result, b[j])
			j++
		} else {
			result = append(result, a[i])
			i++
			j++
		}
	}
	result = append(result, a[i:]...)
	result = append(result, b[j:]...)
	return result
}

// LatencyTracker maintains a simple histogram of query latencies.
type LatencyTracker struct {
	mu      sync.Mutex
	buckets []LatencyBucket
}

// LatencyBucket represents a histogram bucket.
type LatencyBucket struct {
	LE    string `json:"le"`
	Count int64  `json:"count"`
}

// NewLatencyTracker creates a tracker with standard latency buckets.
func NewLatencyTracker() *LatencyTracker {
	return &LatencyTracker{
		buckets: []LatencyBucket{
			{LE: "1ms"}, {LE: "5ms"}, {LE: "10ms"}, {LE: "25ms"},
			{LE: "50ms"}, {LE: "100ms"}, {LE: "250ms"}, {LE: "500ms"}, {LE: "1s"},
		},
	}
}

// Record adds a latency observation to the histogram.
func (lt *LatencyTracker) Record(d time.Duration) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	ms := d.Milliseconds()
	thresholds := []int64{1, 5, 10, 25, 50, 100, 250, 500, 1000}
	for i, t := range thresholds {
		if ms <= t {
			lt.buckets[i].Count++
			return
		}
	}
	// Over 1s goes in last bucket
	lt.buckets[len(lt.buckets)-1].Count++
}

// Buckets returns a snapshot of the histogram.
func (lt *LatencyTracker) Buckets() []LatencyBucket {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	out := make([]LatencyBucket, len(lt.buckets))
	copy(out, lt.buckets)
	return out
}
