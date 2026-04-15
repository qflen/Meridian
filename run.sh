#!/usr/bin/env bash
set -euo pipefail

# ── Meridian TSDB – Quick Start ──────────────────────────────────────
# Usage: ./run.sh [demo|docker|docker-dev|serve|simulate|query|bench|clean]
#
# demo       – build, start server, launch simulator, open dashboard
# docker     – run microservices cluster + simulator via Docker Compose
# docker-dev – Docker cluster with hot-reload for the dashboard
# serve      – start the server only
# simulate   – run the simulator against a running server
# query      – run a sample query
# bench      – run compression benchmarks
# clean      – remove build artifacts and data

ROOT="$(cd "$(dirname "$0")" && pwd)"
BIN="$ROOT/bin/meridian"
DATA="$ROOT/data"
PID_FILE="$ROOT/.meridian.pid"

SIM_PID=""

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

cleanup() {
  if [ -n "$SIM_PID" ] && kill -0 "$SIM_PID" 2>/dev/null; then
    log "Stopping simulator (PID $SIM_PID)..."
    kill "$SIM_PID" 2>/dev/null || true
    wait "$SIM_PID" 2>/dev/null || true
    ok "Simulator stopped"
  fi
  stop_server
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

  trap 'cleanup; exit 0' INT TERM

  start_server
  wait_for_server

  log "Starting simulator (8 hosts, 43 metrics)..."
  "$BIN" simulate &
  SIM_PID=$!

  echo
  ok "Dashboard: http://localhost:8080"
  ok "API:       http://localhost:8080/api/v1/query?q=cpu_usage_percent"
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

ensure_docker() {
  if ! command -v docker &>/dev/null; then
    err "Docker is not installed. Install Docker Desktop from https://www.docker.com/products/docker-desktop/"
    exit 1
  fi

  # Check if Docker daemon is running
  if ! docker info &>/dev/null 2>&1; then
    warn "Docker daemon is not running."
    # macOS: try to start Docker Desktop
    if [ "$(uname)" = "Darwin" ]; then
      if [ -d "/Applications/Docker.app" ]; then
        log "Starting Docker Desktop..."
        open -a Docker
        log "Waiting for Docker daemon to be ready (this may take 30-60s)..."
        local attempts=0
        while ! docker info &>/dev/null 2>&1; do
          attempts=$((attempts + 1))
          if [ $attempts -ge 60 ]; then
            err "Docker daemon did not start within 60s. Open Docker Desktop manually and retry."
            exit 1
          fi
          sleep 2
        done
        ok "Docker daemon is ready"
      else
        err "Docker Desktop not found in /Applications. Install it from https://www.docker.com/products/docker-desktop/"
        exit 1
      fi
    else
      err "Start the Docker daemon (e.g. 'sudo systemctl start docker') and retry."
      exit 1
    fi
  fi
}

cmd_docker() {
  log "═══════════════════════════════════════════════"
  log "  Meridian TSDB – Docker Microservices Cluster"
  log "═══════════════════════════════════════════════"
  echo

  ensure_docker

  log "Building and starting microservices cluster..."
  echo
  ok "Once running:"
  ok "  Gateway:    http://localhost:8080  (dashboard + API)"
  ok "  Ingestor 1: TCP :9090"
  ok "  Ingestor 2: TCP :9091"
  ok "  Storage:    3 nodes (internal)"
  ok "  Querier:    1 node (internal)"
  ok "  Compactor:  1 node (internal)"
  echo
  log "Press Ctrl+C to stop the cluster"
  echo

  # Run in foreground — Ctrl-C sends SIGINT which docker compose handles gracefully
  docker compose -f "$ROOT/docker-compose.yml" up --build

  # If we get here, user stopped with Ctrl-C or containers exited
  log "Cleaning up containers..."
  docker compose -f "$ROOT/docker-compose.yml" down
  ok "Cluster stopped"
}

cmd_docker_dev() {
  log "═══════════════════════════════════════════════"
  log "  Meridian TSDB – Docker Dev Mode (hot-reload)"
  log "═══════════════════════════════════════════════"
  echo

  ensure_docker

  # Create a dev compose override that mounts dashboard source for hot-reload
  cat > "$ROOT/docker-compose.dev.yml" <<'DEVEOF'
services:
  gateway:
    volumes:
      - ./dashboard/dist:/app/dashboard/dist
DEVEOF

  # Build dashboard in watch mode in background
  log "Starting dashboard dev server..."
  if [ ! -d "$ROOT/dashboard/node_modules" ]; then
    (cd "$ROOT/dashboard" && npm install --no-audit --no-fund)
  fi

  # Do an initial build
  (cd "$ROOT/dashboard" && npx vite build --outDir dist)

  # Start vite build in watch mode
  (cd "$ROOT/dashboard" && npx vite build --outDir dist --watch) &
  local VITE_PID=$!

  trap 'kill $VITE_PID 2>/dev/null; docker compose -f "$ROOT/docker-compose.yml" -f "$ROOT/docker-compose.dev.yml" down; rm -f "$ROOT/docker-compose.dev.yml"; exit 0' INT TERM

  echo
  ok "Dashboard hot-reload enabled"
  ok "Gateway: http://localhost:8080"
  echo
  log "Press Ctrl+C to stop"
  echo

  docker compose -f "$ROOT/docker-compose.yml" -f "$ROOT/docker-compose.dev.yml" up --build

  kill $VITE_PID 2>/dev/null || true
  docker compose -f "$ROOT/docker-compose.yml" -f "$ROOT/docker-compose.dev.yml" down
  rm -f "$ROOT/docker-compose.dev.yml"
  ok "Dev cluster stopped"
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
  rm -f "$PID_FILE" "$ROOT/docker-compose.dev.yml"
  ok "Clean"
}

case "${1:-demo}" in
  demo)       cmd_demo ;;
  docker)     cmd_docker ;;
  docker-dev) cmd_docker_dev ;;
  serve)      cmd_serve ;;
  simulate)   cmd_simulate "${@:2}" ;;
  query)      cmd_query "${@:2}" ;;
  bench)      cmd_bench ;;
  clean)      cmd_clean ;;
  *)
    echo "Usage: $0 [demo|docker|docker-dev|serve|simulate|query|bench|clean]"
    exit 1
    ;;
esac
