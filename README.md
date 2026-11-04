# WatcherBridge

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.26-blue)
![Redaction on by default](https://img.shields.io/badge/redaction-on%20by%20default-brightgreen)
![Built for a hackathon](https://img.shields.io/badge/built%20for-hackathon-blueviolet)

**Turn network-change reports into prioritized explanations and a local review inbox.**

WatcherBridge receives signed webhooks from SubnetLens Pro, preserves the original reports, and turns related changes into concise briefings: what changed, why it matters, how urgent it is, and one recommended check. A built-in dashboard combines those explanations with retained evidence, reported-host history, and your own review decisions.

Run it alongside your existing event viewer or use it on its own. AI explanations are optional; local storage, deterministic priorities, and fallback briefings work without a model API key.

## Why WatcherBridge

- **Spend less time connecting events.** Bounded explanation windows group related reports, adding retained context so you can review a change in one place.
- **See what needs attention.** The overview separates items that need action, items to review, and routine changes. Coverage and source health appear alongside the verdict.
- **Understand the priority.** Each briefing separates the deterministic baseline, any model suggestion, and the final priority. AI can raise urgency by one level, up to high; only deterministic rules produce critical.
- **Keep control of review.** Mark a change expected or not expected, snooze it, add notes and labels, or undo a decision. These actions stay local and preserve the original scanner reports.
- **Keep history close.** Events, briefings, reported context, and pseudonym mappings live in a local SQLite database with configurable retention.
- **Continue when AI is unavailable.** Provider errors, rejected responses, and redaction blocks produce clearly marked deterministic fallback briefings. Model work runs outside webhook handling.
- **Fit an existing workflow.** Use the embedded web viewer, query JSON APIs, check status from the CLI, or publish briefing annotations to ScanRelay.

## How it works

```text
SubnetLens Pro
  signed host.appeared / host.changed / host.disappeared reports
        |
        v
Authenticate -> Store original bytes -> Deduplicate -> Group related reports
                                                          |
                                                          v
                                                Deterministic priority
                                                          |
                                                          v
                                          Redacted AI explanation or fallback
                                                          |
                                                          v
                                                Durable linked briefing
                                                          |
                              +---------------------------+------------------+
                              v                           v                  v
                        Local web viewer             JSON API / CLI    ScanRelay annotations
                                                                         (optional)
```

SubnetLens Pro owns discovery, device and OS evaluation, host association, and appearance/change/disappearance detection. WatcherBridge explains what the scanner reported and retains its provenance. It does not scan your network, re-identify devices, or infer that a host disappeared because reports stopped arriving.

## Explore and review

### Overview and briefing detail

The Home screen shows a prioritized attention list, a digest of reported changes, and a quiet, review, or act verdict for the selected time window. Open a briefing to inspect its summary, priority reasoning, reported evidence, retained context, recommended check, and provenance.

Live updates use Server-Sent Events, with bounded replay and resynchronization after gaps. Reported-host pages show scanner-supplied attributes and paged retained history, distinguishing device references, address context, unresolved events, and legacy associations. Matching IP addresses or hostnames do not merge profiles.

The viewer supports keyboard navigation, dark mode, and reduced motion. The Health screen exposes source progress, delivery gaps, provider outcomes, redaction blocks, queues, annotation delivery, and retention activity.

### Local decisions and review rules

Expected, not-expected, snooze, note, and label decisions help organize the inbox without changing scanner severity or identity. Decisions are recorded as an audit trail; undo adds a superseding record.

For sources with a supported stable identity contract, repeated expected decisions can produce exact review-rule proposals through the local API. Approved rules match the source, identity namespace and revision, reported reference, and change details. They can make matching non-critical changes routine; critical remains protected. Address-only and unresolved reports remain available for manual review.

### Optional retained-evidence investigation

Enable the investigator with `-agent-enabled`. It can consult five fixed, read-only tools over retained profiles, history, similar reported changes, reported services, and context comparisons. The default limits are four tool turns plus a final answer within a 90-second run budget. The detail view exposes the tool trace and validated evidence references.

Investigation uses existing local evidence. It has no scanner, shell, arbitrary SQL, file, or network tool. Suggested explanations and model confidence are advisory; they do not establish device identity.

### Incident bundles

When the source supplies qualified stable references, WatcherBridge can present bursts of scanner-reported appearances or disappearances as one expandable incident. By default, formation requires three distinct qualified references from the same source and target within five minutes. Without qualifying identity metadata, reports remain individual items.

Member briefings retain their priorities and evidence. An incident's priority is at least the highest member priority, with a minimum of medium for disappearance bursts. Incident review lets you choose whether a decision also applies to members. Incidents and local decisions stay out of ScanRelay annotations.

An include-members decision applies atomically to at most 500 members; a larger request is rejected without a partial decision. New members do not inherit an earlier acknowledgement, and undoing an incident decision does not undo member decisions. For previously committed larger actions, the API returns continuation cursors for the original member-decision snapshot through `GET /api/decisions/{parent_decision_id}/members?after={cursor}&limit=200` (maximum limit 500).

**Forthcoming:** an optional bounded AI summary model for closed incidents, adding an evidence-linked explanation over the fixed member set while preserving the deterministic priority floor.

## Privacy and reliability

Redaction is enabled by default. Provider-bound context replaces identifying values—including IP and MAC addresses, hostnames, subnets, and upstream references—with local tokens. Leak scanning blocks a call when it detects remaining sensitive values or cannot complete safely. The mapping stays local, where accepted text is restored for display.

The model receives bounded structured context rather than a raw scan dump. Responses pass strict field, token, size, and evidence-reference validation, with bounded repair and deterministic fallback. Schema validation checks structure and references; explanations still require operator judgment.

Webhook admission verifies HMAC signatures, freshness, and envelope consistency before storing accepted reports. SQLite provides durable event and briefing storage; recovery resumes interrupted work. Retention removes eligible history in bounded batches while protecting referenced and pending work.

The listener is restricted to loopback. The browser-facing API also checks Host and same-origin writes. One bridge instance serves one configured scanner source; `-source-id` binds that deployment's reported references to its database.

## Delivery modes

**Tap mode (default, preferred):** configure the sender to deliver separately to WatcherBridge and any existing consumer.

```text
                 +--> WatcherBridge --> local viewer / API / optional annotations
SubnetLens Pro --+
                 +--> ScanRelay (optional, independent event delivery)
```

WatcherBridge acknowledges after local persistence; briefing generation happens asynchronously. A bridge outage does not interrupt the sender's separate delivery to ScanRelay. Tap requires a sender configuration that supports delivery to multiple endpoints.

**Proxy mode:** send events through WatcherBridge when the sender uses a single destination.

```text
SubnetLens Pro --> WatcherBridge --> ScanRelay (original signed event bytes)
                       |
                       +--> local viewer / API / briefing annotations
```

The bridge stores the event, then forwards the original body and signature headers without rewriting them. There is no retry loop inside the request. Sender redelivery and a bounded startup recovery sweep can retry eligible unforwarded events while their signatures remain fresh; expired deliveries are recorded as undeliverable. A bridge outage interrupts this forwarding path.

ScanRelay works independently of WatcherBridge, and WatcherBridge's viewer works without ScanRelay. Briefing annotations are separate records linked to original event IDs.

## Quick start

### Build

Requirements: **Go 1.26 or newer** and a **C compiler** for the SQLite driver (Xcode Command Line Tools on macOS or GCC on Linux). The production viewer assets are embedded and committed; running the bridge does not require Node.js.

```sh
git clone https://github.com/ostefani/WatcherBridge.git
cd WatcherBridge
CGO_ENABLED=1 go build -o watcherbridge ./cmd/watcherbridge
```

### Start locally

Create an owner-only secret file containing a randomly generated webhook secret of at least 32 bytes. Configure the same secret in SubnetLens Pro. Use an owner-only data directory for the database.

```sh
mkdir -m 700 watcherbridge-data
openssl rand -hex 32 > watcherbridge-data/webhook.secret
chmod 600 watcherbridge-data/webhook.secret

./watcherbridge \
  -secret-file ./watcherbridge-data/webhook.secret \
  -db ./watcherbridge-data/watcherbridge.db \
  -scanrelay-url=
```

Point SubnetLens Pro's signed webhook delivery at `http://127.0.0.1:8081/webhook`, then open [the local viewer](http://127.0.0.1:8081/). The sender must be able to reach the bridge's loopback listener. SubnetLens Pro is a separate proprietary application and is not included here.

The default `fake` provider makes no model calls and produces marked fallback briefings. This is enough to use the ingestion, history, dashboard, and review workflow without provider credentials. Fallback activity is visible in Health and can mark coverage degraded.

### Enable AI explanations

The included live adapter targets the Meta Model API; the model ID and full endpoint URL are configurable. Use the endpoint and model ID provided for your account and store its API key in an owner-only file.

```sh
./watcherbridge \
  -secret-file ./watcherbridge-data/webhook.secret \
  -db ./watcherbridge-data/watcherbridge.db \
  -scanrelay-url= \
  -model-provider meta \
  -model-id '<your-model-id>' \
  -model-base-url '<your-https-responses-endpoint>' \
  -model-key-file /absolute/path/to/model-api-key
```

Stop the previous bridge process before restarting with this configuration. Keep the default redaction enabled when using an external provider. Add `-agent-enabled` to opt into retained-evidence investigation.

### Connect ScanRelay

For optional annotations in tap mode, set `-scanrelay-url http://127.0.0.1:8080` and keep the sender's direct ScanRelay delivery. For forwarding, also set `-mode proxy` and point the sender at the bridge. If ScanRelay requires authentication, use `FORWARD_BEARER_TOKEN` or `FORWARD_API_KEY`.

### Check status

```sh
./watcherbridge status
./watcherbridge status --window 24h --json
```

The CLI reads the running bridge's overview API. Exit codes are `0` for quiet, `1` for review, `2` for act, and `3` when status cannot be obtained. A quiet verdict describes the retained reports and your review state; it does not certify network security or host availability. Scanner liveness remains unknown without supported heartbeats; stale heartbeats from a previously reporting source are shown as blind coverage.

## Configuration

Run `./watcherbridge -h` for all startup flags, or `./watcherbridge status -h` for CLI options. Common settings:

| Flag | Default | Purpose |
|---|---|---|
| `-bind` | `127.0.0.1:8081` | Loopback listener for webhooks, viewer, and API |
| `-mode` | `tap` | Independent receipt or proxy forwarding |
| `-source-id` | `default` | Durable scanner deployment scope; also accepts `SOURCE_ID` |
| `-db` | `watcherbridge.db` | Local SQLite database path |
| `-secret-file` | Unset | Owner-only webhook secret file; alternatively `WEBHOOK_SECRET` |
| `-scanrelay-url` | `http://127.0.0.1:8080` | Optional integration; pass an empty value to disable |
| `-model-provider` | `fake` | `fake` for local fallback, `meta` for AI explanations |
| `-model-id` / `-model-base-url` | Test placeholders | Set both for a live provider |
| `-model-key-file` | Unset | Owner-only model key file; alternatively `MODEL_API_KEY` |
| `-redaction` | `true` | Tokenize and leak-scan provider context |
| `-agent-enabled` | `false` | Enable bounded retained-evidence investigation |
| `-group-quiet-window` / `-group-max-window` | `45s` / `2m` | Explanation grouping windows |
| `-overview-window` | `24h` | Default review window, maximum `168h` |
| `-briefing-retention` / `-subject-retention` | `720h` / `2160h` | Briefing history and reported-subject retention |

Configure secrets through a file or the supported environment variables, using one mechanism per secret. Keep the source ID stable for the lifetime of its database. Review rules and counted incident bundles require a separately registered, supported upstream identity capability; a nonempty host ID alone does not enable them.

## Built with

| Component | Implementation |
|---|---|
| Service and CLI | Go, standard-library HTTP server |
| Persistence | SQLite in WAL mode through `mattn/go-sqlite3` |
| Web viewer | Svelte 5 and Vite; embedded production assets |
| Live updates | Server-Sent Events with bounded replay |
| AI integration | Meta Model API adapter, configurable endpoint and model |
| Offline operation | Deterministic baseline and fallback briefings |

To modify the viewer, use the pinned Node.js version in [ui/.nvmrc](ui/.nvmrc), then run `make ui-check`. See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution conventions.

## Hackathon disclosure

WatcherBridge was built during the hackathon. These companion projects predate the submission, live outside this repository, and connect through event and annotation interfaces:

| Project | Role | Included here? |
|---|---|---|
| [SubnetLens](https://github.com/ostefani/subnetlens) | Open-source network scanner | No |
| SubnetLens Pro | Proprietary watch mode, scanner evaluation, and signed webhook sender | No |
| ScanRelay | Proprietary event viewer and optional annotation consumer | No |
| **WatcherBridge** | **Event bridge, briefings, local review, and embedded viewer** | **Yes** |

The companion projects' code is not copied into this submission. Test fixtures use synthetic data.

## License

MIT © 2026 Olha Stefanishyna
