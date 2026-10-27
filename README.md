# WatcherBridge

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
![Status: early development](https://img.shields.io/badge/status-early%20development-orange)
![Developed with Muse Spark](https://img.shields.io/badge/developed%20with-Muse%20Spark-0467DF)
![Model is configurable](https://img.shields.io/badge/model-configurable-lightgrey)
![Redaction on by default](https://img.shields.io/badge/redaction-on%20by%20default-brightgreen)
![Built for a hackathon](https://img.shields.io/badge/built%20for-hackathon-blueviolet)

An open-source bridge that receives network watch events via webhooks and turns them into short, prioritized briefings.

## The problem

SubnetLens Pro is the proprietary companion to the open-source [SubnetLens](https://github.com/ostefani/subnetlens) scanner. It watches a local network and sends an HMAC-signed webhook whenever a host appears, disappears, or changes.

The owner still has to read across the events, connect related changes on the same host, and decide which need attention now. WatcherBridge adds interpretation. It groups related events per host, adds the host's recent history, and asks an AI model for a briefing: what changed, why it matters, how urgent it is, and one check to run. Events continue to the consumers unchanged, and briefings are added alongside them.

## How it works

WatcherBridge is designed to work with SubnetLens Pro's watch mode. WatcherBridge receives those webhooks, groups related events per host, and sends a small redacted bundle to an AI model. It returns a schema-validated briefing: what changed, why it matters, how urgent it is, and one check to run. It ships with its own viewer, so it works on its own, and it can optionally publish briefings to ScanRelay.

## Built with

| Part | Choice |
|---|---|
| Model | Developed against Muse Spark. The model is configurable |
| Model access | [Meta Model API](https://dev.meta.ai) (Standard tier) |
| Input | HMAC-signed webhooks from SubnetLens Pro watch mode |
| Output validation | JSON Schema |
| Storage | Local event log (optionally enriched with JSON scan reports) |
| Language and runtime | To be decided |

> **Status: early development.** Built for a hackathon. Sections marked *(planned)* are not implemented yet.

> **Design note:** Details such as schemas, configuration names, and internal structure may change as development goes on. The core guarantees will not change: the bridge and ScanRelay stay independent, redaction is on by default, and the model only summarizes data that Pro has already computed.

## Hackathon disclosure: what is new and what is not

Everything in this repository was built during the hackathon. The projects below are separate, pre-existing, and not part of this submission. WatcherBridge talks to them only through documented interfaces, and none of their code is copied here.

| Project | Status | Role | In this repo? |
|---|---|---|---|
| [SubnetLens](https://github.com/ostefani/subnetlens) | Existing, open source | Network scanner | No |
| SubnetLens Pro | Existing, proprietary | Watch mode, device and OS evaluation, webhook sender | No (interface only) |
| ScanRelay | Existing, proprietary | Viewer for Pro's events. Works with Pro on its own | No (optional integration only) |
| **WatcherBridge** | **Built during the hackathon** | Verification, grouping, redaction, AI briefing, built-in viewer | **Yes, all of it** |

The first commit marks the start of hackathon work. Test fixtures are synthetic, so no real network data or proprietary code is included.

## Independence

WatcherBridge and ScanRelay are independent of each other.

- **ScanRelay does not need WatcherBridge.** It keeps working with Pro exactly as before.
- **WatcherBridge does not need ScanRelay.** Its built-in viewer shows each original event with its briefing. ScanRelay support is an optional output.
- In tap mode, if the bridge stops, you lose briefings only. See delivery modes below for proxy mode.

## Delivery modes *(planned)*

```
TAP MODE (preferred): Pro sends each event to both

                 +--> ScanRelay (optional, works without the bridge)
SubnetLens Pro --+
 (watch mode)    +--> WatcherBridge --> outputs


PROXY MODE: Pro sends to one URL, the bridge forwards unchanged

SubnetLens Pro --> WatcherBridge --> ScanRelay (events forwarded unchanged)
 (watch mode)           |
                        +--> outputs
```

- **Tap mode (preferred).** There is no coupling between the bridge and ScanRelay. If the bridge fails, you lose only briefings. It needs a sender that can deliver each event to more than one endpoint.
- **Proxy mode.** Use it if Pro can only send to one endpoint. The bridge forwards the raw request body and signature header untouched, so ScanRelay can keep verifying Pro's original HMAC. By default there are no retries or buffering, so ScanRelay's delivery semantics don't change. If the bridge is down, events do not reach ScanRelay.

In both modes, original events reach ScanRelay unchanged. Briefings are separate records linked to the original events by ID.

## Briefing example *(proposed format)*

```json
{
  "event_ids": ["evt-001", "evt-002"],
  "host": "host-a1",
  "what_changed": "New listening service appeared on an existing device.",
  "context": "Consistent with the device type Pro evaluated; first change in the recent history.",
  "priority": "medium",
  "priority_reason": "A new service on a stable host is unusual but not alarming.",
  "recommended_check": "Confirm the service was installed intentionally."
}
```

The values above are illustrative sample data.

## What the model does and does not do

- It **synthesizes** data that Pro has already computed: the change, Pro's device and OS evaluation with its evidence, and the host's recent history.
- It does **not** re-identify devices.
- It does **not** override or second-guess Pro's evaluation.
- It never sees real addresses unless you turn redaction off.

## Privacy

- Redaction is **on by default**. IP and MAC addresses and other identifying strings are replaced with local pseudonyms before anything leaves your machine.
- The pseudonym map and the event log stay local.
- Only a small structured bundle is sent to the model, not raw scan output.
- Turning redaction off requires an explicit setting.

## Requirements

- SubnetLens Pro with watch mode and webhooks enabled (proprietary, not included)
- An API key for the configured model provider (developed against the Meta Model API)
- A webhook shared secret, the same one configured on the sender
- ScanRelay is **not** required

## Quick start *(planned)*

```bash
# 1. Clone the repository
git clone https://github.com/ostefani/WatcherBridge
cd WatcherBridge

# 2. Configure (setting names are proposals and may change)
export MODEL_API_KEY=...
export WEBHOOK_SECRET=...

# 3. Run the bridge (exact command to be filled in once implemented)

# 4. Point SubnetLens Pro's webhook at the bridge
# 5. Open the built-in viewer
```

## Roadmap

- [ ] Webhook receiver with HMAC verification and immediate acknowledgement
- [ ] Local event log
- [ ] Per-host grouping and structured diff
- [ ] Redaction with local pseudonym map
- [ ] Configurable model client (first target: Muse Spark via the Meta Model API)
- [ ] Briefing schema validation and fallback
- [ ] Address restoration
- [ ] Built-in viewer (events and briefings side by side)
- [ ] JSON / CLI output
- [ ] Tap mode and proxy mode
- [ ] Optional ScanRelay adapter
- [ ] Tests with synthetic fixtures

## Limitations

- Briefings are advisory. They do not replace your own review.
- Model output can be wrong. Schema validation checks the shape of a response, not whether its content is true.
- Quality and structured-output support vary between models.
- In proxy mode, the bridge becomes a single point of failure for event delivery to ScanRelay.

## License

MIT © 2026 Olha Stefanishyna