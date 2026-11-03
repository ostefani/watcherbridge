#!/bin/sh
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
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DATA="$(mktemp -d "${TMPDIR:-/tmp}/wb-load.XXXXXX")"
SECRET="e2e-load-secret-0123456789abcdef"
BRIDGE_PORT=18081
STUB_PORT=18080
RES="$ROOT/e2e/results/$(date -u +%Y%m%dT%H%M%SZ)"
BIN="$ROOT/.e2e-bin"
export GOCACHE="${GOCACHE:-$(go env GOCACHE)}"

cleanup() {
  [ -f "$DATA/samplers" ] && while read -r pid; do kill "$pid" 2>/dev/null || true; done <"$DATA/samplers"
  rm -rf "$DATA"
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
    echo "heap=Go live heap parsed from GODEBUG=gctrace stderr (GC-paced sampling, stated limitation)"
  } >"$RES/env.txt"
  cat "$RES/env.txt"
}

build_all() {
  (cd "$ROOT" && go build -o "$BIN/bridge" ./cmd/watcherbridge \
    && go build -o "$BIN/stubrelay" ./e2e/stubrelay \
    && go build -o "$BIN/loadgen" ./e2e/loadgen) || fail "build"
}

start_stub() {
  rm -rf "$DATA/stub"
  (cd "$DATA" && "$BIN/stubrelay" >stub.log 2>&1 & echo $! >stub.pid)
  sleep 1
}

stop_stub() { [ -f "$DATA/stub.pid" ] && kill "$(cat "$DATA/stub.pid")" 2>/dev/null || true; }

start_bridge() { # mode dbname (log appends: sweep restarts share it)
  GODEBUG=gctrace=1 "$BIN/bridge" -bind "127.0.0.1:$BRIDGE_PORT" -mode "$1" -db "$DATA/$2" \
    -secret "$SECRET" -scanrelay-url "http://127.0.0.1:$STUB_PORT" >>"$DATA/bridge-$2.log" 2>&1 &
  echo $! >"$DATA/bridge.pid"
  for _ in $(seq 1 100); do
    sleep 0.1
    curl -sf -o /dev/null "http://127.0.0.1:$BRIDGE_PORT/" 2>/dev/null && return 0
  done
  fail "bridge did not come up ($1 $2)"
}

stop_bridge() { [ -f "$DATA/bridge.pid" ] && kill "$(cat "$DATA/bridge.pid")" 2>/dev/null || true; sleep 1; }

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
  ( while kill -0 "$(cat "$DATA/bridge.pid")" 2>/dev/null; do
      wc -c <"$DATA/$1-wal" 2>/dev/null || echo 0
      sleep 5
    done >"$DATA/$2" 2>&1 & )
  echo $! >>"$DATA/samplers"
}

echo "### load env"; record_env
echo "### build"; build_all

FIX="$ROOT/e2e/fixtures/appeared-qualified.json,$ROOT/e2e/fixtures/changed-qualified.json"
MANIFEST="$RES/forwarded-manifest.txt"
: >"$MANIFEST"

drive() { # phase url db : burst or sustained per $phase prefix
  case "$1" in
    *-burst) "$BIN/loadgen" -url "$2" -secret "$SECRET" -mode burst -n 512 -workers 16 \
      -fixtures "$FIX" -prefix "ld-$1" -out "$RES/$1.jsonl" | tee "$RES/$1.summary";;
    *-sust) "$BIN/loadgen" -url "$2" -secret "$SECRET" -mode sustained -rate 50 -duration 60s -workers 8 \
      -fixtures "$FIX" -prefix "ld-$1" -out "$RES/$1.jsonl" | tee "$RES/$1.summary";;
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
  echo "proxy-$rep forwarded=$(ls "$DATA/stub"/webhook-*.json 2>/dev/null | wc -l)" | tee -a "$MANIFEST"
  start_bridge proxy "proxy-$rep.db" # startup sweep drains stragglers
  sleep 5; stop_bridge; stop_stub
  echo "proxy-$rep forwarded-after-sweep=$(ls "$DATA/stub"/webhook-*.json 2>/dev/null | wc -l)" | tee -a "$MANIFEST"
  # --- direct-to-stub baselines (bridge overhead reference) --------------
  start_stub
  drive "direct-burst-$rep" "http://127.0.0.1:$STUB_PORT/webhook" _
  drive "direct-sust-$rep" "http://127.0.0.1:$STUB_PORT/webhook" _
  stop_stub
done

echo "### verdicts"
python3 - "$RES" "$DATA" <<'EOF'
import json, glob, os, re, sys
resdir, datadir = sys.argv[1], sys.argv[2]

def summary(phase):
    with open(os.path.join(resdir, phase + ".summary")) as f:
        return json.load(f)

def heap_live_max_mb(bridgelog):
    best = 0.0
    with open(bridgelog, errors="replace") as f:
        for line in f:
            m = re.search(r"(\d+\.\d+)->(\d+\.\d+)->(\d+\.\d+) MB", line)
            if m:
                best = max(best, float(m.group(3)))
    return best or None

def wal_max_kb(path):
    with open(path) as f:
        vals = [int(x.strip() or 0) for x in f]
    return (max(vals) / 1024) if vals else 0

def stored(db):
    import sqlite3
    c = sqlite3.connect(f"file:{datadir}/{db}?mode=ro", uri=True)
    return c.execute("select count(*) from raw_events").fetchone()[0]

def pctl(path, p):
    xs = []
    with open(path) as f:
        for line in f:
            r = json.loads(line)
            xs.append(r["lat_ms"])
    xs.sort()
    return xs[min(int(p * len(xs)), len(xs) - 1)] if xs else -1

verdicts, breaches = [], []
for rep in ("1", "2", "3"):
    for workload, n in (("burst", 512), ("sust", 3000)):
        tap = summary(f"tap-{workload}-{rep}")
        direct = summary(f"direct-{workload}-{rep}")
        proxy = summary(f"proxy-{workload}-{rep}")
        tap_p99 = pctl(f"{resdir}/tap-{workload}-{rep}.jsonl", 0.99)
        direct_p99 = pctl(f"{resdir}/direct-{workload}-{rep}.jsonl", 0.99)
        proxy_p99 = pctl(f"{resdir}/proxy-{workload}-{rep}.jsonl", 0.99)
        overhead = proxy_p99 - direct_p99
        checks = {
            "tap_zero_unexplained_loss": tap["accepted_202"] == n and tap["dropped_unsent"] == 0,
            "tap_p99_le_50ms": tap_p99 <= 50,
            "proxy_overhead_p99_le_25ms": overhead <= 25,
        }
        stored_n = stored(f"tap-{rep}.db") if workload == "sust" else None
        if workload == "sust":
            # Tap db accumulates burst 512 + sustained 3000 per rep.
            checks["stored_matches_attempts"] = stored_n == 512 + 3000
        v = {"rep": rep, "workload": workload,
             "tap": tap, "direct": direct, "proxy": proxy,
             "tap_p99_ms": tap_p99, "direct_p99_ms": direct_p99,
             "proxy_p99_ms": proxy_p99, "proxy_overhead_p99_ms": overhead,
             "stored_tap_db": stored_n, "checks": checks}
        verdicts.append(v)
        for name, ok in checks.items():
            if not ok:
                breaches.append(f"{workload} rep{rep}: {name} FAILED {json.dumps(v)}")
    heap = heap_live_max_mb(f"{datadir}/bridge-proxy-{rep}.db.log")
    wal = wal_max_kb(f"{datadir}/wal-proxy-{rep}.txt")
    verdicts.append({"rep": rep, "resource": {"heap_live_max_mb": heap, "wal_max_kb": wal,
        "heap_le_64mib": heap is not None and heap <= 64,
        "wal_le_64mib": wal <= 65536}})
    if heap is None or heap > 64:
        breaches.append(f"rep{rep}: heap budget FAILED (live max {heap} MiB)")
    if wal > 65536:
        breaches.append(f"rep{rep}: WAL budget FAILED ({wal} KiB)")

with open(os.path.join(resdir, "verdicts.json"), "w") as f:
    json.dump(verdicts, f, indent=1)
print(f"verdicts: {len(verdicts)} records at {resdir}/verdicts.json")
if breaches:
    print("BUDGET BREACHES:")
    print("\n".join(breaches))
    sys.exit(3)
print("ALL BUDGETS HELD")
EOF
echo "LOAD DONE: results at $RES"
