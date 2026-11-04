#!/usr/bin/env python3
"""Evaluate a completed Step 31 synthetic load run."""
import json, glob, os, re, sys

def summary(phase, resdir):
    with open(os.path.join(resdir, phase + ".summary")) as f:
        return json.load(f)

def heap_peak_mb(bridgelog):
    best = 0.0
    samples = 0
    with open(bridgelog, errors="replace") as f:
        for line in f:
            m = re.search(r"(\d+(?:\.\d+)?)->(\d+(?:\.\d+)?)->(\d+(?:\.\d+)?) MB", line)
            if m:
                samples += 1
                best = max(best, *(float(m.group(i)) for i in (1, 2, 3)))
    return best if samples else None, samples

def wal_max_kb(path):
    with open(path) as f:
        vals = [int(x.strip() or 0) for x in f]
    return (max(vals) / 1024) if vals else None, len(vals)

def stored(db, phase, datadir):
    import sqlite3
    c = sqlite3.connect(f"file:{datadir}/{db}?mode=ro", uri=True)
    return c.execute("select count(*) from raw_events where event_id like ?", (f"ld-{phase}-%",)).fetchone()[0]

def pctl(path, p):
    xs = []
    with open(path) as f:
        for line in f:
            r = json.loads(line)
            xs.append(r["lat_ms"])
    xs.sort()
    return xs[min(int(p * len(xs)), len(xs) - 1)] if xs else -1

def paired_proxy_delta_p99(proxy_path, direct_path):
    # Both phases offer the same fixture and reference at each index with
    # the same worker/rate settings. Match by index, then take p99 of
    # per-index proxy-minus-direct latency. Event IDs differ by mode
    # prefix, and these are sequential runs, so this is a matched-workload
    # estimate with small payload-shape and run-order noise.
    def read(path):
        rows = {}
        with open(path) as f:
            for line in f:
                row = json.loads(line)
                if row["status"] != 202:
                    continue
                index = int(row["event_id"].rsplit("-", 1)[1])
                if index in rows:
                    raise ValueError(f"duplicate load index {index} in {path}")
                rows[index] = row["lat_ms"]
        return rows
    proxy, direct = read(proxy_path), read(direct_path)
    if not proxy or proxy.keys() != direct.keys():
        return None
    deltas = sorted(proxy[index] - direct[index] for index in proxy)
    import math
    return deltas[math.ceil(0.99 * len(deltas)) - 1]

def main():
    resdir, datadir = sys.argv[1], sys.argv[2]
    verdicts, breaches, pending = [], [], []
    for rep in ("1", "2", "3"):
        for workload, n in (("burst", 512), ("sust", 3000)):
            tap = summary(f"tap-{workload}-{rep}", resdir)
            direct = summary(f"direct-{workload}-{rep}", resdir)
            proxy = summary(f"proxy-{workload}-{rep}", resdir)
            tap_p99 = pctl(f"{resdir}/tap-{workload}-{rep}.jsonl", 0.99)
            direct_p99 = pctl(f"{resdir}/direct-{workload}-{rep}.jsonl", 0.99)
            proxy_p99 = pctl(f"{resdir}/proxy-{workload}-{rep}.jsonl", 0.99)
            overhead = paired_proxy_delta_p99(f"{resdir}/proxy-{workload}-{rep}.jsonl",
                                               f"{resdir}/direct-{workload}-{rep}.jsonl")
            if overhead is None:
                pending.append(f"{workload} rep{rep}: proxy/direct successful event indexes did not match")
            tap_stored = stored(f"tap-{rep}.db", f"tap-{workload}-{rep}", datadir)
            proxy_stored = stored(f"proxy-{rep}.db", f"proxy-{workload}-{rep}", datadir)
            checks = {
                "tap_delivery_accounted": tap["offered"] == n and tap["sent"] == n and tap["accepted_202"] == n and tap["dropped_unsent"] == 0 and tap_stored == n,
                "proxy_delivery_accounted": proxy["offered"] == n and proxy["sent"] == n and proxy["accepted_202"] == n and proxy["dropped_unsent"] == 0 and proxy_stored == n,
                "tap_p99_le_50ms": 0 <= tap_p99 <= 50,
            }
            if overhead is not None:
                checks["paired_proxy_delta_p99_le_25ms"] = overhead <= 25
            v = {"rep": rep, "workload": workload,
                 "tap": tap, "direct": direct, "proxy": proxy,
                 "tap_p99_ms": tap_p99, "direct_p99_ms": direct_p99,
                 "proxy_p99_ms": proxy_p99, "paired_proxy_delta_p99_ms": overhead,
                 "stored_tap_phase": tap_stored, "stored_proxy_phase": proxy_stored, "checks": checks}
            verdicts.append(v)
            for name, ok in checks.items():
                if not ok:
                    breaches.append(f"{workload} rep{rep}: {name} FAILED {json.dumps(v)}")
        import collections
        expected = collections.Counter()
        for workload in ("burst", "sust"):
            with open(f"{resdir}/proxy-{workload}-{rep}.jsonl") as f:
                expected.update(json.loads(line)["body_sha256"] for line in f if json.loads(line)["status"] == 202)
        actual = collections.Counter()
        for path in glob.glob(f"{resdir}/proxy-{rep}-stub/webhook-*.json"):
            with open(path, "rb") as f:
                actual.update([f.readline().decode().strip().removeprefix("sha256=")])
        forwarded_ok = actual == expected
        verdicts.append({"rep": rep, "forwarded_byte_identity_and_count": forwarded_ok,
                         "expected_forwarded": sum(expected.values()), "observed_forwarded": sum(actual.values())})
        if not forwarded_ok:
            breaches.append(f"rep{rep}: forwarded bytes/count do not match accepted proxy requests")
        for mode in ("tap", "proxy"):
            heap, samples = heap_peak_mb(f"{datadir}/bridge-{mode}-{rep}.db.log")
            wal, wal_samples = wal_max_kb(f"{datadir}/wal-{mode}-{rep}.txt")
            verdicts.append({"rep": rep, "mode": mode, "resource": {"heap_gc_peak_mb": heap,
                "heap_gc_samples": samples, "wal_sample_max_kib": wal, "wal_samples": wal_samples,
                "heap_observed_samples_le_64mib": heap is not None and heap <= 64,
                "wal_observed_samples_le_64mib": wal is not None and wal <= 65536}})
            if heap is None:
                pending.append(f"{mode} rep{rep}: no GC heap sample")
            elif heap > 64:
                breaches.append(f"{mode} rep{rep}: observed GC heap peak {heap} MiB exceeds 64 MiB")
            pending.append(f"{mode} rep{rep}: GC-paced heap samples do not bound the complete interval")
            if wal is None:
                pending.append(f"{mode} rep{rep}: no WAL sample")
            elif wal > 65536:
                breaches.append(f"{mode} rep{rep}: WAL maximum {wal} KiB exceeds 65536 KiB")
            pending.append(f"{mode} rep{rep}: five-second WAL samples do not bound between-sample peaks or checkpoint recovery")

    with open(os.path.join(resdir, "verdicts.json"), "w") as f:
        json.dump({"status": "fail" if breaches else "pending" if pending else "pass",
                   "verdicts": verdicts, "breaches": breaches, "pending": pending}, f, indent=1)
    print(f"verdicts: {len(verdicts)} records at {resdir}/verdicts.json")
    if breaches:
        print("BUDGET BREACHES:")
        print("\n".join(breaches))
        sys.exit(3)
    print("MEASURED CHECKS HELD; RELEASE BUDGETS PENDING:")
    print("\n".join(pending))

if __name__ == "__main__":
    main()
