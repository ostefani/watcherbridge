# Reviewer guide: run WatcherBridge and send test data

This page is for hackathon reviewers and testers. It covers building the
bridge and sending it either synthetic data or SubnetLens Pro reports.
Start with the synthetic demo: it needs no scanner or AI credentials and
performs no network scan. For the optional scanner demo, download SubnetLens
Pro and the reviewer license from the
[reviewer-preview-1 release](https://github.com/ostefani/watcherbridge/releases/tag/review).
**These Pro binaries are unsigned.** SubnetLens Pro is proprietary; its
source code is not part of this submission. macOS, Windows, or endpoint security
software may warn or block them. You do not need to override those
protections: use the synthetic-webhook instructions below instead.


## 1. Prerequisites

- Go 1.26 or newer and a C compiler supported by Go cgo (Xcode Command
  Line Tools on macOS, GCC on Linux, or MinGW-w64 on Windows). The bridge
  uses cgo for SQLite.
  The committed viewer assets are prebuilt; Node.js is not needed.
- Bridge and event sender must run on the **same machine**: the bridge
  listens on loopback only (`127.0.0.1:8081` by default).
- The optional SubnetLens Pro binaries and reviewer license are in the
  [reviewer-preview-1 release](https://github.com/ostefani/watcherbridge/releases/tag/review).
  Choose Windows amd64, macOS arm64 (Apple silicon), macOS amd64 (Intel),
  or Linux amd64 as appropriate, and download `reviewer-license.zip`.

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

Windows PowerShell (with Go 1.26+ and
MinGW-w64 GCC installed and available on `PATH`):

```powershell
git clone https://github.com/ostefani/WatcherBridge.git
Set-Location WatcherBridge
New-Item -ItemType Directory -Force .\demo-data | Out-Null
$reviewerIdentity = [Security.Principal.WindowsIdentity]::GetCurrent().Name
icacls .\demo-data /inheritance:r /grant:r "${reviewerIdentity}:(OI)(CI)F"
if ($LASTEXITCODE -ne 0) { throw 'Could not restrict demo-data permissions' }
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

Run only the block for your platform. If you already cloned the repository,
start in its root and skip the clone and directory-change commands. Use a
new `demo-data` directory for the initial setup; do not regenerate its
secret while a bridge or sender is using it. On Windows the directory ACL
restricts the demo files to your account; the bridge reads its webhook
secret from the process environment because its secret-file permission
check uses Unix mode bits.

This runs in tap mode, standalone with no relay dependency
(`-scanrelay-url=` disables the optional relay integration). Without a
model key the demo uses clearly marked deterministic fallback briefings,
so AI credentials are optional for a first pass.

WatcherBridge works alongside any existing relay or event consumer: in
tap mode the sender delivers to the bridge and to the consumer
independently, and in proxy mode (`-mode proxy`) the bridge forwards the
original event bytes onward. Either way the bridge never modifies the
sender's reports.

Leave the bridge running in this terminal. Open `http://127.0.0.1:8081/`
in a browser on the same machine, then use one of the two sender options
below in another terminal, also in the repository root.

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
briefing to appear, plus processing time. This fixture has a medium
baseline, so it also qualifies for investigation when enabled in section 5.

### With SubnetLens Pro

**Download:** [SubnetLens Pro binaries and reviewer license — reviewer-preview-1](https://github.com/ostefani/watcherbridge/releases/tag/reviewer-preview-1).

**The Pro executables are not code-signed.** macOS Gatekeeper, Windows
SmartScreen, or endpoint security may warn or block launch. If you cannot
run the binary under your machine's security policy, use the synthetic
demo above.

Download the archive matching your OS and architecture, plus
`reviewer-license.zip`. Extract them into a `pro/` directory under the
WatcherBridge repository root so these files are available:
`pro/subnetlens-pro` (or `pro/subnetlens-pro.exe`) and
`pro/reviewer.license`. Pass the license path explicitly for both
verification and watch mode; the default license filename is
`subnetlens.license`, not `reviewer.license`. On macOS/Linux, make the binary
executable with `chmod +x pro/subnetlens-pro`.
Use your own subnet or one you are authorized to scan. The synthetic
route above needs none of these binaries and performs no scan.

From the repository root, create a locked-down Pro webhook config that
uses the same bridge secret:

macOS/Linux:

```sh
umask 077
cat > demo-data/bridge.yaml <<EOF
webhook_secret: $(cat demo-data/webhook.secret)
endpoints:
  - url: http://127.0.0.1:8081/webhook
    timeout: 10s
EOF
chmod 600 demo-data/bridge.yaml
```

Windows PowerShell (the `demo-data` ACL was set in section 2):

```powershell
$webhookSecret = (Get-Content -Raw .\demo-data\webhook.secret).Trim()
@"
webhook_secret: $webhookSecret
endpoints:
  - url: http://127.0.0.1:8081/webhook
    timeout: 10s
"@ | Set-Content -Path .\demo-data\bridge.yaml -Encoding ascii
Remove-Variable webhookSecret
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

Replace `192.168.1.0/24` with a subnet you are authorized to scan before
running watch mode. Watch interval and scan duration add to the
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

The included `docs/reviewer-mock.py` sender constructs the same envelope
shape with synthetic host values, generates a fresh timestamp and event
ID, signs the exact request body,
and posts to loopback. Use that command for the reviewer demo; it avoids
copying a stale timestamp or reusing an event ID.

Status meanings: `202` accepted (including an already accepted duplicate);
`401` invalid signature or stale timestamp (check the shared secret and
clock skew); `400` malformed request, unknown event type, or split
envelope; `503` temporary admission failure, with retry guidance. In proxy
mode, `502` means downstream forwarding failed; the event remains stored
locally.
Host-event briefings appear after the grouping window (~45 seconds by
default) plus processing time. Heartbeats update source health and do not
produce briefings; they use a separate [H1 payload contract](heartbeat-h1.md).

## 4. Suggested demo tour

1. Overview verdict (quiet / review / act) and a briefing detail page:
   what changed, priority reasoning, recommended check.
2. Review actions: mark a briefing expected, snooze it, add a note. All
   stay local; the original scanner report is never modified.
3. Health screen: ingest outcomes, delivery gaps, retention activity.
4. For a fresh demo, stop the bridge with Ctrl+C and restart using a new
   database filename, such as `-db ./demo-data/demo-2.db`. Keep the same
   secret and send another event. The previous database remains available.

## 5. Enabling AI explanations (optional)

Stop the bridge with Ctrl+C and wait for it to exit before restarting.
Choose the command for your platform and replace the model ID and full
HTTPS Responses endpoint with values supplied for your account. These
examples explicitly enable retained-evidence investigation.

macOS/Linux: put the API key in a private file such as
`demo-data/model-api-key`, set `chmod 600 demo-data/model-api-key`, and run:

```sh
./watcherbridge -secret-file ./demo-data/webhook.secret \
  -db ./demo-data/demo.db -scanrelay-url= \
  -model-provider meta -model-id '<your-model-id>' \
  -model-base-url '<your-https-responses-endpoint>' \
  -model-key-file ./demo-data/model-api-key \
  -agent-enabled
```

Windows PowerShell: read the webhook secret from the existing file and
enter the model key at the hidden prompt. Use environment credentials
instead of `-secret-file` or `-model-key-file` on Windows:

```powershell
$env:WEBHOOK_SECRET = (Get-Content -Raw .\demo-data\webhook.secret).Trim()
$modelKeyInput = Read-Host 'Model API key' -AsSecureString
$env:MODEL_API_KEY = [System.Net.NetworkCredential]::new('', $modelKeyInput).Password
Remove-Variable modelKeyInput
.\watcherbridge.exe -db .\demo-data\demo.db -scanrelay-url= `
  -model-provider meta -model-id '<your-model-id>' `
  -model-base-url '<your-https-responses-endpoint>' `
  -agent-enabled
```

Keep redaction enabled. Send a **new** synthetic event using section 3;
existing fallback briefings are not regenerated on restart. The investigator
is selected for medium-or-higher baselines by default; low-priority groups
use the single-call path. It has five fixed read-only tools, at most four
tool turns plus a final answer, and a default total run budget of 90 seconds.
Tools read retained evidence; they do not scan or access the network.
The model chooses whether to use tools, so a valid answer may have no tool
trace. Check the briefing source and Health for the actual outcome.

### Longer provider budgets

If provider latency exhausts the defaults, stop the bridge and use this
complete macOS/Linux command (with the same account-specific values):

```sh
./watcherbridge -secret-file ./demo-data/webhook.secret \
  -db ./demo-data/demo.db -scanrelay-url= \
  -model-provider meta -model-id '<your-model-id>' \
  -model-base-url '<your-https-responses-endpoint>' \
  -model-key-file ./demo-data/model-api-key \
  -agent-enabled -provider-timeout 3m \
  -agent-timeout 8m -processing-lease 17m
```

Windows PowerShell, after setting the two environment credentials above:

```powershell
.\watcherbridge.exe -db .\demo-data\demo.db -scanrelay-url= `
  -model-provider meta -model-id '<your-model-id>' `
  -model-base-url '<your-https-responses-endpoint>' `
  -agent-enabled -provider-timeout 3m `
  -agent-timeout 8m -processing-lease 17m
```

This changes the total investigator budget to **8 minutes**; `3m` is the
provider attempt-and-repair cap, and individual investigator turns also
share the remaining run time. The `17m` lease exceeds twice the run budget
plus the required 30-second margin. These values pass startup validation;
they do not guarantee a provider response. Timeouts, provider failures, or
rejected output produce marked deterministic fallback briefings while
preserving accepted reports. Allow the grouping window plus model time
before expecting a result.

## 6. Notes for the thorough

- The webhook secret must be at least 32 bytes; the bridge refuses to
  start otherwise.
- Keep redaction enabled (the default) whenever a model key is
  configured: identifying values are tokenized before any provider call.
- Synthetic fixtures used by the project's own end-to-end suite live in
  `e2e/fixtures/` and follow exactly the contract in section 3.
