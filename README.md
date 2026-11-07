# WatcherBridge

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.26-blue)
![Muse AI](https://img.shields.io/badge/AI-Muse-0866FF)
![Visitors](https://visitor-badge.laobi.icu/badge?page_id=ostefani.watcherbridge)

Network scanners tell you what’s happening. But what does it actually mean? I already had **SubnetLens Pro**, my proprietary network scanner, which is built on top of open-source [SubnetLens](https://github.com/ostefani/subnetlens). Its watch mode detected and reported network changes and **ScanRelay** displayed the events. I still had to piece together why those changes mattered and what to check next.

I built **WatcherBridge** to fill that gap. It receives signed reports from a network scanner and turns them into a local review inbox, with plain explanations of what changed, why it matters, and what to check next. I used SubnetLens Pro as the event source for this project. The demo can use synthetic reports, so reviewers can try it without a scanner or real network data.

## Building notes

Biggest obstacle during the hackathon: repeated power and internet outages in the final days of the build. I worked in short windows when connected, and submitted before the deadline. Wasn't able to update the submission form to add this note there due to the outage too.

## Deployment modes

![WatcherBridge deployment diagram. Standalone runs without a relay. In tap mode the scanner can deliver separately to WatcherBridge and ScanRelay. In proxy mode WatcherBridge forwards the original report to ScanRelay. All setups provide local review.](docs/images/architecture.svg)

- **Standalone** (`-mode tap -scanrelay-url=`). The scanner sends signed reports to WatcherBridge. Local briefings and review work without ScanRelay.
- **Parallel tap** (`-mode tap`, the default). Configure the scanner with separate endpoints for WatcherBridge and ScanRelay. The scanner delivers to each independently.
- **Proxy** (`-mode proxy -scanrelay-url=<url>`). WatcherBridge verifies and stores each accepted change report before forwarding the original body and signature headers to ScanRelay's `/webhook`. Proxy mode requires a relay URL.

A configured relay URL also enables a separate briefing annotation publisher in either mode. Heartbeats terminate at WatcherBridge and update local source coverage, including in proxy mode.

## Screenshots

The screenshots below use synthetic demonstration data.

**Briefing detail.** Inspect reported identity and priority, open saved scanner evidence, and record an operator decision.
**Home overview.** Find reports needing attention and select an item to review its evidence and recommended check.
<table>
<tr>
<td><img src="docs/images/report.png" alt="Briefing detail showing reported host identity, medium priority, and the operator decision sidebar" width="600" /></td>
<td><img src="docs/images/home.png" alt="Home dashboard showing the review verdict, an item needing attention, and reported-change counts" width="600" /></td>
</tr>
</table>

## What it does

- **Keeps the scanner evidence.** WatcherBridge verifies signatures, preserves the original reports, and uses the context the scanner supplies. Network discovery and host identification belong to the scanner.
- **Explains and prioritizes changes.** Briefings link to the underlying reports and explain what changed, why it matters, and what to check next. Local rules set priority. Optional AI can consult retained evidence through bounded, read-only tools and may raise priority by one level, up to high.
- **Groups related briefings into incidents.** Qualifying appearance or disappearance briefings from the same source and scan target can form an incident. The highest member priority sets the minimum, with a floor of medium for disappearance incidents.
- **Keeps review decisions local.** Mark changes expected or unexpected, snooze items, or add notes and labels. Three matching, eligible “expected” decisions can trigger a rule proposal. Operators can approve it with an optional expiry or revoke an active rule.
- **Shows attention and coverage.** The overview pairs an **Act**, **Review**, or **Quiet** verdict with coverage for each source. Liveness is unknown until heartbeat support is established. After that, a stale heartbeat is flagged as blind even if another source is healthy. The overview is available in the dashboard, JSON API, and status CLI.
- **Protects data sent to AI providers.** Redaction is enabled by default, and a final leak scan can block transmission. Model failures produce a clearly marked explanation based on local rules.

## Run the AI demo

See the [reviewer guide](docs/reviewer-guide.md) for setup and copy-paste
commands to run the Meta AI demo. The synthetic reports let you try it without
a scanner or real network data. You can also use SubnetLens Pro as the event source.

## Command-line flags

Run `./watcherbridge -h` for the startup flags supported by your binary, or
`./watcherbridge status -h` for status flags. Durations use Go syntax such as
`30s`, `5m`, and `720h`.

### Listener, storage, and ScanRelay

| Flag | Default | What it controls |
| --- | --- | --- |
| `-bind` | `127.0.0.1:8081` | HTTP listener for ingest and the dashboard. Use a loopback IP and port. |
| `-db` | `watcherbridge.db` | Path to the local SQLite database. |
| `-source-id` | `default` | Scanner deployment identifier used to scope reported references. Can also come from `SOURCE_ID`. |
| `-secret-file` | Unset | Owner-only file containing the webhook signing secret, which must be at least 32 bytes. |
| `-secret` | Unset | Inline webhook secret. Prefer `-secret-file` or `WEBHOOK_SECRET` to avoid putting it on the command line. Choose one secret source. |
| `-mode` | `tap` | Receive reports in `tap` mode, or forward original change reports to ScanRelay in `proxy` mode. |
| `-scanrelay-url` | `http://127.0.0.1:8080` | ScanRelay base URL. Use `-scanrelay-url=` in tap mode to disable annotation publishing. Proxy mode requires a URL. |
| `-clock-skew-allowance` | `0s` | Allowance for clock difference between WatcherBridge and ScanRelay when validating the forwarding time budget. |
| `-forward-bearer` | Unset | Bearer token for ScanRelay requests. Can also come from `FORWARD_BEARER_TOKEN`. Takes precedence over the API key when both are set. |
| `-forward-api-key` | Unset | API key for ScanRelay requests. Can also come from `FORWARD_API_KEY`. |

### AI briefings

| Flag | Default | What it controls |
| --- | --- | --- |
| `-model-provider` | `fake` | `fake` runs the local pipeline without external model calls. `meta` enables AI for eligible reports. |
| `-model-base-url` | `http://127.0.0.1:9/unused` | Full request endpoint for the Meta adapter. Use HTTPS for remote endpoints and include the Responses path. |
| `-model-id` | `test-only` | Model identifier sent to the provider. Set it to a model your key can access. |
| `-model-key-file` | Unset | Owner-only file containing the model API key. |
| `-model-key` | Unset | Inline model key. Prefer `-model-key-file` or `MODEL_API_KEY` to avoid putting it on the command line. Choose one key source for Meta. |
| `-redaction` | `true` | Tokenizes provider-bound data and enables the final leak scan. Keep it enabled for remote providers. Disabling it sends raw values and bypasses the scan. |
| `-model-min-baseline` | `low` | Minimum local priority eligible for a provider call. Reports below this threshold receive a deterministic rules briefing. |
| `-agent-enabled` | `false` | Opts into the investigator for eligible reports when the provider supports multi-turn requests. By default, eligible reports use the single-call path. |
| `-agent-min-baseline` | `medium` | Minimum local priority eligible for the investigator. Must be at least the model threshold. Eligible reports below this threshold use the single-call path. |
| `-provider-timeout` | `30s` | Shared budget for a single model call and any repair. In investigator mode it caps each turn, including any repair in that turn. |
| `-agent-timeout` | `90s` | Total investigator budget, including preparation, provider turns, and local tool work. |
| `-agent-max-steps` | `4` | Allows one to four read-only tool turns, plus a reserved final-answer turn. An earlier valid final answer can finish the run. |
| `-processing-lease` | `210s` | Age at which an in-progress briefing can be reclaimed as stale. Must be at least twice the investigator budget plus `30s`, and greater than twice the provider timeout plus `1s`. |

The baseline flags accept `low`, `medium`, `high`, and `critical`. The
investigator divides its remaining time among the current and remaining turns,
subject to the provider cap. With the defaults, the first turn gets about
`18s` from the `90s` run budget.

If the single-call path times out at `30s`, you can test with a longer budget.
Keep your existing Meta endpoint, model, and credentials, leave
`-agent-enabled` off, and add:

```sh
-provider-timeout 60s -agent-max-steps 2
```

Startup validates the timeout settings even when the investigator is off.
The investigator budget must cover the provider timeout and be at least
`(agent-max-steps + 1) × (provider-timeout / 2)`. Two tool turns fit the
default `90s` budget with a `60s` provider timeout. In single-call mode,
changing the tool-turn limit only affects configuration validation.

Send a fresh synthetic report after restarting and check **Analysis source**
in the briefing. **Model suggestion** or **Investigator agent** means an AI
answer was used. **Deterministic fallback** means WatcherBridge used its local
briefing after a provider or validation failure. Restarting leaves previously
stored fallback briefings in place.

### Grouping, review, and retention

| Flag | Default | What it controls |
| --- | --- | --- |
| `-group-quiet-window` | `45s` | How long a group waits after its latest report before it can close for briefing. |
| `-group-max-window` | `120s` | Maximum grouping window. Must cover the quiet window. |
| `-grouper-tick` | `1s` | Worker interval for checking group closure and processing briefings. Must be at most the quiet window and shorter than the processing lease. |
| `-stable-fields` | `mac,hostname,host_id,identity_confidence,identity_source` | Scanner change fields that break the stability check used by the local baseline. |
| `-overview-window` | `24h` | Default period covered by the overview. |
| `-overview-max-window` | `168h` | Maximum overview period. Must cover the default period and fit within briefing retention. |
| `-overview-tick` | `30s` | Periodic overview refresh interval, from `5s` to `5m`. |
| `-heartbeat-stale-after` | `10m` | Age at which a previously established source heartbeat is considered stale. |
| `-heartbeat-retention` | `168h` | How long to retain heartbeat history. |
| `-briefing-retention` | `720h` | Retained briefing history, with a minimum of `24h`. |
| `-subject-retention` | `2160h` | Retained reported-subject context. Must cover briefing retention. |
| `-purge-interval` | `6h` | Interval between background retention sweeps, with a minimum of `1m`. |
| `-snooze-default` | `24h` | Default briefing snooze duration. Must fit within decision retention. |
| `-decision-retention` | `4320h` | Retained operator decision history, with a minimum of `24h`. |
| `-rule-propose-after` | `3` | Matching eligible expected decisions needed to propose a rule. |
| `-rule-default-ttl` | `2160h` | Default lifetime for an approved review rule. |
| `-rule-retention` | `4320h` | Retention for terminal review rules, with a minimum of `24h`. |
| `-incident-tick` | `30s` | Incident correlation interval, from `5s` to `5m`. |
| `-incident-window` | `5m` | Time window in which qualifying reports can form an incident. Must fit within briefing retention. |
| `-incident-quiet` | `120s` | Time without matching reports before an incident closes. Must cover at least one correlation tick. |
| `-incident-min-refs` | `3` | Minimum distinct qualified references to form an incident. Accepts 2 to 500. |
| `-incident-retention` | `2160h` | Retention for closed incidents, from `24h` up to subject retention. |
| `-incident-summary-enabled` | `false` | Enables AI summaries for newly closed incidents with a provider that supports summaries. `INCIDENT_SUMMARY_ENABLED=true` also enables it. |

### Status command

| Flag | Default | What it controls |
| --- | --- | --- |
| `-bridge` | `http://127.0.0.1:8081` | Base URL of the running bridge. |
| `-window` | Bridge default | Overview period to request. |
| `-json` | `false` | Prints the overview response as JSON. |
| `-timeout` | `10s` | Timeout for the status API request. |

## Operation and development

- Run `./watcherbridge -h` for startup settings and `./watcherbridge status --window 24h --json` for the running bridge's overview. Status exits with `0` for quiet, `1` for review, `2` for act, or `3` when unavailable.
- The listener accepts loopback addresses only. A quiet verdict describes retained reports and review state, not network security or host availability.
- Built with Go, SQLite in WAL mode, Svelte 5, Vite, and Server-Sent Events. Use the Node.js version in [ui/.nvmrc](ui/.nvmrc) and `make ui-check` for viewer changes.

See [CONTRIBUTING.md](CONTRIBUTING.md) for engineering conventions.

## Submission scope

This submission consists solely of WatcherBridge, built during the hackathon. SubnetLens Pro and ScanRelay are external integrations and are not part of the submission.

## License

MIT © 2026 Olha Stefanishyna
