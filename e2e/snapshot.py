#!/usr/bin/env python3
"""Capture and compare immutable seeded data across a forward migration."""
import base64
import hashlib
import json
import sqlite3
import sys


def digest(db, table, columns):
    quoted = ",".join('"' + c.replace('"', '""') + '"' for c in columns)
    rows = db.execute(f'SELECT {quoted} FROM "{table}" ORDER BY rowid')
    h = hashlib.sha256()
    count = 0
    for row in rows:
        encoded = [({"bytes": base64.b64encode(v).decode("ascii")} if isinstance(v, bytes) else v) for v in row]
        h.update(json.dumps(encoded, ensure_ascii=True, separators=(",", ":")).encode())
        h.update(b"\n")
        count += 1
    return {"count": count, "sha256": h.hexdigest(), "columns": columns}


def snapshot(path, prior=None):
    db = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    tables = ("raw_events", "briefings", "pseudonym_counters")
    out = {}
    for table in tables:
        columns = (prior[table]["columns"] if prior else
                   [row[1] for row in db.execute(f'PRAGMA table_info("{table}")')])
        if not columns:
            raise RuntimeError(f"missing {table}")
        out[table] = digest(db, table, columns)
    return out


if __name__ == "__main__":
    action, path, artifact = sys.argv[1:4]
    if action == "capture":
        with open(artifact, "w") as file:
            json.dump(snapshot(path), file, indent=2)
    elif action == "verify":
        with open(artifact) as file:
            expected = json.load(file)
        actual = snapshot(path, expected)
        if actual != expected:
            raise SystemExit(f"migration changed preserved data: {actual} != {expected}")
        print("migration preserved raw events, briefing fields, and pseudonym counters")
    else:
        raise SystemExit("usage: snapshot.py capture|verify DB ARTIFACT")
