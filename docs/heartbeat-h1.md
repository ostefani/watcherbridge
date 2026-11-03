# H1 receiver and source health

This is the WatcherBridge half of Planv4 Step 21 and Designv9 §§3.2–3.4,
4.6. SubnetLens Pro owns the producer contract in its `docs/heartbeat-h1.md`.
H1 establishes scanner/pipeline progress, not stable identity, host presence,
an inventory snapshot, or current per-host ports. It grants no I1 capability
and does not enable identity-scoped rules.

## Configuration and delivery

Pro endpoints default to alerts only. Once both applications include H1, opt
in on the WatcherBridge endpoint with `events: [alerts, heartbeat]`. Keep the
ScanRelay endpoint alerts-only. Both use their configured signing credentials.

The existing `POST /webhook` accepts `scan.heartbeat` in tap and proxy modes.
It validates HMAC and the signature timestamp under the existing admission and
deadline limits. H1 bodies have an independent 8 KiB cap. Strict JSON rejects
unknown/duplicate keys, null/missing fields, trailing documents, inconsistent
headers, malformed scan IDs, invalid counts/statuses, overflow and impossible
completion-after-signing times. Raw error messages and host lists are not part
of H1. Source scope comes only from the authenticated receiver configuration.

The flat producer envelope carries `event_id`, `event_key`, `event_type`,
`occurred_at`, `cycle`, `target`, `scan_id`, `completed_at`, `hosts_seen`,
`duration_ms`, `status`, and `issue_count`. The event ID equals the 32-character
lowercase hex scan ID, the key is `heartbeat:<scan_id>`, and occurred/completed
times agree. `ok` has zero issues; `partial`/`error` have positive issue counts.

Successful persistence returns 202. Identical retries return 202 without
renewing source progress. Reusing an H1 ID with different bytes returns 409;
invalid input returns 400/401/413, and unavailable persistence returns 503 with
Retry-After. Original host-alert delivery behavior is unchanged. Process ingest
counters include H1 outcomes; report receipt/digest counts continue counting
host events only.

## Persistence and freshness

Schema v25 adds an isolated `heartbeats` table and compact source summary.
History insertion and source progress commit together through the serialized
writer. H1 never enters host event storage, the grouping/projector queues,
briefings, provider input, annotation publication, or the forwarding sweep.
No relay connection is attempted for H1, even in proxy mode.

The compact source state records completion/receipt time, estimated freshness,
result status/issues, duration, host-record count, target and signing-clock
offset. It updates only for a strictly newer completion. Identity namespace
and capability columns are preserved, and profile/report ages are untouched.

Freshness uses receiver time minus the scan's age when signed:
`fresh_at = received_at - max(0, signature_at - completed_at)`. This accounts
for queued scans and avoids granting a new freshness window just because Pro
re-signed old work. Signature timestamps have one-second resolution. Clock
offset is exposed as signing time minus receipt time and includes network
delay; it is diagnostic, not proof of exact clock skew. Large backward clock
steps may conservatively leave the source stale until completion timestamps
advance beyond its retained high-water mark.

Defaults are `-heartbeat-stale-after=10m` and `-heartbeat-retention=168h`.
Both must be positive. Before H1, liveness is unknown. A fresh H1 establishes
reporting liveness; `partial`/`error` scans make coverage degraded. At the stale
boundary coverage becomes blind and liveness stale. Blind outranks degraded;
one fresh source cannot mask another stale source. Timer-based shared Go
computation keeps API, SSE, health and CLI behavior aligned without new events.

## Retention and limits

History expires by receipt time under the existing shared 200-write purge
budget, including bookkeeping. An index bounds oldest-first selection. Counts
appear in purge totals and the run ledger. The configured source's compact
capability/progress state survives history purge and restart. History provides
idempotency while retained; an expired retry cannot renew the source's
completion high-water mark even if a history row is inserted again.

The receiver does not change Pro's bounded, memory-only outbox: delivery gaps
or a final heartbeat lost during producer shutdown remain possible. H1 is not
a durable scan audit log. No real network inventory or paid provider calls are
needed for verification.

## Verification evidence (2026-11-03)

Environment: macOS arm64, Apple M2 Max, CGO enabled, local temporary SQLite
databases and synthetic payloads. No real scanner inventory or provider calls.

- `go vet ./...`, `go build ./...`, `go test ./...`, `go test -race ./...`,
  `go mod verify`, source `gofmt` and `git diff --check` passed.
- UI lint, production build, 12-state server-rendered smoke check and asset
  budget passed; JavaScript plus CSS is 30,000 bytes gzipped (204,800 limit).
- `WB_HEARTBEAT_SOAK=1 CGO_ENABLED=1 go test ./internal/server -run
  '^TestHeartbeatSoak$' -count=1 -v`: 3,000 signed H1 requests over 60.003s,
  50/s, proxy mode with coalesced overview refreshes; all 202, zero relay calls,
  p99 2.9805ms, ending heap 2,424,832 bytes, WAL 4,132,392 bytes. This measures
  H1 traffic over real loopback HTTP, not a mixed host-alert workload or LAN.
- `BenchmarkHeartbeatIngest -benchmem -benchtime=1s -count=5`: 74.062,
  74.300, 72.935, 72.990, 72.509 microseconds/op; 23,569–23,577 bytes/op;
  411 allocations/op. Includes fixture construction/signing and SQLite commit;
  excludes socket latency. No equivalent H1 path existed for a baseline run.
- Decoder fuzzing with two workers and `-fuzztime=10s`: final run completed
  107,679 executions without failure. This does not establish exhaustive safety.
- Functional cases cover concurrent duplicates, conflicting bytes, exact-key
  JSON, body-cap boundaries, status/count validation, source isolation, 512-event
  sequential bursts in each mode, a blocked relay, full-writer recovery,
  cancellation, migration from v24, retention budgets, purge/restart/replay,
  identity separation and event-free liveness aging across sources.

Two pre-existing test-fixture problems surfaced during required checks. The
closure-drain helper omitted its source scope and therefore skipped projection;
the unchanged baseline also failed its five-second budget. A temporary overlay
supplying the missing scope restored the original check, and that wiring is now
fixed without changing the budget. An initial race run also timed out in the
shutdown fixture using the default operator port. The fixture now uses an
ephemeral port; five isolated race runs and the full race suite passed afterward.
An external connection causing the earlier timeout was not captured, so that
specific cause remains unproven.

Manual browser checks and a deployed Pro-to-bridge operational walkthrough are
not recorded as passed here. This evidence is implementation verification,
not release approval or a declaration that the complete Plan phase is approved.
