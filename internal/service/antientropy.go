package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/meridiandb/meridian/internal/cluster"
	"github.com/meridiandb/meridian/internal/storage"
)

// aeStats holds the anti-entropy counters (ADR-030). All are cumulative.
type aeStats struct {
	rounds    atomic.Int64 // replica-group comparisons run (a converged group still counts)
	divergent atomic.Int64 // (group × window) buckets found divergent
	repairs   atomic.Int64 // backfill pushes sent to converge a peer
	samples   atomic.Int64 // samples transferred by those pushes
	bytes     atomic.Int64 // bytes transferred (marshaled backfill bodies)
}

// AntiEntropyStats is a snapshot of the anti-entropy counters, for metrics.
type AntiEntropyStats struct {
	Rounds             int64
	DivergentWindows   int64
	Repairs            int64
	SamplesTransferred int64
	BytesTransferred   int64
}

// AntiEntropyStats returns a snapshot of the anti-entropy counters (ADR-030).
func (c *StorageClient) AntiEntropyStats() AntiEntropyStats {
	return AntiEntropyStats{
		Rounds:             c.ae.rounds.Load(),
		DivergentWindows:   c.ae.divergent.Load(),
		Repairs:            c.ae.repairs.Load(),
		SamplesTransferred: c.ae.samples.Load(),
		BytesTransferred:   c.ae.bytes.Load(),
	}
}

// AntiEntropyOptions configures the background anti-entropy sweep (ADR-030).
type AntiEntropyOptions struct {
	// Interval is the time between sweep rounds. <= 0 disables the sweep.
	Interval time.Duration
	// Window is the time-bucket size for digests. Smaller windows localise divergence
	// more finely (less data re-transferred) at the cost of larger digests.
	Window time.Duration
	// Lookback bounds how far back from now a round reconciles. 0 means all history;
	// a finite value bounds the per-round read cost on large datasets.
	Lookback time.Duration
	// Jitter is a random [0, Jitter) delay added to each interval so co-located
	// coordinators do not sweep in lockstep.
	Jitter time.Duration
	// GroupsPerRound caps how many replica groups a round reconciles, the spatial rate
	// limit; the round-robin cursor covers the rest over subsequent rounds.
	GroupsPerRound int
}

// StartAntiEntropy runs the background anti-entropy sweep until ctx is cancelled
// (ADR-030): each round reconciles up to GroupsPerRound replica groups, advancing a
// round-robin cursor so the whole ring is covered over successive rounds. It is a no-op
// when disabled (Interval <= 0) or the cluster is not replicated (N < 2), since there is
// no peer to converge against.
func (c *StorageClient) StartAntiEntropy(ctx context.Context, opts AntiEntropyOptions) {
	if opts.Interval <= 0 || c.rf < 2 {
		return
	}
	go func() {
		timer := time.NewTimer(jitteredInterval(opts.Interval, opts.Jitter))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				c.AntiEntropyRound(ctx, opts)
				timer.Reset(jitteredInterval(opts.Interval, opts.Jitter))
			}
		}
	}()
}

func jitteredInterval(interval, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return interval
	}
	return interval + time.Duration(rand.Int63n(int64(jitter)))
}

// AntiEntropyRound runs one sweep round: it reconciles up to GroupsPerRound replica
// groups starting at the round-robin cursor, advancing the cursor so the next round
// continues where this one left off. Exposed (not just the loop) so tests can drive a
// round deterministically. Not safe to call concurrently with itself or the loop — both
// own the cursor.
func (c *StorageClient) AntiEntropyRound(ctx context.Context, opts AntiEntropyOptions) {
	groups := c.ring.ReplicaGroups(c.rf)
	if len(groups) == 0 {
		return
	}

	window := opts.Window.Milliseconds()
	if window <= 0 {
		window = time.Hour.Milliseconds()
	}
	end := time.Now().UnixMilli()
	start := int64(0)
	if opts.Lookback > 0 {
		start = end - opts.Lookback.Milliseconds()
	}

	n := opts.GroupsPerRound
	if n < 1 {
		n = 1
	}
	for i := 0; i < n && i < len(groups); i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		g := groups[c.aeCursor%len(groups)]
		c.aeCursor = (c.aeCursor + 1) % len(groups)
		c.reconcileGroup(ctx, g, start, end, window)
	}
}

// reconcileGroup compares the live replicas of one group's hash arcs and repairs any
// divergent time window. It fetches a digest per replica, localises the windows whose
// leaves disagree, and reconciles each — so a group whose replicas already agree
// transfers nothing.
func (c *StorageClient) reconcileGroup(ctx context.Context, g cluster.ReplicaGroup, start, end, window int64) {
	// Reconcile only replicas serving live traffic: a Dead one is unreachable and a
	// Joining one is already catching up via hinted handoff (ADR-029).
	reachable := make([]string, 0, len(g.Replicas))
	for _, id := range g.Replicas {
		if st, ok := c.ring.State(id); ok && st == cluster.NodeActive {
			reachable = append(reachable, id)
		}
	}
	if len(reachable) < 2 {
		return
	}
	arcs := arcsToWire(g.Arcs)

	digests := make(map[string]storage.MerkleDigest, len(reachable))
	for _, addr := range reachable {
		if d, ok := c.fetchDigest(ctx, addr, arcs, start, end, window); ok {
			digests[addr] = d
		}
	}
	if len(digests) < 2 {
		return
	}
	c.ae.rounds.Add(1)

	// Localise divergence to specific windows: gather each replica's leaf hash per
	// window start; a window not reported with the same hash by every replica diverges.
	leafByWindow := make(map[int64]map[string]string)
	for addr, d := range digests {
		for _, l := range d.Leaves {
			m := leafByWindow[l.Start]
			if m == nil {
				m = make(map[string]string)
				leafByWindow[l.Start] = m
			}
			m[addr] = l.Hash
		}
	}
	divergent := make([]int64, 0)
	for ws, perAddr := range leafByWindow {
		if windowDivergent(perAddr, len(digests)) {
			divergent = append(divergent, ws)
		}
	}
	if len(divergent) == 0 {
		return // converged
	}
	sort.Slice(divergent, func(i, j int) bool { return divergent[i] < divergent[j] })

	for _, ws := range divergent {
		select {
		case <-ctx.Done():
			return
		default:
		}
		c.ae.divergent.Add(1)
		ws0 := ws
		if ws0 < start {
			ws0 = start
		}
		we := ws + window - 1
		if we > end {
			we = end
		}
		c.reconcileWindow(ctx, reachable, arcs, ws0, we)
	}
}

// windowDivergent reports whether a window's per-replica leaf hashes disagree: a missing
// replica (fewer than the digest count) or two distinct hashes both count as divergent.
func windowDivergent(perAddr map[string]string, replicas int) bool {
	if len(perAddr) < replicas {
		return true
	}
	first, set := "", false
	for _, h := range perAddr {
		if !set {
			first, set = h, true
		} else if h != first {
			return true
		}
	}
	return false
}

// reconcileWindow reads one divergent window from each replica, builds the union of
// (series, timestamp) → value, and pushes to each replica only the points it is missing
// — genuine gaps, never an overwrite (backfill is gap-fill, ADR-029). Bidirectional
// missing-fill converges every replica to the union of the window in a single pass.
func (c *StorageClient) reconcileWindow(ctx context.Context, replicas []string, arcs [][2]uint64, start, end int64) {
	perReplica := make(map[string][]TimeSeries, len(replicas))
	for _, addr := range replicas {
		if ts, ok := c.fetchRange(ctx, addr, arcs, start, end); ok {
			perReplica[addr] = ts
		}
	}
	if len(perReplica) < 2 {
		return
	}

	type seriesData struct {
		name   string
		labels []Label
		points map[int64]float64
	}
	union := make(map[string]*seriesData)
	have := make(map[string]map[string]map[int64]bool) // addr -> seriesKey -> timestamp set
	for addr, list := range perReplica {
		hv := make(map[string]map[int64]bool)
		have[addr] = hv
		for _, ts := range list {
			key := wireSeriesKey(ts)
			sd := union[key]
			if sd == nil {
				sd = &seriesData{name: ts.Name, labels: ts.Labels, points: make(map[int64]float64)}
				union[key] = sd
			}
			hset := hv[key]
			if hset == nil {
				hset = make(map[int64]bool)
				hv[key] = hset
			}
			for _, s := range ts.Samples {
				if _, ok := sd.points[s.TimestampMs]; !ok {
					sd.points[s.TimestampMs] = s.Value
				}
				hset[s.TimestampMs] = true
			}
		}
	}

	for _, addr := range replicas {
		hv, ok := have[addr]
		if !ok {
			continue // did not answer the range read
		}
		var missing []TimeSeries
		var nsamples int64
		for key, sd := range union {
			hset := hv[key]
			var samples []Sample
			for ts, val := range sd.points {
				if hset == nil || !hset[ts] {
					samples = append(samples, Sample{TimestampMs: ts, Value: val})
				}
			}
			if len(samples) > 0 {
				sort.Slice(samples, func(i, j int) bool { return samples[i].TimestampMs < samples[j].TimestampMs })
				missing = append(missing, TimeSeries{Name: sd.name, Labels: sd.labels, Samples: samples})
				nsamples += int64(len(samples))
			}
		}
		if len(missing) == 0 {
			continue
		}
		body, err := json.Marshal(WriteRequest{TimeSeries: missing})
		if err != nil {
			continue
		}
		if c.postBackfill(ctx, addr, body) {
			c.ae.repairs.Add(1)
			c.ae.samples.Add(nsamples)
			c.ae.bytes.Add(int64(len(body)))
		}
	}
}

// wireSeriesKey builds a stable "name{sorted_labels}" key for a wire series so the same
// series read from different replicas aligns when building the union. It mirrors the
// storage seriesKey / cluster.MetricKey format; it is only used to align samples in
// memory, so it needs to be internally consistent rather than byte-identical to either.
func wireSeriesKey(ts TimeSeries) string {
	if len(ts.Labels) == 0 {
		return ts.Name
	}
	labels := append([]Label(nil), ts.Labels...)
	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
	var sb strings.Builder
	sb.WriteString(ts.Name)
	sb.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(l.Name)
		sb.WriteByte('=')
		sb.WriteString(l.Value)
	}
	sb.WriteByte('}')
	return sb.String()
}

func arcsToWire(arcs []cluster.HashArc) [][2]uint64 {
	out := make([][2]uint64, len(arcs))
	for i, a := range arcs {
		out[i] = [2]uint64{a.Start, a.End}
	}
	return out
}

// fetchDigest fetches a peer's Merkle range digest over the given arcs and span. Any
// failure (unreachable, decode error) drops the peer from this round's comparison.
func (c *StorageClient) fetchDigest(ctx context.Context, addr string, ranges [][2]uint64, start, end, window int64) (storage.MerkleDigest, bool) {
	body, err := json.Marshal(DigestRequest{Ranges: ranges, Start: start, End: end, Window: window})
	if err != nil {
		return storage.MerkleDigest{}, false
	}
	resp, ok := c.postInternal(ctx, addr, "/api/internal/antientropy/digest", body)
	if !ok {
		return storage.MerkleDigest{}, false
	}
	defer resp.Body.Close()
	var d storage.MerkleDigest
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return storage.MerkleDigest{}, false
	}
	return d, true
}

// fetchRange reads a peer's raw samples over the given arcs and span as wire series.
func (c *StorageClient) fetchRange(ctx context.Context, addr string, ranges [][2]uint64, start, end int64) ([]TimeSeries, bool) {
	body, err := json.Marshal(RangeRequest{Ranges: ranges, Start: start, End: end})
	if err != nil {
		return nil, false
	}
	resp, ok := c.postInternal(ctx, addr, "/api/internal/antientropy/range", body)
	if !ok {
		return nil, false
	}
	defer resp.Body.Close()
	var wr WriteRequest
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		return nil, false
	}
	return wr.TimeSeries, true
}

// postInternal POSTs a JSON body to a peer's internal endpoint, returning the response
// for the caller to decode (and close) on HTTP 200, or false otherwise.
func (c *StorageClient) postInternal(ctx context.Context, addr, path string, body []byte) (*http.Response, bool) {
	url := fmt.Sprintf("http://%s%s", addr, path)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, false
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, false
	}
	return resp, true
}
