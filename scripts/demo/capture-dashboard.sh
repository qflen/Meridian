#!/usr/bin/env bash
# Auto-capture the dashboard hero GIF (docs/assets/dashboard.gif).
#
# Boots a real single-binary Meridian node with the metric simulator, drives the
# dashboard headlessly with Playwright (record-dashboard.mjs), and renders the
# recording to an optimized looping GIF with ffmpeg + gifsicle. No manual
# recording. Re-run with SKIP_RECORD=1 to only re-render the GIF from the last
# capture (fast iteration on size/quality).
#
# Requires: go, node/npm (Playwright installed on demand here), ffmpeg, gifsicle.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DEMO="$ROOT/scripts/demo"
WORK="${WORK:-/tmp/meridian-demo/dash}"
VIDEO_DIR="$WORK/video"
DATA_DIR="$WORK/data"
CFG="$WORK/meridian.demo.yaml"
OUT="$ROOT/docs/assets/dashboard.gif"

# GIF render knobs (tunable for size/quality). Defaults land ~2.7 MB at a
# legible 780px for the scroll-heavy capture; bump GIF_WIDTH/FPS for a crisper,
# larger asset.
FPS="${FPS:-10}"
GIF_WIDTH="${GIF_WIDTH:-780}"
TRIM_START="${TRIM_START:-1.6}"
MAX_DUR="${MAX_DUR:-19}"
LOSSY="${LOSSY:-110}"
COLORS="${COLORS:-160}"

HTTP_ADDR="127.0.0.1:8080"
GRPC_ADDR="127.0.0.1:9090"

SERVE_PID=""; SIM_PID=""
cleanup() {
  [ -n "$SIM_PID" ] && kill "$SIM_PID" 2>/dev/null || true
  [ -n "$SERVE_PID" ] && kill "$SERVE_PID" 2>/dev/null || true
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

log() { printf '\033[0;34m[capture-dashboard]\033[0m %s\n' "$*"; }

mkdir -p "$WORK" "$VIDEO_DIR" "$DATA_DIR" "$ROOT/docs/assets"

if [ "${SKIP_RECORD:-0}" != "1" ]; then
  rm -rf "$VIDEO_DIR" "$DATA_DIR"; mkdir -p "$VIDEO_DIR" "$DATA_DIR"

  if [ "${SKIP_BUILD:-0}" != "1" ]; then
    log "Building meridian binary + dashboard…"
    (cd "$ROOT" && go build -o bin/meridian ./cmd/meridian)
    (cd "$ROOT/dashboard" && npx vite build --outDir dist >/dev/null 2>&1)
  fi

  # Demo config: shorten the anomaly warmup and lower the threshold a touch so the
  # detector is warm and the live anomaly strip lights up within the short capture
  # window. Everything else falls back to repo defaults (config merges onto them).
  cat > "$CFG" <<YAML
storage:
  data_dir: "$DATA_DIR"
  wal_dir: "$DATA_DIR/wal"
  block_duration: "2h"
anomaly:
  enabled: true
  threshold: 2.6
  warmup: 8
  debounce_k: 1
  mode: ewma
YAML

  log "Starting meridian serve…"
  (cd "$ROOT" && ./bin/meridian serve --config "$CFG" --http-listen "$HTTP_ADDR" --ingestion-listen "$GRPC_ADDR") \
    > "$WORK/serve.log" 2>&1 &
  SERVE_PID=$!

  for _ in $(seq 1 40); do
    curl -sf "http://$HTTP_ADDR/health" >/dev/null 2>&1 && break
    sleep 0.25
  done
  log "Server healthy. Starting simulator (fast cadence for a lively stream + spikes)…"
  (cd "$ROOT" && ./bin/meridian simulate --addr "$GRPC_ADDR" --hosts 8 --rate 250ms) \
    > "$WORK/sim.log" 2>&1 &
  SIM_PID=$!

  log "Pre-warming (filling charts + warming the anomaly detector)…"
  sleep 16

  if [ ! -d "$DEMO/node_modules/playwright" ]; then
    log "Installing Playwright (local, one-time)…"
    (cd "$DEMO" && npm install --no-audit --no-fund >/dev/null 2>&1)
  fi

  log "Recording dashboard with Playwright…"
  ( cd "$DEMO" && BASE_URL="http://$HTTP_ADDR" OUT_DIR="$VIDEO_DIR" \
      node record-dashboard.mjs )

  cleanup; SERVE_PID=""; SIM_PID=""
fi

WEBM="$(ls -t "$VIDEO_DIR"/*.webm 2>/dev/null | head -1 || true)"
[ -z "$WEBM" ] && { echo "no recording found in $VIDEO_DIR" >&2; exit 1; }
log "Rendering GIF from $(basename "$WEBM") (fps=$FPS width=$GIF_WIDTH)…"

PALETTE="$WORK/palette.png"
RAW_GIF="$WORK/dashboard.raw.gif"
FILTERS="fps=$FPS,scale=$GIF_WIDTH:-1:flags=lanczos"

ffmpeg -y -hide_banner -loglevel error -ss "$TRIM_START" -t "$MAX_DUR" -i "$WEBM" \
  -vf "$FILTERS,palettegen=stats_mode=diff" "$PALETTE"
ffmpeg -y -hide_banner -loglevel error -ss "$TRIM_START" -t "$MAX_DUR" -i "$WEBM" -i "$PALETTE" \
  -lavfi "$FILTERS[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=3:diff_mode=rectangle" "$RAW_GIF"

gifsicle -O3 --loopcount=0 --lossy="$LOSSY" --colors "$COLORS" "$RAW_GIF" -o "$OUT"

log "Wrote $OUT ($(du -h "$OUT" | cut -f1))"
