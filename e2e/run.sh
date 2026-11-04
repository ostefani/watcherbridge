#!/bin/bash
# E2E: three-process run with fixture configs. Tap and proxy modes, stub
# ScanRelay present/absent, plus kill -9 restart survival.
#
# Runs UNSANDBOXED: it binds loopback ports and controls processes.
# Usage: ./e2e/run.sh   (from anywhere; exits non-zero on first failure)
# E2E_SKIP_LONG=1 skips M/KP2 for a fast functional regression; those
# retention/crash cells remain unrun in that invocation.
# E2E_MIG_ONLY=1 runs only MIG/MIGK/NEGMIG.
# E2E_RETENTION_ONLY=1 runs only M/KP2.
# E2E_MATRIX_ONLY=1 runs MX plus the migration tail (unless E2E_SKIP_MIG=1).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/.e2e-bin"
DATA="$(mktemp -d "${TMPDIR:-/tmp}/wb-e2e.XXXXXX")"
BRIDGE_PORT=18081
STUB_PORT=18080
SECRET="e2e-secret-0123456789abcdef0123456789"

BRIDGE_PID=""; STUB_PID=""
PENDING="$DATA/pending.txt"
pending() { echo "PENDING: $*" | tee -a "$PENDING" >&2; }
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
  echo "--- process logs (full bundle preserved):"
  for f in "$DATA"/*.log; do
    [ -f "$f" ] && { echo "--- $f"; tail -20 "$f"; }
  done
  bundle="$ROOT/.e2e-failures/$(date -u +%Y%m%dT%H%M%SZ)-$$"
  mkdir -p "$ROOT/.e2e-failures"
  cp -r "$DATA" "$bundle" 2>/dev/null || true
  echo "E2E evidence preserved at $bundle"
  exit 1
}

build_all() {
  mkdir -p "$BIN"
  (cd "$ROOT" && go build -o "$BIN/bridge" ./cmd/watcherbridge \
    && go build -o "$BIN/stubrelay" ./e2e/stubrelay \
    && go build -o "$BIN/send" ./e2e/send \
    && go build -o "$BIN/register" ./e2e/register \
    && go build -o "$BIN/seed" ./e2e/seed)
}

start_stub() { # [extra stub flags]
  "$BIN/stubrelay" --port "$STUB_PORT" --dir "$DATA/stub" "$@" >"$DATA/stub.log" 2>&1 &
  STUB_PID=$!
  for _ in $(seq 1 100); do
    kill -0 "$STUB_PID" 2>/dev/null || fail "stub relay exited before readiness"
    [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$STUB_PORT/" 2>/dev/null || true)" = 404 ] && return 0
    sleep 0.1
  done
  fail "stub relay did not become ready"
}

stop_stub() { [ -n "$STUB_PID" ] && quiet_kill "$STUB_PID"; STUB_PID=""; }

start_bridge() { # mode dbname [extra bridge flags]
  local mode="$1" db="$2"; shift 2
  "$BIN/bridge" -bind "127.0.0.1:$BRIDGE_PORT" -mode "$mode" -db "$DATA/$db" \
    -secret "$SECRET" -scanrelay-url "http://127.0.0.1:$STUB_PORT" \
    -group-quiet-window 4s -grouper-tick 500ms -incident-tick 5s "$@" >"$DATA/bridge-$db.log" 2>&1 &
  BRIDGE_PID=$!
  for _ in $(seq 1 100); do
    kill -0 "$BRIDGE_PID" 2>/dev/null || fail "bridge exited before readiness ($mode $db)"
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

send_mismatch() { # id type fixture: deliberately inconsistent envelope
  # Sends the fixture bytes verbatim with mismatched header identity as
  # an explicit negative case (Designv9 §1.1 metadata consistency).
  local status
  status="$("$BIN/send" -secret "$SECRET" -url "http://127.0.0.1:$BRIDGE_PORT/webhook" \
    -id "$1" -type "$2" -payload "$ROOT/e2e/fixtures/$3" -rewrite-envelope=false | sed 's/STATUS=//')"
  echo "$status"
}

briefing_count() {
  curl -s "http://127.0.0.1:$BRIDGE_PORT/api/briefings?limit=100" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))"
}

incidents_count() {
  curl -s "http://127.0.0.1:$BRIDGE_PORT/api/incidents?limit=100" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))"
}

incident_field() { # incident_id field
  curl -s "http://127.0.0.1:$BRIDGE_PORT/api/incidents?limit=100" | python3 -c "import json,sys; rows=json.load(sys.stdin); print(next(r['$2'] for r in rows if r['incident_id']=='$1'))"
}

source_liveness() {
  curl -s "http://127.0.0.1:$BRIDGE_PORT/api/overview" | python3 -c "import json,sys; sources=json.load(sys.stdin).get('sources') or []; print(sources[0]['liveness'] if sources else '')"
}

source_has_heartbeat() {
  curl -s "http://127.0.0.1:$BRIDGE_PORT/api/overview" | python3 -c "import json,sys; print('heartbeat' in json.load(sys.stdin)['sources'][0])"
}

groups_count() {
  curl -s "http://127.0.0.1:$BRIDGE_PORT/api/groups" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))"
}

register_cap() { # db namespace capabilities (before bridge start only)
  "$BIN/register" -db "$DATA/$1" -scope default -namespace "$2" -capabilities "$3"
}

send_hb() { # scanid (32 lowercase hex): posts a signed H1 heartbeat
  "$BIN/send" -secret "$SECRET" -url "http://127.0.0.1:$BRIDGE_PORT/webhook" \
    -id "$1" -heartbeat | sed 's/STATUS=//'
}

# Read-only SQLite probes (mode=ro URI: never a second writer while the
# bridge owns the database). Used for purge/migration/token evidence
# the HTTP API does not expose.
db_count() { # db table
  python3 -c "import sqlite3,sys; c=sqlite3.connect('file:$DATA/$1?mode=ro',uri=True); print(c.execute('select count(*) from $2').fetchone()[0])"
}
subject_count() { db_count "$1" reported_subjects; }
group_status_count() { # db status
  python3 -c "import sqlite3; c=sqlite3.connect('file:$DATA/$1?mode=ro',uri=True); print(c.execute('select count(*) from active_groups where status=?',('$2',)).fetchone()[0])"
}

db_version() {
  python3 -c "import sqlite3; c=sqlite3.connect('file:$DATA/$1?mode=ro',uri=True); print(c.execute('pragma user_version').fetchone()[0])"
}

purge_completed() {
  python3 -c "import sqlite3; c=sqlite3.connect('file:$DATA/$1?mode=ro',uri=True); print(c.execute(\"select count(*) from purge_runs where status='completed'\").fetchone()[0])"
}

purge_statuses() {
  python3 -c "import sqlite3; c=sqlite3.connect('file:$DATA/$1?mode=ro',uri=True); print(' '.join(sorted(r[0] for r in c.execute('select distinct status from purge_runs'))))"
}

purge_maxrun() { # max affected rows (deleted + bookkeeping) in any run
  python3 - "$DATA/$1" <<'EOF'
import sqlite3, sys
c = sqlite3.connect(f'file:{sys.argv[1]}?mode=ro', uri=True)
cols = [d[0] for d in c.execute('select * from purge_runs limit 0').description]
take = [i for i, n in enumerate(cols) if n.endswith('_deleted') or n == 'bookkeeping_writes']
print(max([sum(r[i] for i in take) for r in c.execute('select * from purge_runs')] or [0]))
EOF
}

purge_running_seen() { # yes once any run row is mid-flight
  python3 -c "import sqlite3; c=sqlite3.connect('file:$DATA/$1?mode=ro',uri=True); print('yes' if c.execute(\"select 1 from purge_runs where status='running' limit 1\").fetchone() else 'no')"
}

token_counter() { # high-water next value: must never decrease
  python3 -c "import sqlite3; c=sqlite3.connect('file:$DATA/$1?mode=ro',uri=True); print(c.execute(\"select next from pseudonym_counters where category='hostname'\").fetchone()[0])"
}

token_unique() { # distinct tokens stay 1:1 (UNIQUE also enforces it)
  python3 -c "import sqlite3; c=sqlite3.connect('file:$DATA/$1?mode=ro',uri=True); n=c.execute('select count(*), count(distinct token) from pseudonym_map').fetchone(); print('ok' if n[0]==n[1] else 'BAD %s' % (n,))"
}

table_exists() { # yes iff the table survived migration
  python3 -c "import sqlite3; c=sqlite3.connect('file:$DATA/$1?mode=ro',uri=True); print('yes' if c.execute(\"select 1 from sqlite_master where name='$2'\").fetchone() else 'no')"
}

wal_bytes() {
  f="$DATA/$1-wal"
  if [ -f "$f" ]; then wc -c <"$f"; else echo 0; fi
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

wait_for_ge() { # what want timeout_secs command...: numeric >= for
  # monotonic counters (purge_runs) that may start above the target after
  # a restart; exact-equality would spin past an already-satisfied goal.
  local what="$1" want="$2" timeout="$3"; shift 3
  local deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt "$deadline" ]; do
    [ "$("$@")" -ge "$want" ] 2>/dev/null && return 0
    sleep 1
  done
  fail "timed out waiting for $what >= $want"
}

step "build"
build_all

if [ "${E2E_MIG_ONLY:-0}" != 1 ]; then
if [ "${E2E_RETENTION_ONLY:-0}" != 1 ]; then
if [ "${E2E_MATRIX_ONLY:-0}" != 1 ]; then
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
[ "$(group_status_count e.db open)" = 1 ] || fail "open group not observed before kill"
kill9_bridge
start_bridge tap e.db
wait_for "briefings" 1 30 briefing_count
[ "$(send_event e2e-6 host.changed changed.json)" = 202 ] || fail "idempotent repost"
sleep 6 # past another window: a duplicate must not brief twice
[ "$(briefing_count)" = 1 ] || fail "duplicate briefing after restart"
[ "$(annotation_count)" = 1 ] || fail "duplicate annotation after restart"
stop_bridge; stop_stub

# --- case F: inconsistent envelope rejected, nothing stored --------------
step "F: split envelope is 400 with no briefing"
start_bridge tap f.db
[ "$(send_event e2e-7 host.appeared appeared-qualified.json)" = 202 ] || fail "qualified post"
wait_for "briefings" 1 30 briefing_count
# Fixture bytes carry e2e-qualified-1/* while headers say otherwise.
[ "$(send_mismatch e2e-8 host.appeared appeared-qualified.json)" = 400 ] || fail "split envelope must be 400"
sleep 6 # past another window: the rejected delivery must not brief
[ "$(briefing_count)" = 1 ] || fail "split envelope produced a briefing"
stop_bridge

# --- case G1: capability absent stays event-local ------------------------
step "G1: no registration, qualified shape still uncounted"
start_bridge tap g1.db
[ "$(send_event e2e-g1a host.appeared appeared-qualified.json)" = 202 ] || fail "g1 mac post"
[ "$(send_event e2e-g1b host.appeared appeared-provided-e2e.json)" = 202 ] || fail "g1 provided post"
wait_for "briefings" 2 30 briefing_count
[ "$(incidents_count)" = 0 ] || fail "incident formed without a capability contract"
stop_bridge

# --- case G2: synthetic supported capability forms an incident -----------
step "G2: registered capability, burst forms one incident"
register_cap g2.db e2e-ns-1 stable-identity-v1
start_bridge tap g2.db
[ "$(send_event e2e-g2a host.appeared burst-a1.json)" = 202 ] || fail "g2 post a1"
[ "$(send_event e2e-g2b host.appeared burst-a2.json)" = 202 ] || fail "g2 post a2"
[ "$(send_event e2e-g2c host.appeared burst-a3.json)" = 202 ] || fail "g2 post a3"
wait_for "briefings" 3 30 briefing_count
wait_for "incidents" 1 60 incidents_count
INC="$(curl -s "http://127.0.0.1:$BRIDGE_PORT/api/incidents?limit=100" | python3 -c "import json,sys; print(json.load(sys.stdin)[0]['incident_id'])")"
[ "$(incident_field "$INC" kind)" = burst_appearance ] || fail "incident kind"
[ "$(incident_field "$INC" member_count)" = 3 ] || fail "member count"
[ "$(incident_field "$INC" qualified_refs)" = 3 ] || fail "qualified refs"
[ -n "$(incident_field "$INC" floor)" ] || fail "incident floor empty"
stop_bridge

# --- case H: namespace change is stop/rebind/restart, never a merge -----
step "H: rebind to ns-2, old incident untouched"
register_cap g2.db e2e-ns-2 ""
start_bridge tap g2.db
[ "$(send_event e2e-h1 host.appeared burst-a1.json)" = 202 ] || fail "h post"
wait_for "briefings" 4 30 briefing_count
[ "$(incidents_count)" = 1 ] || fail "rebind formed or dropped an incident"
[ "$(incident_field "$INC" member_count)" = 3 ] || fail "old incident members changed across rebind"
stop_bridge

# --- case H1a: no heartbeat reads unknown liveness -----------------------
step "H1a: silence without H1 is unknown, never blind"
start_bridge tap h1.db
[ "$(send_event e2e-h1a host.appeared appeared-qualified.json)" = 202 ] || fail "h1a post"
wait_for "briefings" 1 30 briefing_count
wait_for "liveness" unknown 30 source_liveness
[ "$(source_has_heartbeat)" = False ] || fail "heartbeat recorded without H1"
stop_bridge

# --- case H1b: fresh then stale heartbeat --------------------------------
HB1="$(printf 'a%.0s' $(seq 32))"
step "H1b: heartbeat reporting then stale"
start_bridge tap h2.db -heartbeat-stale-after 5s
[ "$(send_hb "$HB1")" = 202 ] || fail "heartbeat post"
wait_for "heartbeat" True 30 source_has_heartbeat
wait_for "liveness" reporting 30 source_liveness
sleep 8 # past the 5s stale horizon
wait_for "liveness" stale 30 source_liveness
stop_bridge

# --- case H1c: heartbeat never enters the relay pipeline -----------------
step "H1c: proxy heartbeat is local-only"
rm -rf "$DATA/stub"; start_stub
start_bridge proxy h3.db
[ "$(send_hb "$HB1")" = 202 ] || fail "proxy heartbeat post"
sleep 3 # a forwarded heartbeat would have landed by now
[ -z "$(ls "$DATA/stub"/webhook-*.json 2>/dev/null || true)" ] || fail "heartbeat forwarded to ScanRelay"
[ "$(briefing_count)" = 0 ] || fail "heartbeat briefed"
stop_bridge; stop_stub

# --- case I: duplicates, conflicts, out-of-order -------------------------
step "I: duplicate/conflicting/out-of-order delivery"
start_bridge tap i.db
[ "$(send_event e2e-d1 host.changed changed.json)" = 202 ] || fail "dup post 1"
[ "$(send_event e2e-d1 host.changed changed.json)" = 202 ] || fail "dup repost"
[ "$(send_event e2e-d1 host.appeared appeared.json)" = 202 ] || fail "conflicting body, same id"
[ "$(send_event e2e-o1 host.disappeared disappeared-qualified.json)" = 202 ] || fail "ooo disappeared first"
[ "$(send_event e2e-o2 host.appeared appeared-qualified.json)" = 202 ] || fail "ooo appeared second"
# e2e-d1's three deliveries share one event-local group (first wins);
# the two qualified posts group by their own references: 3 briefings.
wait_for "briefings" 3 30 briefing_count
stop_bridge

# --- case D2: downstream failure, then recovery forwards -----------------
webhook_present() { [ -f "$DATA/stub/webhook-001.json" ] && echo yes || echo no; }
step "D2: relay 500, then healthy restart forwards original bytes"
rm -rf "$DATA/stub"; start_stub --fail webhook-500
start_bridge proxy d2.db
[ "$(send_event e2e-d2 host.changed changed.json "$DATA/d2-sent.json")" = 502 ] || fail "failing relay must be 502"
stop_bridge; stop_stub; start_stub
start_bridge proxy d2.db # startup sweep retries the parked event
wait_for "webhook" yes 60 webhook_present
sent="$(shasum -a 256 "$DATA/d2-sent.json" | cut -d' ' -f1)"
fwd="$(python3 -c "print(open('$DATA/stub/webhook-001.json').read().split(chr(10),1)[0].split('=')[1])")"
[ "$sent" = "$fwd" ] || fail "recovered forward differs: $sent vs $fwd"
stop_bridge; stop_stub

# --- case K: kill after close, before briefing observed ------------------
step "K: kill mid-processing, exactly-once recovery"
start_bridge tap kp.db
[ "$(send_event e2e-kp1 host.appeared appeared-qualified.json)" = 202 ] || fail "kp post"
HIT=0
for _ in $(seq 1 100); do # ~5s at 50ms: close lands, briefing may not have
  if [ "$(group_status_count kp.db processing)" -gt 0 ]; then HIT=1; break; fi
  sleep 0.05
done
[ "$HIT" = 1 ] || pending "K processing state not observed; crash timing unproved"
kill9_bridge
echo "kill-mid-processing: HIT=$HIT"
start_bridge tap kp.db
wait_for "briefings" 1 30 briefing_count
sleep 6 # past another window: recovery must not brief twice
[ "$(briefing_count)" = 1 ] || fail "recovery briefed $HIT-path event twice"
stop_bridge

# --- case K2: kill with incident members pending -------------------------
step "K2: kill before incident tick, exactly-once formation"
register_cap ki.db e2e-ns-1 stable-identity-v1
start_bridge tap ki.db
[ "$(send_event e2e-k1 host.appeared burst-a1.json)" = 202 ] || fail "k2 post 1"
[ "$(send_event e2e-k2 host.appeared burst-a2.json)" = 202 ] || fail "k2 post 2"
[ "$(send_event e2e-k3 host.appeared burst-a3.json)" = 202 ] || fail "k2 post 3"
[ "$(incidents_count)" = 0 ] || pending "K2 incident already formed before crash"
[ "$(db_count ki.db raw_events)" = 3 ] || fail "K2 pending events missing before crash"
kill9_bridge
start_bridge tap ki.db
wait_for "briefings" 3 30 briefing_count
wait_for "incidents" 1 60 incidents_count
KINC="$(curl -s "http://127.0.0.1:$BRIDGE_PORT/api/incidents?limit=100" | python3 -c "import json,sys; rows=json.load(sys.stdin); assert len(rows)==1, rows; print(rows[0]['incident_id'])")"
[ "$(incident_field "$KINC" member_count)" = 3 ] || fail "restart incident members"
stop_bridge

# --- case S: SSE burst ordering and resume -------------------------------
step "S: ordered SSE replay, resume filters stale state"
start_bridge tap sse.db
curl -N -s "http://127.0.0.1:$BRIDGE_PORT/events" >"$DATA/sse.log" 2>&1 &
SSE_PID=$!
sleep 1 # let the stream attach before the burst
[ "$(send_event e2e-s1 host.appeared burst-a1.json)" = 202 ] || fail "sse post 1"
[ "$(send_event e2e-s2 host.appeared burst-a2.json)" = 202 ] || fail "sse post 2"
[ "$(send_event e2e-s3 host.appeared burst-a3.json)" = 202 ] || fail "sse post 3"
[ "$(send_event e2e-s4 host.appeared appeared-qualified.json)" = 202 ] || fail "sse post 4"
[ "$(send_event e2e-s5 host.disappeared disappeared-qualified.json)" = 202 ] || fail "sse post 5"
wait_for "briefings" 5 30 briefing_count
sleep 3 # flush the last frames into the log
kill "$SSE_PID" 2>/dev/null || true
wait "$SSE_PID" 2>/dev/null || true
python3 - "$DATA/sse.log" <<'EOF'
import sys
ids, pending = [], None
for line in open(sys.argv[1]):
    line = line.rstrip("\n")
    if line.startswith("id: "):
        pending = line[4:]
    elif line.startswith("event: "):
        if line == "event: briefing":
            assert pending is not None, "briefing frame without id"
            ids.append(int(pending))
        pending = None
assert [int(x) for x in ids] == [1, 2, 3, 4, 5], ids
print("sse order ok:", ids)
EOF
curl -N -s -H "Last-Event-ID: 3" "http://127.0.0.1:$BRIDGE_PORT/events" >"$DATA/sse-resume.log" 2>&1 &
SSE_PID=$!
sleep 4 # replay of 4,5 plus live edge, then cut
kill "$SSE_PID" 2>/dev/null || true
wait "$SSE_PID" 2>/dev/null || true
python3 - "$DATA/sse-resume.log" <<'EOF'
import sys
ids, pending = [], None
for line in open(sys.argv[1]):
    line = line.rstrip("\n")
    if line.startswith("id: "):
        pending = line[4:]
    elif line.startswith("event: "):
        if line == "event: briefing":
            ids.append(int(pending))
        pending = None
assert ids, "resume delivered no briefing ids"
assert all(i > 3 for i in ids), ids
assert {4, 5} <= set(ids), ids
print("sse resume ok:", ids)
EOF
stop_bridge

fi # E2E_MATRIX_ONLY skips A–S.
# --- matrix MX: mode × relay × identity contract × H1 --------------------
step "MX: complete synthetic capability/mode matrix"
printf 'cell\tmode\trelay\tidentity\th1\thost_status\traw\tbriefings\theartbeats\tclass\n' >"$DATA/matrix.tsv"
MX=0
for mode in tap proxy; do
  for relay in present absent; do
    for identity in supported absent; do
      for h1 in present absent; do
        MX=$((MX + 1))
        db="mx-$MX.db"
        [ "$identity" = supported ] && register_cap "$db" "e2e-mx-ns-$MX" stable-identity-v1
        if [ "$relay" = present ]; then rm -rf "$DATA/stub"; start_stub; fi
        start_bridge "$mode" "$db"
        status="$(send_event "e2e-mx-$MX" host.appeared appeared-provided-e2e.json)"
        want=202
        [ "$mode" = proxy ] && [ "$relay" = absent ] && want=502
        [ "$status" = "$want" ] || fail "MX$MX host status $status, want $want"
        if [ "$h1" = present ]; then
          scanid="$(printf '%032x' "$MX")"
          [ "$(send_hb "$scanid")" = 202 ] || fail "MX$MX heartbeat status"
          wait_for "MX$MX liveness" reporting 30 source_liveness
        else
          wait_for "MX$MX liveness" unknown 30 source_liveness
        fi
        wait_for "MX$MX briefing" 1 30 briefing_count
        [ "$(db_count "$db" raw_events)" = 1 ] || fail "MX$MX raw event count"
        [ "$(db_count "$db" heartbeats)" = "$([ "$h1" = present ] && echo 1 || echo 0)" ] || fail "MX$MX heartbeat count"
        wait_for "MX$MX projected subject" 1 30 subject_count "$db"
        class="$(python3 -c "import sqlite3; c=sqlite3.connect('file:$DATA/$db?mode=ro',uri=True); print(c.execute('select reference_class from reported_subjects').fetchone()[0])")" || fail "MX$MX subject query"
        want_class=event
        # The provided fixture carries no I1 namespace/lifetime contract.
        # Registration alone cannot promote its payload claim to identity.
        [ "$class" = "$want_class" ] || fail "MX$MX class $class, want $want_class"
        if [ "$mode" = proxy ] && [ "$relay" = present ]; then
          [ -f "$DATA/stub/webhook-001.json" ] || fail "MX$MX host not forwarded"
          [ ! -f "$DATA/stub/webhook-002.json" ] || fail "MX$MX heartbeat forwarded"
        fi
        printf '%s\t%s\t%s\t%s\t%s\t%s\t1\t1\t%s\t%s\n' "$MX" "$mode" "$relay" "$identity" "$h1" "$status" "$([ "$h1" = present ] && echo 1 || echo 0)" "$class" >>"$DATA/matrix.tsv"
        stop_bridge
        [ "$relay" = present ] && stop_stub
      done
    done
  done
done
[ "$MX" = 16 ] || fail "matrix did not execute all 16 cells"

fi # E2E_RETENTION_ONLY skips the functional cases before M.
# --- case M: aged db drains bounded, ingest unaffected --------------------
if [ "${E2E_SKIP_LONG:-0}" != 1 ]; then
step "M: purge drains in bounded runs, ingest stays 202"
"$BIN/seed" -db "$DATA/aged.db" -events 600 -subjects 300 -age-days 100
PSEUDO0="$(token_counter aged.db)"
WAL0="$(wal_bytes aged.db)"
B0="$(db_count aged.db briefings)"
rm -rf "$DATA/stub"; start_stub
start_bridge proxy aged.db -purge-interval 1m
[ "$(send_event e2e-m1 host.appeared appeared-qualified.json "$DATA/m1-sent.json")" = 202 ] || fail "mid-purge ingest must stay 202"
wait_for_ge "purge runs" 2 200 purge_completed aged.db
# The 200-row budget is per transaction (retention.go maxPurgeRowsPerTxn);
# per-run totals scale with aged volume, so runs assert completed-only plus
# drain progress, never a per-run row cap.
case " $(purge_statuses aged.db) " in
  *" failed "*) fail "a purge run failed: $(purge_statuses aged.db)";;
esac
[ "$(token_counter aged.db)" -ge "$PSEUDO0" ] || fail "pseudonym counter decreased"
[ "$(token_unique aged.db)" = ok ] || fail "pseudonym token reused"
B1="$(db_count aged.db briefings)"
[ "$B1" -le "$B0" ] || fail "purge grew the briefing table"
sleep 65 # one more interval: drain must progress, never regress
[ "$(db_count aged.db briefings)" -le "$B1" ] || fail "briefing count regressed across runs"
case " $(purge_statuses aged.db) " in
  *" failed "*) fail "a later purge run failed: $(purge_statuses aged.db)";;
esac
WAL1="$(wal_bytes aged.db)"
[ "$WAL1" -le 67108864 ] || fail "WAL exceeded 64MiB: $WAL1"
echo "purge: B0=$B0 B1=$B1 WAL0=$WAL0 WAL1=$WAL1 maxrun_total=$(purge_maxrun aged.db) (per-run total, informational; budget is per-txn) statuses: $(purge_statuses aged.db)"
[ -f "$DATA/stub/webhook-001.json" ] || fail "proxy forward stalled during purge"
stop_bridge; stop_stub

# --- case KP2: kill mid-purge, interrupted accounting ---------------------
step "KP2: kill mid-purge, interrupted run recorded, drain completes"
"$BIN/seed" -db "$DATA/aged2.db" -events 600 -subjects 300 -age-days 100
start_bridge tap aged2.db -purge-interval 1m
PHIT=0
for _ in $(seq 1 375); do # ~75s at 200ms: catch a running pass
  if [ "$(purge_running_seen aged2.db)" = yes ]; then PHIT=1; break; fi
  sleep 0.2
done
[ "$PHIT" = 1 ] || pending "KP2 running purge not observed; mid-purge crash unproved"
kill9_bridge
echo "kill-mid-purge: HIT=$PHIT"
start_bridge tap aged2.db -purge-interval 1m
wait_for_ge "purge runs" 1 200 purge_completed aged2.db
case " $(purge_statuses aged2.db) " in
  *" failed "*) fail "a purge run failed: $(purge_statuses aged2.db)";;
esac
[ "$(token_unique aged2.db)" = ok ] || fail "token map unsound after purge kill"
stop_bridge

fi # The long retention soak can be run separately from functional regressions.
fi # E2E_MIG_ONLY runs the migration tail without repeating the long scenarios.

if [ "${E2E_RETENTION_ONLY:-0}" != 1 ] && [ "${E2E_SKIP_MIG:-0}" != 1 ]; then
# --- case MIG: supported lineage migrates with bytes intact ---------------
step "MIG: v13 lineage migrates, raw bytes and counts preserved"
"$BIN/seed" -db "$DATA/mig.db" -events 60 -subjects 30 -age-days 10 -downgrade v13
[ "$(db_version mig.db)" = 13 ] || fail "downgrade did not land"
python3 "$ROOT/e2e/snapshot.py" capture "$DATA/mig.db" "$DATA/mig-before.json"
RAW0="$(db_count mig.db raw_events)"
BRF0="$(db_count mig.db briefings)"
T0="$(date +%s)"
start_bridge tap mig.db
[ "$(db_version mig.db)" = 31 ] || fail "migration did not reach v31"
python3 "$ROOT/e2e/snapshot.py" verify "$DATA/mig.db" "$DATA/mig-before.json" || fail "migration preservation"
[ "$(db_count mig.db raw_events)" = "$RAW0" ] || fail "migration lost raw events"
[ "$(db_count mig.db briefings)" = "$BRF0" ] || fail "migration lost briefings"
[ "$(table_exists mig.db incidents)" = yes ] || fail "incident schema missing after migration"
echo "migration: ${RAW0} events, ${BRF0} briefings, $(( $(date +%s) - T0 ))s wall"
stop_bridge

# --- case MIGK: kill during first startup, idempotent recovery ------------
step "MIGK: kill during migrate, restart recovers without loss"
"$BIN/seed" -db "$DATA/migk.db" -events 60 -subjects 30 -age-days 10 -downgrade v13
python3 "$ROOT/e2e/snapshot.py" capture "$DATA/migk.db" "$DATA/migk-before.json"
"$BIN/bridge" -bind "127.0.0.1:$BRIDGE_PORT" -mode tap -db "$DATA/migk.db" \
  -secret "$SECRET" -scanrelay-url "http://127.0.0.1:$STUB_PORT" >"$DATA/bridge-migk.db.log" 2>&1 &
BRIDGE_PID=$!
MIG_HIT=0
for _ in $(seq 1 100); do
  version="$(db_version migk.db 2>/dev/null || echo 0)"
  if kill -0 "$BRIDGE_PID" 2>/dev/null && [ "$version" -gt 13 ] && [ "$version" -lt 31 ]; then MIG_HIT=1; break; fi
  sleep 0.01
done
[ "$MIG_HIT" = 1 ] || pending "MIGK intermediate migration version not observed while bridge ran; interruption unproved"
kill9_bridge
start_bridge tap migk.db
[ "$(db_version migk.db)" = 31 ] || fail "recovery did not reach v31"
python3 "$ROOT/e2e/snapshot.py" verify "$DATA/migk.db" "$DATA/migk-before.json" || fail "migration restart preservation"
[ "$(db_count migk.db raw_events)" = 60 ] || fail "recovery lost raw events"
stop_bridge

# --- case NEGMIG: unsupported schema refused, bytes untouched -------------
step "NEGMIG: user_version 9999 refused, file unchanged"
python3 -c "import sqlite3; c=sqlite3.connect('$DATA/neg.db'); c.execute('pragma user_version=9999'); c.commit()"
SHA0="$(shasum -a 256 "$DATA/neg.db" | cut -d' ' -f1)"
"$BIN/bridge" -bind "127.0.0.1:$BRIDGE_PORT" -mode tap -db "$DATA/neg.db" \
  -secret "$SECRET" -scanrelay-url "http://127.0.0.1:$STUB_PORT" >"$DATA/neg.log" 2>&1 &
NEG_PID=$!
sleep 3
if kill -0 "$NEG_PID" 2>/dev/null; then
  kill -9 "$NEG_PID" 2>/dev/null || true
  fail "bridge accepted an unsupported schema version"
fi
wait "$NEG_PID" 2>/dev/null || true
grep -q "newer than supported" "$DATA/neg.log" || fail "refusal reason missing from log"
[ "$(shasum -a 256 "$DATA/neg.db" | cut -d' ' -f1)" = "$SHA0" ] || fail "refused database was modified"
fi # E2E_RETENTION_ONLY skips migration cases.

bundle="$ROOT/e2e/results/e2e-$(date -u +%Y%m%dT%H%M%SZ)-$$"
mkdir -p "$ROOT/e2e/results"
cp -r "$DATA" "$bundle"
if [ -s "$PENDING" ]; then
  cp "$PENDING" "$ROOT/.e2e-pending.txt"
  echo "E2E FUNCTIONAL CHECKS PASSED; crash targets pending (see $bundle/pending.txt)"
else
  echo "E2E FUNCTIONAL CHECKS PASSED; evidence at $bundle"
fi
