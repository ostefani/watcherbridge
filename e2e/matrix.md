# Step 31 E2E scenario matrix

Original harness baseline: commit `adb8a96`. Step 31 executions in this
document use baseline commit `385fcb6` plus uncommitted corrections; the
working tree is not clean. Bridge scope defaults to
`default` (no `-source-id` passed); capability registration (if any)
is seeded with `e2e/register` before bridge start — never through a
second process while the bridge owns the database, never from payloads.
Synthetic registration proves bridge behavior under a configured
contract, never upstream I1 interoperability.

Conventions: `send_event` rewrites the body envelope to the header
identity before signing; `send_mismatch` sends fixture bytes verbatim
as an explicit negative. Counts below are unique accepted events
unless noted; duplicates/reposts are asserted separately.

## Execution status (2026-11-03)

The first full `e2e/run.sh` execution passed A–S and M, but K missed the
processing window, KP2 missed the active purge window, and the then-invalid
v13 fixture stopped at MIG. The fixture was corrected and the migration-only
tail passed MIG and NEGMIG; MIGK still missed an intermediate migration
version. A final `E2E_SKIP_LONG=1` run on 2026-11-04 passed A–S and the
migration tail on the current store code. K and MIGK still missed their crash
targets; M/KP2 were intentionally skipped in that run. These missed or skipped
targets remain pending, not passes. The fast-run bundle is archived under
`docs/evidence/e2e-20261104T161745Z.tar.gz`.
The separate final-tree `E2E_RETENTION_ONLY=1` run passed M's diagnostic
assertions; KP2 did not observe a running purge and remains pending. Its
bundle is `docs/evidence/e2e-retention-20261104T162400Z.tar.gz`.
The separate `E2E_MATRIX_ONLY=1 E2E_SKIP_LONG=1 E2E_SKIP_MIG=1` run
executed all 16 mode/relay/registration/H1 combinations; `matrix.tsv` in
`docs/evidence/e2e-matrix-20261104T163524Z.tar.gz` records each response,
stored event, briefing, heartbeat and projection class. The provided-ID
fixture remained event-local even with synthetic registration because it
does not carry a verified I1 namespace/lifetime contract. G2 separately
exercises exact MAC-reference incidents under synthetic registration.

## Baseline (e2e/run.sh, must stay green)

| Cell | Mode | Relay | Fixtures | Expect |
|---|---|---|---|---|
| A | tap | present | changed, appeared (legacy shape) | 2×202, 2 briefings, 2 annotations, no severity written |
| B | tap | absent | changed | 202, 1 briefing, local unaffected |
| C | proxy | present | changed + `-out` bytes | 202, 1 briefing, 1 annotation, sha256(sent) == relay record |
| D | proxy | absent | changed | 502, briefing still completes |
| E | tap | present | changed, kill -9 mid-window, restart, idempotent repost | exactly 1 briefing + 1 annotation after drain |
| F | tap | n/a | qualified appeared (202) then verbatim mismatch (400) | 400, no new briefing after window |

## Capability and identity (§2–§3)

| Cell | Setup | Expect |
|---|---|---|
| G1 absent | no registration | provided-ID reports stay event-local; identity-scoped rules and qualified counts unavailable (reason visible in Health) |
| G2 supported | register `default`/`e2e-ns-1`/`stable-identity-v1` | exact-reference association; ≥3 qualified refs in 5m form incidents; eligible rules |
| H namespace change | stop → register `e2e-ns-2` → restart (no live-transition surface exists; recorded as stop/rebind/restart) | no ownership transfer, no cross-namespace merge, no stale rule application |
| H1 absent | no heartbeats | unknown liveness; silence never implies blind or disappearance |
| H1 stale | heartbeat then silence past horizon | correct per-source blind transition |
| Heartbeats | signed H1 in tap and proxy | 202, source-health row only; no grouping/briefing/provider-send/forwarding |

## Delivery, recovery, streaming (§3)

| Cell | Expect |
|---|---|
| Duplicates/conflicting retries | idempotent no-op; single storage; single logical briefing and incident membership |
| Out-of-order delivery | same durable outcome as ordered (window/threshold logic is time-based, not arrival-based) |
| Delivery gaps | pending delivery reported separately; HTTP failure alone never asserts storage outcome; bounded drain defines "lost" |
| Kill mid open/processing group, projection, purge, incident tick, migration | restart resumes from durable cursor; idempotent replay; valid references; preserved counters; eventual completion (synchronized, not sleep-timed) |
| SSE under load | ordered replay or explicit resync; bounded client state; no stale overwrite of newer state |

## Aged data and migration (§4)

Seeded representative databases (documented row counts, payload sizes,
reference fan-out, cardinality, churn) must prove per-table eligibility
and deleted counts, ≤200 actual affected rows per maintenance
transaction (children/scratch/bookkeeping included), empty cascades,
interrupted-run accounting, fresh-reference renewal, pending-work and
incident evidence protection, monotonic pseudonym counters, purge
yield under pressure without ingest 503, and blocked-peer isolation.
Each supported migration lineage at representative size must preserve
raw bytes/text/IDs/sequences and quarantine legacy; unsupported schemas
refused with contents and schema unchanged. Startup/migration duration
and heap/WAL growth recorded.

## Load (§5)

512 unique signed events (documented launch schedule/concurrency) and
50 events/s × 60s (defined arrival schedule; open-loop generator) in
tap and proxy, ≥3 repetitions: zero unexplained loss with
attempt/unique accounting, tap p99 ≤50ms over the real HTTP path,
matched direct-to-stub baseline for added proxy overhead p99 ≤25ms,
sampled Go heap ≤64MiB, sampled WAL ≤64MiB with checkpoint behavior.
Machine-readable results plus explicit budget verdict per run.

## Manual UI and sign-off (§§6–7)

Full `.innerdoc/ui-manual-checklist.md` human walkthrough with
Pass/Fail/Not-run verdicts; `docs/release-checklist.md` evidence
package on completion. No release-complete declaration by these
documents.
