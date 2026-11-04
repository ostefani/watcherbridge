#!/usr/bin/env python3
"""Send one synthetic signed webhook to a local WatcherBridge instance."""

import hashlib
import hmac
import json
import sys
import time
import urllib.request


def main() -> int:
    if len(sys.argv) != 2:
        print(f"usage: {sys.argv[0]} PATH_TO_WEBHOOK_SECRET", file=sys.stderr)
        return 2

    with open(sys.argv[1], encoding="ascii") as secret_file:
        secret = secret_file.read().strip()

    event_id = f"review-demo-{time.time_ns()}"
    event_key = "review-demo"
    event_type = "host.appeared"
    body = json.dumps(
        {
            "event_id": event_id,
            "event_key": event_key,
            "event_type": event_type,
            "occurred_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "target": "192.0.2.0/24",
            "host": {
                "ip": "192.0.2.10",
                "mac": "02:00:00:00:00:10",
                "hostname": "review-demo",
                "host_id": f"mock:{event_id}",
                "identity_source": "mock",
                "identity_confidence": "high",
                "alive": True,
            },
        },
        separators=(",", ":"),
    ).encode()

    timestamp = str(int(time.time()))
    signed = f"{timestamp}.{event_id}.{event_key}.{event_type}.".encode() + body
    signature = "sha256=" + hmac.new(secret.encode(), signed, hashlib.sha256).hexdigest()
    headers = {
        "Content-Type": "application/json",
        "X-SubnetLens-Event-ID": event_id,
        "X-SubnetLens-Event-Key": event_key,
        "X-SubnetLens-Event-Type": event_type,
        "X-SubnetLens-Signature-Timestamp": timestamp,
        "X-SubnetLens-Signature-256": signature,
    }
    request = urllib.request.Request("http://127.0.0.1:8081/webhook", data=body, headers=headers)
    with urllib.request.urlopen(request) as response:
        print("Synthetic event accepted:", response.status)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
