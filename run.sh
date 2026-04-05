#!/usr/bin/env bash
set -euo pipefail

# ── Meridian TSDB – Quick Start ──────────────────────────────────────
# Usage: ./run.sh [demo|serve|simulate|query|bench|clean]
#
# demo     – build, start server, launch simulator, open dashboard
# serve    – start the server only
# simulate – run the simulator against a running server
# query    – run a sample query
# bench    – run compression benchmarks
# clean    – remove build artifacts and data

ROOT="$(cd "$(dirname "$0")" && pwd)"
BIN="$ROOT/bin/meridian"
DATA="$ROOT/data"
PID_FILE="$ROOT/.meridian.pid"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log()  { echo -e "${BLUE}[meridian]${NC} $*"; }
ok()   { echo -e "${GREEN}[✓]${NC} $*"; }
warn() { echo -e "${YELLOW}[!]${NC} $*"; }
err()  { echo -e "${RED}[✗]${NC} $*" >&2; }

build_go() {
  log "Building Go binary..."
  (cd "$ROOT" && go build -o "$BIN" ./cmd/meridian)
  ok "Built $BIN"
}

build_dashboard() {
  if [ ! -d "$ROOT/dashboard/node_modules" ]; then
    log "Installing dashboard dependencies..."
    (cd "$ROOT/dashboard" && npm install --no-audit --no-fund)
  fi
  log "Building dashboard..."
  (cd "$ROOT/dashboard" && npx vite build --outDir dist)
  ok "Dashboard built"
}

start_server() {
  if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    warn "Server already running (PID $(cat "$PID_FILE"))"
    return
  fi
  log "Starting Meridian server..."
  mkdir -p "$DATA"
  "$BIN" serve &
  echo $! > "$PID_FILE"
  sleep 1
  if kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    ok "Server started (PID $(cat "$PID_FILE"))"
  else
    err "Server failed to start"
    exit 1
  fi
}

stop_server() {
  if [ -f "$PID_FILE" ]; then
    local pid
    pid=$(cat "$PID_FILE")
    if kill -0 "$pid" 2>/dev/null; then
      log "Stopping server (PID $pid)..."
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      ok "Server stopped"
    fi
    rm -f "$PID_FILE"
  fi
}

wait_for_server() {
  log "Waiting for server to be ready..."
  for i in $(seq 1 30); do
    if curl -sf http://localhost:8080/health > /dev/null 2>&1; then
      ok "Server is ready"
      return
    fi
    sleep 0.5
  done
  err "Server did not become ready in time"
  exit 1
}

cmd_demo() {
  log "═══════════════════════════════════════════════"
  log "  Meridian TSDB – Demo Mode"
  log "═══════════════════════════════════════════════"
  echo

  build_go
  build_dashboard

  # Trap to clean up on exit
  trap 'stop_server; exit 0' INT TERM

  start_server
  wait_for_server

  log "Starting simulator (8 hosts, 43 metrics)..."
  "$BIN" simulate &
  SIM_PID=$!

  echo
  ok "Dashboard: http://localhost:8080"
  ok "API:       http://localhost:8080/api/query?q=cpu_usage_percent"
  ok "Health:    http://localhost:8080/health"
  echo
  log "Press Ctrl+C to stop"

  # Open browser on macOS
  if command -v open &>/dev/null; then
    sleep 2
    open "http://localhost:8080" 2>/dev/null || true
  fi

  wait "$SIM_PID" 2>/dev/null || true
}

cmd_serve() {
  build_go
  trap 'stop_server; exit 0' INT TERM
  start_server
  wait_for_server
  log "Press Ctrl+C to stop"
  wait "$(cat "$PID_FILE")" 2>/dev/null || true
}

cmd_simulate() {
  build_go
  log "Running simulator..."
  "$BIN" simulate
}

cmd_query() {
  build_go
  local q="${1:-cpu_usage_percent}"
  log "Querying: $q"
  "$BIN" query "$q"
}

cmd_bench() {
  build_go
  log "Running compression benchmarks..."
  "$BIN" bench
}

cmd_clean() {
  log "Cleaning..."
  rm -rf "$ROOT/bin" "$ROOT/data" "$ROOT/dashboard/dist" "$ROOT/dashboard/node_modules"
  rm -f "$PID_FILE"
  ok "Clean"
}

case "${1:-demo}" in
  demo)     cmd_demo ;;
  serve)    cmd_serve ;;
  simulate) cmd_simulate "${@:2}" ;;
  query)    cmd_query "${@:2}" ;;
  bench)    cmd_bench ;;
  clean)    cmd_clean ;;
  *)
    echo "Usage: $0 [demo|serve|simulate|query|bench|clean]"
    exit 1
    ;;
esac
