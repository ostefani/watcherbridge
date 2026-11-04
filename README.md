# WatcherBridge

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.26-blue)
![Muse AI](https://img.shields.io/badge/AI-Muse-0866FF)
![Local first](https://img.shields.io/badge/local-first-brightgreen)

**Turn network changes into clear explanations and a focused review inbox.**

WatcherBridge receives signed network-change reports, preserves the original evidence, and groups related events into prioritized briefings: what changed, why it matters, and what to check next. I used **SubnetLens Pro**, my proprietary network scanner, as the event source for this project.

The bridge runs locally with an embedded dashboard, SQLite storage, and optional AI explanations. It interprets reported changes; network discovery and host identification belong to the event source.

## What it does

- **Prioritizes changes.** Deterministic rules set the priority floor. AI may raise it by one level, up to high; only rules produce critical.
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

Requires **Go 1.26+** and a **C compiler** for SQLite. The viewer is embedded; Node.js is needed only for frontend development. These commands use a macOS/Linux shell; see the [reviewer guide](docs/reviewer-guide.md) for Windows instructions.

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
  -model-key-file /absolute/path/to/model-api-key
```

Keep redaction enabled for external providers. Add `-agent-enabled` for retained-evidence investigation. AI explanations are advisory; the bridge remains usable without a model key.

## Operation and development

- Run `./watcherbridge -h` for startup settings and `./watcherbridge status --window 24h --json` for the running bridge's overview. Status exits with `0` for quiet, `1` for review, `2` for act, or `3` when unavailable.
- The listener accepts loopback addresses only. A quiet verdict describes retained reports and review state, not network security or host availability.
- Tap mode receives events independently of other consumers. Proxy mode (`-mode proxy`) forwards original signed bytes to the configured relay. `-scanrelay-url=` disables relay integration.
- Built with Go, SQLite in WAL mode, Svelte 5, Vite, and Server-Sent Events. Use the Node.js version in [ui/.nvmrc](ui/.nvmrc) and `make ui-check` for viewer changes.

See the [reviewer guide](docs/reviewer-guide.md) for the demo walkthrough and optional scanner setup, and [CONTRIBUTING.md](CONTRIBUTING.md) for engineering conventions.

## Submission scope

WatcherBridge was built during the hackathon. The scanner and optional ScanRelay consumer predate this submission; their source code is outside this repository. The included demo and test fixtures use synthetic data.

## License

MIT © 2026 Olha Stefanishyna
