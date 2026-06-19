#!/usr/bin/env bash
# Auto-capture the cluster-ops GIF (docs/assets/cluster.gif).
#
# Boots a real local Meridian cluster from the actual service binaries - 3 storage
# nodes + an ingestor + a querier (no Docker) - drives a metric stream into it,
# records the narrated fault-tolerance sequence (cluster-cast.sh) with asciinema,
# and renders it to an optimized looping GIF with agg + gifsicle. No manual typing.
# Re-run with SKIP_REC=1 to only re-render the GIF from the last cast.
#
# Requires: go, asciinema, agg, gifsicle.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DEMO="$ROOT/scripts/demo"
WORK="${WORK:-/tmp/meridian-demo/cluster}"
CAST="$WORK/cluster.cast"
ENVF="$WORK/cluster.env"
OUT="$ROOT/docs/assets/cluster.gif"

# Ports (loopback only).
S1=7201; S2=7202; S3=7203; ING_HTTP=7210; ING_TCP=7290; QRY=7220

# agg render knobs.
SPEED="${SPEED:-1.5}"
IDLE="${IDLE:-1.4}"
THEME="${THEME:-monokai}"
FONT="${FONT:-Menlo,Monaco,DejaVu Sans Mono,monospace}"
LOSSY="${LOSSY:-60}"
COLORS="${COLORS:-200}"

log() { printf '\033[0;34m[capture-cluster]\033[0m %s\n' "$*"; }

PORTS="$S1 $S2 $S3 $ING_HTTP $ING_TCP $QRY"
# Free the demo ports. Node data dirs are passed via env (not argv), so a path
# match can't find these processes - kill by listening port and by binary path.
free_ports() { for p in $PORTS; do lsof -ti tcp:"$p" 2>/dev/null | xargs kill -9 2>/dev/null || true; done; }

cleanup() {
  if [ -d "$WORK" ]; then for f in "$WORK"/*.pid; do [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null || true; done; fi
  pkill -f 'bin/storage-svc' 2>/dev/null || true
  pkill -f 'bin/ingestor' 2>/dev/null || true
  pkill -f 'bin/querier' 2>/dev/null || true
  free_ports
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

if [ "${SKIP_REC:-0}" != "1" ]; then
  free_ports
  rm -rf "$WORK"; mkdir -p "$WORK" "$ROOT/docs/assets"

  if [ "${SKIP_BUILD:-0}" != "1" ]; then
    log "Building service binaries…"
    (cd "$ROOT" && go build -o bin/storage-svc ./cmd/storage \
      && go build -o bin/ingestor ./cmd/ingestor \
      && go build -o bin/querier ./cmd/querier \
      && go build -o bin/meridian ./cmd/meridian)
  fi

  # Shared env + node launchers, sourced by both this orchestrator and the cast
  # (so the cast can restart storage-2 with the identical command).
  cat > "$ENVF" <<ENV
ROOT="$ROOT"; WORK="$WORK"
S1=$S1; S2=$S2; S3=$S3; ING_HTTP=$ING_HTTP; ING_TCP=$ING_TCP; QRY=$QRY
STORAGE_ADDRS="127.0.0.1:$S1,127.0.0.1:$S2,127.0.0.1:$S3"

start_storage() { # <port> <id>
  STORAGE_HTTP_ADDR=":\$1" STORAGE_DATA_DIR="$WORK/\$2" STORAGE_NODE_ID="\$2" \
  STORAGE_FLUSH_INTERVAL="30s" STORAGE_DOWNSAMPLING_ENABLED="false" \
    "$ROOT/bin/storage-svc" > "$WORK/\$2.log" 2>&1 &
  echo \$! > "$WORK/\$2.pid"
}
start_s1() { start_storage $S1 storage-1; }
start_s2() { start_storage $S2 storage-2; }
start_s3() { start_storage $S3 storage-3; }

start_ingestor() {
  INGESTOR_HTTP_ADDR=":$ING_HTTP" INGESTOR_TCP_ADDR=":$ING_TCP" INGESTOR_NODE_ID="ingestor-1" \
  INGESTOR_DATA_DIR="$WORK/ingestor" STORAGE_ADDRS="\$STORAGE_ADDRS" \
  HINTED_HANDOFF_ENABLED="true" HINT_REPLAY_INTERVAL="2s" \
  ANTI_ENTROPY_ENABLED="true" ANTI_ENTROPY_INTERVAL="4s" ANTI_ENTROPY_JITTER="1s" ANTI_ENTROPY_WINDOW="1h" \
    "$ROOT/bin/ingestor" > "$WORK/ingestor.log" 2>&1 &
  echo \$! > "$WORK/ingestor.pid"
}

start_querier() {
  QUERIER_HTTP_ADDR=":$QRY" QUERIER_NODE_ID="querier-1" STORAGE_ADDRS="\$STORAGE_ADDRS" \
    "$ROOT/bin/querier" > "$WORK/querier.log" 2>&1 &
  echo \$! > "$WORK/querier.pid"
}
ENV

  # shellcheck disable=SC1090
  source "$ENVF"

  log "Starting 3 storage nodes…"; start_s1; start_s2; start_s3
  log "Starting ingestor + querier…"; start_ingestor; start_querier

  log "Waiting for the ring to be healthy…"
  for p in "$S1" "$S2" "$S3" "$ING_HTTP" "$QRY"; do
    for _ in $(seq 1 40); do
      curl -sf "http://127.0.0.1:$p/health" >/dev/null 2>&1 && break
      sleep 0.25
    done
  done

  log "Streaming metrics into the cluster (simulator → ingestor :$ING_TCP)…"
  (cd "$ROOT" && ./bin/meridian simulate --addr "127.0.0.1:$ING_TCP" --hosts 8 --rate 250ms) \
    > "$WORK/sim.log" 2>&1 &
  echo $! > "$WORK/sim.pid"

  log "Letting data accumulate + anti-entropy settle…"
  sleep 8

  command -v asciinema >/dev/null || { echo "asciinema not found" >&2; exit 1; }
  export CLUSTER_ENV="$ENVF"
  log "Recording cluster cast with asciinema…"
  asciinema rec --overwrite --window-size 100x28 -c "bash $DEMO/cluster-cast.sh" "$CAST"

  log "Stopping simulator + nodes…"; cleanup
fi

[ -f "$CAST" ] || { echo "no cast at $CAST" >&2; exit 1; }
command -v agg >/dev/null || { echo "agg not found" >&2; exit 1; }

RAW_GIF="$WORK/cluster.raw.gif"
log "Rendering cast → GIF with agg (speed=$SPEED idle≤${IDLE}s theme=$THEME)…"
agg --speed "$SPEED" --idle-time-limit "$IDLE" --theme "$THEME" --font-family "$FONT" \
  "$CAST" "$RAW_GIF"

log "Optimizing with gifsicle…"
gifsicle -O3 --loopcount=0 --lossy="$LOSSY" --colors "$COLORS" "$RAW_GIF" -o "$OUT"
log "Wrote $OUT ($(du -h "$OUT" | cut -f1))"
