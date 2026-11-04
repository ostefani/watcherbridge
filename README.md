# WatcherBridge

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.26-blue)
![Muse AI](https://img.shields.io/badge/AI-Muse-0866FF)
![Visitors](https://visitor-badge.laobi.icu/badge?page_id=ostefani.watcherbridge)

**Turn network changes into clear explanations and a focused review inbox.**

WatcherBridge receives signed network-change reports, preserves the original evidence, and groups related events into prioritized briefings: what changed, why it matters, and what to check next. I used **SubnetLens Pro**, my proprietary network scanner, as the event source for this project.

The bridge runs locally with an embedded dashboard, SQLite storage, and optional AI explanations. It interprets reported changes; network discovery and host identification belong to the event source.

## What it does

- **Shows what needs attention.** Each briefing shows a priority derived from deterministic rules over the reported changes, with the reasoning behind it. AI may raise that priority by one level, up to high; only rules produce critical.
- **Explains with evidence.** Briefings link to original reports and retained host context. An optional investigator uses bounded, read-only tools over local history.
- **Supports review.** Mark changes expected or unexpected, snooze items, and add notes or labels. Review decisions stay local.
- **Protects provider context.** Redaction is enabled by default, and a final leak scan blocks unsafe provider requests. Model failures produce marked deterministic fallbacks.
- **Fits existing workflows.** Use the live dashboard, JSON API, or status CLI. Optionally forward original signed events and publish briefing annotations to a relay.

```text
Signed reports → Verify & store → Group → Prioritize → Explain or fallback
                                                           ↓
                                                  Dashboard · API · CLI
```

## Quick start

Requires **Go 1.26+** and a **C compiler** for SQLite. The viewer is embedded; Node.js is needed only for frontend development. The examples below use a POSIX shell.

The [reviewer guide](docs/reviewer-guide.md) covers setup and demos on macOS, Linux, and Windows, including optional scanner integration.

```sh
git clone https://github.com/ostefani/WatcherBridge.git
cd WatcherBridge
CGO_ENABLED=1 go build -o watcherbridge ./cmd/watcherbridge

mkdir -m 700 demo-data
openssl rand -hex 32 > demo-data/webhook.secret
chmod 600 demo-data/webhook.secret

./watcherbridge -secret-file ./demo-data/webhook.secret \
  -db ./demo-data/demo.db -scanrelay-url=
```

Open the [local dashboard](http://127.0.0.1:8081/). In another terminal, send a synthetic report using Python 3:

```sh
python3 docs/reviewer-mock.py demo-data/webhook.secret
```

The sender should print `Synthetic event accepted: 202`. Allow about 45 seconds for the grouping window to close. This demo performs no network scan and makes no external AI calls. The default `fake` provider produces marked fallback briefings, which may show degraded coverage in Health.

For integration, configure a sender on the same machine to post to `http://127.0.0.1:8081/webhook` using the shared secret and the [signed webhook contract](docs/reviewer-guide.md#without-pro-the-webhook-contract). Compatible senders must implement that event schema and signature format.

## AI explanations

The live adapter uses the Meta Model API with a configurable model ID and endpoint. Store your API key in an owner-only file, stop the running bridge, and restart it with:

```sh
./watcherbridge -secret-file ./demo-data/webhook.secret \
  -db ./demo-data/demo.db -scanrelay-url= \
  -model-provider meta -model-id '<your-model-id>' \
  -model-base-url '<your-https-responses-endpoint>' \
  -model-key-file /absolute/path/to/model-api-key \
  -agent-enabled
```

This example enables retained-evidence investigation. Keep redaction enabled for external providers. AI explanations are advisory; the bridge remains usable without a model key.

## Operation and development

- Run `./watcherbridge -h` for startup settings and `./watcherbridge status --window 24h --json` for the running bridge's overview. Status exits with `0` for quiet, `1` for review, `2` for act, or `3` when unavailable.
- The listener accepts loopback addresses only. A quiet verdict describes retained reports and review state, not network security or host availability.
- Tap mode receives events independently of other consumers. Proxy mode (`-mode proxy`) forwards original signed bytes to the configured relay. `-scanrelay-url=` disables relay integration.
- Built with Go, SQLite in WAL mode, Svelte 5, Vite, and Server-Sent Events. Use the Node.js version in [ui/.nvmrc](ui/.nvmrc) and `make ui-check` for viewer changes.

See [CONTRIBUTING.md](CONTRIBUTING.md) for engineering conventions.

## Submission scope

This submission consists solely of WatcherBridge, built during the hackathon. SubnetLens Pro and ScanRelay are external integrations and are not part of the submission.

## License

MIT © 2026 Olha Stefanishyna
