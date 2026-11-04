# Reviewer guide: run WatcherBridge and send test data

This page is for hackathon reviewers and testers. It covers building the
bridge and sending it either synthetic data or SubnetLens Pro reports.
The synthetic route is the default recommendation: it needs no Pro binary
and performs no network scan. Operational details live in
[README.md](../README.md).

## 1. Prerequisites

- Go 1.26 or newer and a C compiler supported by Go cgo (Xcode Command
  Line Tools on macOS, GCC on Linux, or MinGW-w64 on Windows). The bridge
  uses cgo for SQLite.
  The committed viewer assets are prebuilt; Node.js is not needed.
- Bridge and event sender must run on the **same machine**: the bridge
  listens on loopback only (`127.0.0.1:8081` by default).
- The optional SubnetLens Pro binaries and reviewer license are in the
  [reviewer-preview-1 release](https://github.com/ostefani/watcherbridge/releases/tag/reviewer-preview-1).
  Choose Windows amd64, macOS arm64 (Apple silicon), macOS amd64 (Intel),
  or Linux amd64 as appropriate, and download `reviewer-license.zip`.
- The Pro binaries are unsigned. macOS, Windows, or endpoint security
  software may warn or block them. You do not need to override those
  protections: use the synthetic-webhook instructions below instead.

## 2. Build and start the bridge

The following commands use a POSIX shell (macOS/Linux). On Windows,
install Go and a cgo-compatible C compiler, then use the PowerShell
commands below. The bridge listens on loopback on every platform.

```sh
git clone https://github.com/ostefani/WatcherBridge.git && cd WatcherBridge
CGO_ENABLED=1 go build -o watcherbridge ./cmd/watcherbridge

mkdir -m 700 demo-data
openssl rand -hex 32 > demo-data/webhook.secret
chmod 600 demo-data/webhook.secret

./watcherbridge -secret-file ./demo-data/webhook.secret \
  -db ./demo-data/demo.db -scanrelay-url=
```

Windows PowerShell, from the repository root (with Go 1.26+ and
MinGW-w64 GCC installed and available on `PATH`):

```powershell
New-Item -ItemType Directory -Force .\demo-data | Out-Null
$bytes = New-Object byte[] 32
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($bytes)
$rng.Dispose()
$secret = [BitConverter]::ToString($bytes).Replace('-', '').ToLowerInvariant()
Set-Content -Path .\demo-data\webhook.secret -Value $secret -NoNewline -Encoding ascii
$env:CGO_ENABLED = '1'
go build -o watcherbridge.exe .\cmd\watcherbridge
$env:WEBHOOK_SECRET = $secret
.\watcherbridge.exe -db .\demo-data\demo.db -scanrelay-url=
```

Keep the bridge running. In another PowerShell window the mock sender
reads the secret file directly; use the separate Pro config steps below
when running the scanner.

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

Leave the bridge running in this terminal. Open `http://127.0.0.1:8081/`
in a browser on the same machine, then use one of the two sender options
below in another terminal.

## 3. Sending data

### Synthetic event (no Pro, no network scan)

This sends one local, synthetic `host.appeared` event to the bridge. It
does not execute SubnetLens Pro or probe a network. The default fake model
provider makes no external AI calls; the resulting briefing is marked as
a deterministic fallback.

From the repository root, run the included standard-library Python
sender. It works on macOS and Linux; Windows PowerShell users can use
the `py` launcher:

```sh
python3 docs/reviewer-mock.py demo-data/webhook.secret
```

```powershell
py .\docs\reviewer-mock.py .\demo-data\webhook.secret
```

The command should print `Synthetic event accepted: 202`. Each run uses
a new event ID. Allow the 45-second grouping quiet window for the
briefing to appear.

### With SubnetLens Pro

Download the matching Pro archive and `reviewer-license.zip` from the
release linked above. Extract them into a `pro/` directory under the
WatcherBridge repository root so these files are available:
`pro/subnetlens-pro` (or `pro/subnetlens-pro.exe`) and
`pro/reviewer.license`. Pass the license path explicitly for both
verification and watch mode; the default license filename is
`subnetlens.license`, not `reviewer.license`. On Linux, make the binary
executable with `chmod +x pro/subnetlens-pro`. On macOS, right-click the
unsigned binary and choose **Open** if Gatekeeper blocks the first launch.
Use your own subnet or one you are authorized to scan. The synthetic
route above needs none of these binaries and performs no scan.

From the repository root, create a locked-down Pro webhook config that
uses the same bridge secret:

```sh
cat > demo-data/bridge.yaml <<EOF
webhook_secret: $(cat demo-data/webhook.secret)
endpoints:
  - url: http://127.0.0.1:8081/webhook
    timeout: 10s
EOF
chmod 600 demo-data/bridge.yaml
```

Do not start a second bridge; keep the one from section 2 running. From
the repository root, open a new terminal and run the command for your
platform:

```sh
./pro/subnetlens-pro -license ./pro/reviewer.license -license-status
./pro/subnetlens-pro -subnet 192.168.1.0/24 -watch -watch-interval 10s \
  -watch-webhook-config "$PWD/demo-data/bridge.yaml" -license ./pro/reviewer.license
```

For Windows PowerShell, run:

```powershell
.\pro\subnetlens-pro.exe -license .\pro\reviewer.license -license-status
.\pro\subnetlens-pro.exe -subnet 192.168.1.0/24 -watch -watch-interval 10s `
  -watch-webhook-config (Resolve-Path .\demo-data\bridge.yaml).Path -license .\pro\reviewer.license
```

Replace the example subnet. Watch interval and scan duration add to the
bridge's 45-second grouping window, so results may take longer than 45
seconds after network activity.

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

The included `docs/reviewer-mock.py` sender constructs this envelope,
generates a fresh timestamp and event ID, signs the exact request body,
and posts to loopback. Use that command for the reviewer demo; it avoids
copying a stale timestamp or reusing an event ID.

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
