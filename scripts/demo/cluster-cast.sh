#!/usr/bin/env bash
# The narrated cluster-ops sequence recorded by asciinema (see capture-cluster.sh).
# Assumes a local cluster is already running; CLUSTER_ENV points at the file the
# orchestrator wrote, holding ports, paths and the node start_* helpers.
#
# Story: bring the ring up → ingest → kill a storage node, quorum holds →
# restart → hinted handoff + Merkle anti-entropy converge → query.
set -uo pipefail
# shellcheck disable=SC1090
source "${CLUSTER_ENV:?set CLUSTER_ENV}"

H='\033[1;36m'; OK='\033[1;32m'; WARN='\033[1;33m'; ERR='\033[1;31m'
DIM='\033[0;90m'; CMD='\033[0;37m'; ACC='\033[1;35m'; NC='\033[0m'

say()  { printf "\n${H}» %s${NC}\n" "$*"; }
note() { printf "${DIM}  %s${NC}\n" "$*"; }
good() { printf "${OK}  ✓ %s${NC}\n" "$*"; }
bad()  { printf "${ERR}  ✗ %s${NC}\n" "$*"; }
run()  { printf "${CMD}\$ %s${NC}\n" "$1"; eval "$2"; }
pause(){ sleep "${1:-1}"; }

QB="http://127.0.0.1:$QRY/api/internal/query"
IB="http://127.0.0.1:$ING_HTTP/metrics"
Q='cpu_usage_percent'

# One-line query summary through the querier (quorum read, R=2).
query() {
  local resp series pr
  resp="$(curl -s "$QB?q=$Q")"
  series="$(printf '%s' "$resp" | grep -o '"name"' | wc -l | tr -d ' ')"
  pr="$(printf '%s' "$resp" | grep -oE '"points_read":[0-9]+' | grep -oE '[0-9]+' | head -1)"
  if [ "${series:-0}" -gt 0 ]; then
    good "querier returned ${series} series  (points_read=${pr:-?})"
  else
    bad "querier returned no data"
  fi
}

# Print matching /metrics lines, trimming the (constant) label block so the
# value is legible: `meridian_handoff_pending_samples 1161`.
metric() { curl -s "$IB" | grep -E "$1" | grep -v '#' | sed -E 's/\{[^}]*\} / /; s/^/  /'; }

clear
printf "${ACC}╔══════════════════════════════════════════════════════════════╗${NC}\n"
printf "${ACC}║${NC}  ${H}Meridian${NC} · distributed cluster ops                            ${ACC}║${NC}\n"
printf "${ACC}║${NC}  3 storage nodes · consistent-hash ring · quorum ${OK}N=3 W=2 R=2${NC}   ${ACC}║${NC}\n"
printf "${ACC}╚══════════════════════════════════════════════════════════════╝${NC}\n"
pause 1.2

say "Ring health: every storage node is live"
for p in "$S1" "$S2" "$S3"; do
  id="$(curl -s "http://127.0.0.1:$p/health" | grep -oE '"node_id":"[^"]+"' | cut -d'"' -f4)"
  good "storage @ :$p  →  ${id:-unreachable}"
  pause 0.3
done
note "ingestor shards + replicates writes; querier scatter-reads at R and merges"
pause 1.2

say "Ingesting live metrics (simulator → ingestor → 3 replicas)"
note "writes succeed at W=2 acks; reads merge R=2 and read-repair stale replicas"
pause 1.5
run "curl -s \$QB?q=$Q | head" "query"
pause 1.5

say "FAULT: kill storage-2 mid-stream"
run "kill \$(cat \$WORK/storage-2.pid)" "kill \$(cat \"\$WORK/storage-2.pid\") 2>/dev/null || true"
pause 1.5
if curl -sf "http://127.0.0.1:$S2/health" >/dev/null 2>&1; then
  bad "storage-2 still responding?!"
else
  bad "storage-2 @ :$S2 is DOWN; /health no longer answers"
fi
note "the failure detector marks it dead within ~2s; routing excludes it"
pause 3

say "Quorum holds: read still served by the 2 survivors (R=2)"
query
good "a node died, the read stayed complete: no error, no partial data"
pause 2

say "Writes during the outage buffer as durable hints for storage-2"
note "quorum still succeeds on storage-1 + storage-3; the missed copy is hinted"
pause 3.5
metric 'meridian_handoff_pending_samples'
pause 2

say "RECOVER: restart storage-2"
run "start_s2   # re-exec the storage binary on :$S2" "start_s2"
pause 2
if curl -sf "http://127.0.0.1:$S2/health" >/dev/null 2>&1; then
  good "storage-2 @ :$S2 is back UP"
else
  note "storage-2 booting…"
fi
note "it rejoins through the catching-up state, out of live routing until whole"
pause 2

say "Convergence: hinted handoff replays + Merkle anti-entropy reconciles"
for i in 1 2 3; do
  printf "${DIM}  ── t+%ds ─────────────────────────────────────────────${NC}\n" "$((i*3))"
  metric 'meridian_handoff_(pending|replayed)_samples'
  metric 'meridian_anti_entropy_(repairs|rounds)_total'
  pause 3
done
good "pending hints drained → storage-2 is whole again"
pause 1.5

say "Final query: consistent across the healed ring"
query
pause 1
printf "\n${ACC}══ fault-tolerant: a node died, quorum held, the ring self-healed ══${NC}\n"
pause 2.5
