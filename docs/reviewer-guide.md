# Reviewer guide: run WatcherBridge and send test data

This page is for hackathon reviewers and testers. It covers building the
bridge, running it locally, and sending it data with or without
SubnetLens Pro. Operational details (delivery modes, configuration
reference) live in [README.md](../README.md).

## 1. Prerequisites

- Go 1.26 or newer and a C compiler (Xcode Command Line Tools on macOS,
  GCC on Linux). The committed viewer assets are prebuilt; Node.js is not
  needed to run the bridge.
- Bridge and event sender must run on the **same machine**: the bridge
  listens on loopback only (`127.0.0.1:8081` by default).
- SubnetLens Pro is a separate proprietary app and is not in this
  repository. Its macOS binary is not code-signed: on first launch,
  right-click it, choose Open, and confirm once. It is then whitelisted.

## 2. Build and start

```sh
git clone https://github.com/ostefani/WatcherBridge.git && cd WatcherBridge
CGO_ENABLED=1 go build -o watcherbridge ./cmd/watcherbridge

mkdir -m 700 demo-data
openssl rand -hex 32 > demo-data/webhook.secret
chmod 600 demo-data/webhook.secret

./watcherbridge -secret-file ./demo-data/webhook.secret \
  -db ./demo-data/demo.db -scanrelay-url=
```

This runs in tap mode, standalone with no relay dependency
(`-scanrelay-url=` disables the optional relay integration). Open
`http://127.0.0.1:8081/`. Without a model key everything works with
clearly marked deterministic fallback briefings, so AI credentials are
optional for a first pass.

WatcherBridge works alongside any existing relay or event consumer: in
tap mode the sender delivers to the bridge and to the consumer
independently, and in proxy mode (`-mode proxy`) the bridge forwards the
original event bytes onward. Either way the bridge never modifies the
sender's reports.

## 3. Sending data

### With SubnetLens Pro

Paste the same secret into Pro's webhook settings and point delivery at
`http://127.0.0.1:8081/webhook`. Generate activity on the watched network;
reports arrive as signed webhooks.

### Without Pro: the webhook contract

`POST /webhook` with `Content-Type: application/json` and these headers:

| Header | Value |
|---|---|
| `X-SubnetLens-Event-ID` | unique ID per event |
| `X-SubnetLens-Event-Key` | sender key/scope |
| `X-SubnetLens-Event-Type` | `host.appeared`, `host.changed`, `host.disappeared`, or `scan.heartbeat` |
| `X-SubnetLens-Signature-Timestamp` | current Unix seconds (must be within **240 seconds** of the bridge clock) |
| `X-SubnetLens-Signature-256` | `sha256=` + hex HMAC-SHA256 of `timestamp.id.key.type.rawbody`, keyed with the shared secret |

The JSON body must echo the same `event_id`/`event_key`/`event_type`
(the bridge rejects split envelopes). Example body:

```json
{"event_id":"demo-1","event_key":"demo-key","event_type":"host.appeared",
 "occurred_at":"2026-11-04T12:00:00Z","target":"192.0.2.0/24",
 "host":{"ip":"192.0.2.10","mac":"aa:bb:cc:dd:ee:10","hostname":"demo-laptop",
  "host_id":"mac:aa:bb:cc:dd:ee:10","identity_source":"mac",
  "identity_confidence":"high","alive":true}}
```

Minimal dependency-free sender:

```sh
SECRET=$(cat demo-data/webhook.secret)
python3 - "$SECRET" <<'EOF'
import hashlib, hmac, json, sys, time, urllib.request
body = json.dumps({"event_id":"demo-1","event_key":"demo-key","event_type":"host.appeared",
 "occurred_at":"2026-11-04T12:00:00Z","target":"192.0.2.0/24",
 "host":{"ip":"192.0.2.10","mac":"aa:bb:cc:dd:ee:10","hostname":"demo-laptop",
 "host_id":"mac:aa:bb:cc:dd:ee:10","identity_source":"mac",
 "identity_confidence":"high","alive":True}}).encode()
ts = str(int(time.time()))
sig = "sha256=" + hmac.new(sys.argv[1].encode(), f"{ts}.demo-1.demo-key.host.appeared.".encode()+body, hashlib.sha256).hexdigest()
req = urllib.request.Request("http://127.0.0.1:8081/webhook", data=body,
 {"Content-Type":"application/json","X-SubnetLens-Event-ID":"demo-1",
  "X-SubnetLens-Event-Key":"demo-key","X-SubnetLens-Event-Type":"host.appeared",
  "X-SubnetLens-Signature-Timestamp":ts,"X-SubnetLens-Signature-256":sig})
print("status:", urllib.request.urlopen(req).status)
EOF
```

Status meanings: `202` stored; `401` bad signature or unknown secret;
`400` unknown event type, split envelope, or stale timestamp (check clock
skew). A briefing appears in the viewer after the grouping window
(~45 seconds by default).

## 4. Suggested demo tour

1. Overview verdict (quiet / review / act) and a briefing detail page:
   what changed, priority reasoning, recommended check.
2. Review actions: mark a briefing expected, snooze it, add a note. All
   stay local; the original scanner report is never modified.
3. Health screen: ingest outcomes, delivery gaps, retention activity.
4. Reset any time: stop the bridge, `rm -f demo-data/demo.db*`, restart.

## 5. Enabling AI explanations (optional)

```sh
./watcherbridge -secret-file ./demo-data/webhook.secret \
  -db ./demo-data/demo.db -scanrelay-url= \
  -model-provider meta -model-id '<your-model-id>' \
  -model-base-url '<your-https-responses-endpoint>' \
  -model-key-file /absolute/path/to/model-api-key
```

Add `-agent-enabled` to opt into the retained-evidence investigator
(five fixed read-only tools, 90-second run budget). On slow networks,
provider calls may need larger budgets, e.g. `-provider-timeout 3m
-agent-timeout 8m -processing-lease 17m`; without them, slow calls
surface as marked fallback briefings instead of errors.

## 6. Notes for the thorough

- The webhook secret must be at least 32 bytes; the bridge refuses to
  start otherwise.
- Keep redaction enabled (the default) whenever a model key is
  configured: identifying values are tokenized before any provider call.
- Synthetic fixtures used by the project's own end-to-end suite live in
  `e2e/fixtures/` and follow exactly the contract in section 3.
