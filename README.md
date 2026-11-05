# WatcherBridge

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.26-blue)
![Muse AI](https://img.shields.io/badge/AI-Muse-0866FF)
![Visitors](https://visitor-badge.laobi.icu/badge?page_id=ostefani.watcherbridge)

**Turn network changes into clear explanations and a focused review inbox.**

WatcherBridge receives signed network-change reports, preserves the original evidence, and groups related events into prioritized briefings: what changed, why it matters, and what to check next. I used **SubnetLens Pro**, my proprietary network scanner, as the event source for this project.

The bridge runs locally with an embedded dashboard, SQLite storage, and optional AI explanations. It interprets reported changes; network discovery and host identification belong to the event source.

## Screenshots

The screenshots below use synthetic demonstration data.

**Briefing detail.** Inspect reported identity and priority, open saved scanner evidence, and record an operator decision.

![Briefing detail showing reported host identity, medium priority, and the operator decision sidebar](docs/images/report.png)

**Home overview.** Find reports needing attention and select an item to review its evidence and recommended check.

![Home dashboard showing the review verdict, an item needing attention, and reported-change counts](docs/images/home.png)

## Building notes

Biggest obstacle: repeated power and internet outages in the final days of the build. I worked in short windows when connected, and submitted before the deadline. Wasn't able to update the submission form to add this note there due to the outage too.

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

## Run the AI demo

See the [reviewer guide](docs/reviewer-guide.md) for setup and copy-paste
commands to run the Meta AI demo with synthetic reports or SubnetLens Pro.

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
