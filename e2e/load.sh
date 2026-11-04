#!/bin/bash
# Step 31 load measurements. Sibling of e2e/run.sh: same constraints
# (synthetic data only, loopback only, UNSANDBOXED — binds are required),
# but repeatable measured runs instead of pass/fail scenarios.
#
# Workloads (each 3 repetitions, tap and proxy): 512-event burst
# (all-at-once, 16 senders) and 50 events/s for 60s (open-loop ticker;
# the generator counts unsent arrivals instead of slowing down).
# Added proxy overhead is measured against a matched direct-to-stub
# baseline run with the same generator, concurrency and fixtures.
# Results land machine-readable under e2e/results/<utc-timestamp>/
# with an explicit per-phase budget verdict. Expected wall time ~35 min.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DATA="$(mktemp -d "${TMPDIR:-/tmp}/wb-load.XXXXXX")"
SECRET="e2e-load-secret-0123456789abcdef"
BRIDGE_PORT=18081
STUB_PORT=18080
RES="$ROOT/e2e/results/$(date -u +%Y%m%dT%H%M%SZ)"
BIN="$ROOT/.e2e-bin"
export GOCACHE="${GOCACHE:-$(go env GOCACHE)}"

cleanup() {
  stop_bridge
  stop_stub
  [ -f "$DATA/samplers" ] && while read -r pid; do kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; done <"$DATA/samplers"
  mkdir -p "$RES/process"
  cp "$DATA"/*.log "$RES/process/" 2>/dev/null || true
  cp "$DATA"/*.db "$RES/process/" 2>/dev/null || true
  cp "$DATA"/*.db-wal "$RES/process/" 2>/dev/null || true
  cp "$DATA"/*.db-shm "$RES/process/" 2>/dev/null || true
  cp "$DATA"/wal-*.txt "$RES/process/" 2>/dev/null || true
  echo "load artifacts: $DATA" >&2
}
trap cleanup EXIT INT TERM

fail() { echo "LOAD FAIL: $*" >&2; echo "results (partial) at $RES"; exit 1; }

record_env() {
  mkdir -p "$RES"
  {
    echo "os=$(uname -srm)"
    echo "cpu=$(sysctl -n machdep.cpu.brand_string 2>/dev/null || grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2)"
    echo "ncpu=$(sysctl -n hw.ncpu 2>/dev/null || nproc)"
    echo "go=$(go version)"
    echo "cc=$(cc --version 2>/dev/null | head -1)"
    echo "commit=$(git -C "$ROOT" rev-parse HEAD)"
    echo "tree=$(git -C "$ROOT" status --short | head -5 | tr '\n' ';')"
    echo "db=fresh per rep; payloads=appeared/changed-qualified rotating 64 synthetic mac refs (198.51.100.0/24)"
    echo "generator=open-loop (burst all-at-once/16 workers; sustained 50/s ticker, unsent arrivals counted)"
    echo "heap=GC trace pre/peak/post MiB parsed from bridge stderr; GC-paced samples only, full-interval gate pending"
  } >"$RES/env.txt"
  cat "$RES/env.txt"
}

build_all() {
  mkdir -p "$BIN"
  (cd "$ROOT" && go build -o "$BIN/bridge" ./cmd/watcherbridge \
    && go build -o "$BIN/stubrelay" ./e2e/stubrelay \
    && go build -o "$BIN/loadgen" ./e2e/loadgen) || fail "build"
}

start_stub() {
  if [ -d "$DATA/stub" ]; then cp -R "$DATA/stub" "$RES/stub-$(date +%s%N)" 2>/dev/null || true; fi
  rm -rf "$DATA/stub"
  mkdir -p "$DATA/stub"
  "$BIN/stubrelay" --dir "$DATA/stub" --port "$STUB_PORT" >"$DATA/stub.log" 2>&1 &
  STUB_PID=$!
  for _ in $(seq 1 100); do
    kill -0 "$STUB_PID" 2>/dev/null || fail "stub relay exited before readiness"
    # A 404 proves that the HTTP listener is serving; the stub has no GET route.
    code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$STUB_PORT/" 2>/dev/null || true)"
    [ "$code" = 404 ] && return 0
    sleep 0.1
  done
  fail "stub relay did not become ready"
}

stop_stub() { if [ -n "${STUB_PID:-}" ]; then kill "$STUB_PID" 2>/dev/null || true; wait "$STUB_PID" 2>/dev/null || true; STUB_PID=""; fi; }

start_bridge() { # mode dbname (log appends: sweep restarts share it)
  GODEBUG=gctrace=1 "$BIN/bridge" -bind "127.0.0.1:$BRIDGE_PORT" -mode "$1" -db "$DATA/$2" \
    -secret "$SECRET" -scanrelay-url "http://127.0.0.1:$STUB_PORT" >>"$DATA/bridge-$2.log" 2>&1 &
  BRIDGE_PID=$!
  echo "$BRIDGE_PID" >"$DATA/bridge.pid"
  for _ in $(seq 1 100); do
    kill -0 "$BRIDGE_PID" 2>/dev/null || fail "bridge exited before readiness ($1 $2)"
    sleep 0.1
    curl -sf -o /dev/null "http://127.0.0.1:$BRIDGE_PORT/" 2>/dev/null && return 0
  done
  fail "bridge did not come up ($1 $2)"
}

stop_bridge() { if [ -n "${BRIDGE_PID:-}" ]; then kill "$BRIDGE_PID" 2>/dev/null || true; wait "$BRIDGE_PID" 2>/dev/null || true; BRIDGE_PID=""; fi; }

db_count() {
  python3 -c "import sqlite3; c=sqlite3.connect('file:$DATA/$1?mode=ro',uri=True); print(c.execute('select count(*) from $2').fetchone()[0])"
}

wait_stable() { # db table timeout_s: count unchanged across 5s
  prev=""; stable=0
  for _ in $(seq 1 "$3"); do
    cur="$(db_count "$1" "$2")"
    if [ "$cur" = "$prev" ]; then
      stable=$((stable + 1)); [ "$stable" -ge 5 ] && return 0
    else stable=0; fi
    prev="$cur"; sleep 1
  done
  fail "table $2 never stabilized"
}

sample_wal_while() { # db outfile: background 5s WAL sampler until bridge stops
  ( while kill -0 "$BRIDGE_PID" 2>/dev/null; do
      wc -c <"$DATA/$1-wal" 2>/dev/null || echo 0
      sleep 5
    done >"$DATA/$2" 2>&1 ) &
  echo "$!" >>"$DATA/samplers"
}

echo "### load env"; record_env
echo "### build"; build_all

FIX="$ROOT/e2e/fixtures/appeared-qualified.json,$ROOT/e2e/fixtures/changed-qualified.json"
MANIFEST="$RES/forwarded-manifest.txt"
: >"$MANIFEST"

drive() { # phase url: burst or sustained per phase name
  case "$1" in
    *-burst-*) "$BIN/loadgen" -url "$2" -secret "$SECRET" -mode burst -n 512 -workers 16 \
      -fixtures "$FIX" -prefix "ld-$1" -out "$RES/$1.jsonl" | tee "$RES/$1.summary";;
    *-sust-*) "$BIN/loadgen" -url "$2" -secret "$SECRET" -mode sustained -rate 50 -duration 60s -workers 8 \
      -fixtures "$FIX" -prefix "ld-$1" -out "$RES/$1.jsonl" | tee "$RES/$1.summary";;
    *) fail "unknown load phase: $1";;
  esac
}

for rep in 1 2 3; do
  # --- tap burst + sustained -------------------------------------------
  start_stub
  start_bridge tap "tap-$rep.db"
  sample_wal_while "tap-$rep.db" "wal-tap-$rep.txt"
  drive "tap-burst-$rep" "http://127.0.0.1:$BRIDGE_PORT/webhook" "tap-$rep.db"
  wait_stable "tap-$rep.db" raw_events 120
  drive "tap-sust-$rep" "http://127.0.0.1:$BRIDGE_PORT/webhook" "tap-$rep.db"
  wait_stable "tap-$rep.db" raw_events 180
  stop_bridge; stop_stub
  # --- proxy burst + sustained (healthy relay) --------------------------
  start_stub
  start_bridge proxy "proxy-$rep.db"
  sample_wal_while "proxy-$rep.db" "wal-proxy-$rep.txt"
  drive "proxy-burst-$rep" "http://127.0.0.1:$BRIDGE_PORT/webhook" "proxy-$rep.db"
  wait_stable "proxy-$rep.db" raw_events 120
  drive "proxy-sust-$rep" "http://127.0.0.1:$BRIDGE_PORT/webhook" "proxy-$rep.db"
  wait_stable "proxy-$rep.db" raw_events 180
  stop_bridge
  echo "proxy-$rep forwarded=$(find "$DATA/stub" -name 'webhook-*.json' | wc -l)" | tee -a "$MANIFEST"
  start_bridge proxy "proxy-$rep.db" # startup sweep drains stragglers
  sleep 5; stop_bridge; stop_stub
  echo "proxy-$rep forwarded-after-sweep=$(find "$DATA/stub" -name 'webhook-*.json' | wc -l)" | tee -a "$MANIFEST"
  cp -R "$DATA/stub" "$RES/proxy-$rep-stub"
  # --- direct-to-stub baselines (bridge overhead reference) --------------
  start_stub
  drive "direct-burst-$rep" "http://127.0.0.1:$STUB_PORT/webhook" _
  drive "direct-sust-$rep" "http://127.0.0.1:$STUB_PORT/webhook" _
  stop_stub
done

echo "### verdicts"
python3 "$ROOT/e2e/load_verdict.py" "$RES" "$DATA"
echo "LOAD DONE: results at $RES"
