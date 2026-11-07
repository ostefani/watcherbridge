# Reviewer guide: AI demo

Please review the WatcherBridge v0.1.1 release. If building from source, check out v0.1.1 before following the reviewer guide. Open source development continues on main.

Run WatcherBridge, send a synthetic report, and inspect the AI
briefing. No scanner is required for this demo. Synthetic reports contain
sample network data. Their content is sent to your configured model.

## Requirements

- **Build from source:** Go 1.26 or later and a C compiler for SQLite via cgo.
  The checkout includes the built dashboard, so the Go build can embed it
  without Node.js.
- **Use a release binary:** Download the binary for your platform and skip
  the Go build command. Go and a C compiler are only needed for source builds.
- **Run the synthetic demo:** Python 3.7 or later for the report sender
  (`python3` on macOS/Linux, `py` on Windows). The macOS/Linux setup
  also uses `openssl` to generate a webhook secret on first run.
- A Meta Model API key with access to `muse-spark-1.3-contributor` (or replace
  the model ID below with one enabled for your account).
- Run the bridge, sender, and browser on the same machine.

Download a binary from the [v0.1.1 release](https://github.com/ostefani/watcherbridge/releases/tag/v0.1.1)
or build from source.

Clone. If you already have the repository, start in its root and skip:

```sh
git clone https://github.com/ostefani/watcherbridge.git
cd watcherbridge
```

## 1. Start the AI demo — Terminal 1

Stop any running bridge with Ctrl+C first. Use the block for your platform.
The existing webhook secret is reused. Replace `LLM_your_api_key_here`
below with your actual Meta API key.

### macOS / Linux

```sh
CGO_ENABLED=1 go build -o watcherbridge ./cmd/watcherbridge
umask 077
mkdir -p demo-data
chmod 700 demo-data
if [ ! -f demo-data/webhook.secret ]; then
  openssl rand -hex 32 > demo-data/webhook.secret
fi
chmod 600 demo-data/webhook.secret
printf '%s' 'LLM_your_api_key_here' > /tmp/model.key && chmod 600 /tmp/model.key

./watcherbridge -secret-file ./demo-data/webhook.secret -db ./demo-data/demo.db -scanrelay-url= -model-provider meta -model-base-url https://api.meta.ai/v1/responses -model-id muse-spark-1.3-contributor -model-key-file /tmp/model.key -agent-enabled -group-quiet-window 5s
```

For later restarts, run only the same AI-enabled bridge command. Keep the
webhook secret and recreate `/tmp/model.key` only if it has been cleared.

<details>
<summary>Windows PowerShell</summary>

For a source build, install Go and MinGW-w64 GCC and put them on `PATH`.
The private directory protects local files. Credentials use environment
variables because the secret-file checks require Unix permission bits.

```powershell
$env:CGO_ENABLED = '1'
go build -o watcherbridge.exe .\cmd\watcherbridge
New-Item -ItemType Directory -Force .\demo-data | Out-Null
$reviewerIdentity = [Security.Principal.WindowsIdentity]::GetCurrent().Name
icacls .\demo-data /inheritance:r /grant:r "${reviewerIdentity}:(OI)(CI)F"
if ($LASTEXITCODE -ne 0) { throw 'Could not restrict demo-data permissions' }
if (-not (Test-Path .\demo-data\webhook.secret)) {
    py -c "import secrets; from pathlib import Path; Path('demo-data/webhook.secret').write_text(secrets.token_hex(32), encoding='ascii')"
}
$env:WEBHOOK_SECRET = (Get-Content -Raw .\demo-data\webhook.secret).Trim()
$modelKeyInput = Read-Host 'Meta API key' -AsSecureString
$env:MODEL_API_KEY = [System.Net.NetworkCredential]::new('', $modelKeyInput).Password
Remove-Variable modelKeyInput
.\watcherbridge.exe -db .\demo-data\demo.db -scanrelay-url= -model-provider meta -model-base-url https://api.meta.ai/v1/responses -model-id muse-spark-1.3-contributor -agent-enabled -group-quiet-window 5s
```

</details>

Leave Terminal 1 running. Wait for `serving ingest and viewer on
http://127.0.0.1:8081/`. `no ScanRelay URL; annotations disabled` is expected:
this demo uses the local dashboard without a relay.

## 2. Send a report — Terminal 2

Open another terminal in the repository root:

```sh
python3 docs/reviewer-mock.py demo-data/webhook.secret
```

On Windows, use `py docs/reviewer-mock.py demo-data/webhook.secret`.
Expected output: **`Synthetic event accepted: 202`**.

Open [the dashboard](http://127.0.0.1:8081/) and select the new review under
**Needs attention**. Allow about **5 seconds plus model processing time**;
the default investigator run budget is 90 seconds. `202` confirms receipt,
not completion of the AI request.

A successful briefing shows **Investigator agent** or **Model suggestion**.
Read the summary, priority reasoning, recommended check, and **How I checked
this**. Tool steps appear only if the model used tools. Try marking the
review expected or adding a note. Rerun the sender for a fresh event;
existing fallback briefings are not regenerated on restart.

## Optional: use SubnetLens Pro reports

Keep the same AI-enabled bridge running. Download the appropriate Pro
binary and reviewer license from [GitHub releases](https://github.com/ostefani/watcherbridge/releases/tag/review),
and extract them to `pro/` as `subnetlens-pro` and `reviewer.license`
(`subnetlens-pro.exe` on Windows). Pro is proprietary and its binaries are
unsigned; use the synthetic sender if your system blocks them.

macOS/Linux, from the repository root:

```sh
umask 077
cat > demo-data/bridge.yaml <<EOF
webhook_secret: $(cat demo-data/webhook.secret)
endpoints:
  - url: http://127.0.0.1:8081/webhook
    timeout: 10s
EOF
chmod 600 demo-data/bridge.yaml
chmod +x pro/subnetlens-pro
./pro/subnetlens-pro -license ./pro/reviewer.license -license-status
./pro/subnetlens-pro -subnet 192.168.1.0/24 -watch -watch-interval 10s -watch-webhook-config "$PWD/demo-data/bridge.yaml" -license ./pro/reviewer.license
```

Replace `192.168.1.0/24` with a subnet you are authorized to scan.

<details>
<summary>Windows PowerShell sender</summary>

```powershell
$webhookSecret = (Get-Content -Raw .\demo-data\webhook.secret).Trim()
@"
webhook_secret: $webhookSecret
endpoints:
  - url: http://127.0.0.1:8081/webhook
    timeout: 10s
"@ | Set-Content -Path .\demo-data\bridge.yaml -Encoding ascii
Remove-Variable webhookSecret
.\pro\subnetlens-pro.exe -license .\pro\reviewer.license -license-status
.\pro\subnetlens-pro.exe -subnet 192.168.1.0/24 -watch -watch-interval 10s -watch-webhook-config (Resolve-Path .\demo-data\bridge.yaml).Path -license .\pro\reviewer.license
```

Replace the subnet before running. Keep the private directory ACL from setup.

</details>

### Without Pro: the webhook contract

The synthetic sender above creates and signs the request for you. Custom
senders use `POST /webhook`, `Content-Type: application/json`, and:

| Header | Value |
|---|---|
| `X-SubnetLens-Event-ID` | Unique event ID |
| `X-SubnetLens-Event-Key` | Sender key/scope |
| `X-SubnetLens-Event-Type` | `host.appeared`, `host.changed`, `host.disappeared`, or `scan.heartbeat` |
| `X-SubnetLens-Signature-Timestamp` | Unix seconds, within 240 seconds of the bridge clock |
| `X-SubnetLens-Signature-256` | `sha256=` + hex HMAC-SHA256 of `timestamp.id.key.type.rawbody`, keyed with the shared secret |

The JSON body must echo `event_id`, `event_key`, and `event_type` exactly.
See [the sender](reviewer-mock.py) for a complete example. Heartbeats use
[the H1 contract](heartbeat-h1.md) and do not produce AI briefings.

## Troubleshooting

- **Connection refused:** start Terminal 1 first and leave it running.
- **Secret/key file permissions:** on macOS/Linux, run `chmod 600 demo-data/webhook.secret /tmp/model.key`.
- **401 from the sender:** use the same webhook secret as the running bridge; check your system clock.
- **Deterministic fallback:** inspect Terminal 1 and Health for the reason. Confirm startup used `-model-provider meta`; a fallback is not an AI result.
- **Event-local grouping:** expected for the synthetic sender's `mock` reference; it does not prevent AI processing.
- **No new result:** allow grouping and model time, then check Health. After changing model settings, restart the bridge and send a new event.
