#!/bin/bash
# E2E: three-process run with fixture configs. Tap and proxy modes, stub
# ScanRelay present/absent, plus kill -9 restart survival.
#
# Runs UNSANDBOXED: it binds loopback ports and controls processes.
# Usage: ./e2e/run.sh   (from anywhere; exits non-zero on first failure)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/.e2e-bin"
DATA="$(mktemp -d "${TMPDIR:-/tmp}/wb-e2e.XXXXXX")"
BRIDGE_PORT=18081
STUB_PORT=18080
SECRET="e2e-secret-0123456789abcdef0123456789"

BRIDGE_PID=""; STUB_PID=""
# Kills are routine here (case teardown, kill -9 test); wait reaps each
# pid so bash does not narrate our own cleanup kills.
quiet_kill() { kill -9 "$1" 2>/dev/null || true; wait "$1" 2>/dev/null || true; }
cleanup() {
  [ -n "$BRIDGE_PID" ] && quiet_kill "$BRIDGE_PID"
  [ -n "$STUB_PID" ] && quiet_kill "$STUB_PID"
  rm -rf "$DATA"
}
trap cleanup EXIT

step() { echo "### $*"; }
fail() {
  echo "E2E FAIL: $*" >&2
  echo "--- process logs (kept, dir removed on exit):"
  for f in "$DATA"/*.log; do
    [ -f "$f" ] && { echo "--- $f"; tail -20 "$f"; }
  done
  exit 1
}

build_all() {
  mkdir -p "$BIN"
  (cd "$ROOT" && go build -o "$BIN/bridge" ./cmd/watcherbridge \
    && go build -o "$BIN/stubrelay" ./e2e/stubrelay \
    && go build -o "$BIN/send" ./e2e/send)
}

start_stub() { # [extra stub flags]
  "$BIN/stubrelay" --port "$STUB_PORT" --dir "$DATA/stub" "$@" >"$DATA/stub.log" 2>&1 &
  STUB_PID=$!
  sleep 0.5 # no GET routes on the stub; a fixed settle beats a fake probe
}

stop_stub() { [ -n "$STUB_PID" ] && quiet_kill "$STUB_PID"; STUB_PID=""; }

start_bridge() { # mode dbname [extra bridge flags]
  local mode="$1" db="$2"; shift 2
  "$BIN/bridge" -bind "127.0.0.1:$BRIDGE_PORT" -mode "$mode" -db "$DATA/$db" \
    -secret "$SECRET" -scanrelay-url "http://127.0.0.1:$STUB_PORT" \
    -group-quiet-window 4s -grouper-tick 500ms "$@" >"$DATA/bridge-$db.log" 2>&1 &
  BRIDGE_PID=$!
  for _ in $(seq 1 100); do
    curl -sf -o /dev/null "http://127.0.0.1:$BRIDGE_PORT/" && return 0
    sleep 0.1
  done
  fail "bridge did not come up ($mode $db)"
}

stop_bridge() { [ -n "$BRIDGE_PID" ] && quiet_kill "$BRIDGE_PID"; BRIDGE_PID=""; }
kill9_bridge() { quiet_kill "$BRIDGE_PID"; BRIDGE_PID=""; sleep 0.5; }

send_event() { # id type fixture [out]
  local status
  status="$("$BIN/send" -secret "$SECRET" -url "http://127.0.0.1:$BRIDGE_PORT/webhook" \
    -id "$1" -type "$2" -payload "$ROOT/e2e/fixtures/$3" ${4:+-out "$4"} | sed 's/STATUS=//')"
  echo "$status"
}

briefing_count() {
  curl -s "http://127.0.0.1:$BRIDGE_PORT/api/briefings?limit=100" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))"
}

annotation_count() {
  [ -f "$DATA/stub/annotations.jsonl" ] && wc -l <"$DATA/stub/annotations.jsonl" | tr -d ' ' || echo 0
}

wait_for() { # what want timeout_secs command...
  local what="$1" want="$2" timeout="$3"; shift 3
  local deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt "$deadline" ]; do
    [ "$("$@")" = "$want" ] && return 0
    sleep 1
  done
  fail "timed out waiting for $what == $want"
}

step "build"
build_all

# --- case A: tap, stub present -------------------------------------------
step "A: tap + ScanRelay present"
rm -rf "$DATA/stub"; start_stub
start_bridge tap a.db
[ "$(send_event e2e-1 host.changed changed.json)" = 202 ] || fail "tap post 1"
[ "$(send_event e2e-2 host.appeared appeared.json)" = 202 ] || fail "tap post 2"
wait_for "briefings" 2 30 briefing_count
wait_for "annotations" 2 30 annotation_count
python3 - "$DATA/stub/annotations.jsonl" <<'EOF'
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1])]
assert len(rows) == 2, rows
for a in rows:
    assert "severity" not in a, "bridge must never write ScanRelay severity"
    assert a["priority"] in ("low", "medium", "high", "critical"), a
    assert a["event_ids"], a
print("annotations contract ok")
EOF
stop_bridge; stop_stub

# --- case B: tap, stub absent --------------------------------------------
step "B: tap + ScanRelay absent (local unaffected)"
start_bridge tap b.db
[ "$(send_event e2e-3 host.changed changed.json)" = 202 ] || fail "tap post 3"
wait_for "briefings" 1 30 briefing_count
stop_bridge

# --- case C: proxy, stub present, byte-identity ---------------------------
step "C: proxy + ScanRelay present"
rm -rf "$DATA/stub"; start_stub
start_bridge proxy c.db
[ "$(send_event e2e-4 host.changed changed.json "$DATA/sent.json")" = 202 ] || fail "proxy post"
wait_for "briefings" 1 30 briefing_count
wait_for "annotations" 1 30 annotation_count
sent="$(shasum -a 256 "$DATA/sent.json" | cut -d' ' -f1)"
fwd="$(python3 -c "import json; print(open('$DATA/stub/webhook-001.json').read().split(chr(10),1)[0].split('=')[1])")"
[ "$sent" = "$fwd" ] || fail "forwarded bytes differ: $sent vs $fwd"
echo "byte-identical forward ok"
stop_bridge; stop_stub

# --- case D: proxy, stub absent ------------------------------------------
step "D: proxy + ScanRelay absent (502, briefing still completes)"
start_bridge proxy d.db
[ "$(send_event e2e-5 host.changed changed.json)" = 502 ] || fail "proxy post without relay must be 502"
wait_for "briefings" 1 30 briefing_count
stop_bridge

# --- case E: kill -9 mid-window, restart ----------------------------------
step "E: kill -9 mid-window, exactly one briefing after restart"
rm -rf "$DATA/stub"; start_stub
start_bridge tap e.db
[ "$(send_event e2e-6 host.changed changed.json)" = 202 ] || fail "restart post"
sleep 1 # mid-window: group still open
kill9_bridge
start_bridge tap e.db
wait_for "briefings" 1 30 briefing_count
[ "$(send_event e2e-6 host.changed changed.json)" = 202 ] || fail "idempotent repost"
sleep 6 # past another window: a duplicate must not brief twice
[ "$(briefing_count)" = 1 ] || fail "duplicate briefing after restart"
[ "$(annotation_count)" = 1 ] || fail "duplicate annotation after restart"
stop_bridge; stop_stub

echo "E2E PASS"
