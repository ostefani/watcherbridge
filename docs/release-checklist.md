# Release 2 evidence checklist — Step 31 in progress

This is an evidence ledger, not release sign-off. Governing requirements:
Designv9 §§9, 12 and Planv4 §31. Baseline commit: `385fcb6`; this working
tree contains uncommitted Step 31 corrections, so final evidence must record
the eventual tested tree hash and status.

## Scope and capability

Release 2 includes the retained Release 1 receipt, briefing, viewer and
decision surface plus investigator, exact rules and presentation incidents.
Stable provided-device behavior remains unavailable without verified upstream
I1. Synthetic `BindSourceIdentity` registration in `e2e/run.sh` exercises the
bridge contract only; it is no proof of upstream interoperability. H1 absence
must show unknown liveness. Optional H1 heartbeat receipt must not enter
grouping, provider, or forwarding.

## Executable scenario ledger

`e2e/matrix.md` records the intended matrix. Results must be added here from
an actual run; a matrix row alone is not a pass.

| Scope | Evidence | Verdict |
| --- | --- | --- |
| Tap/proxy × relay present/absent × synthetic identity registration present/absent × H1 present/absent | `e2e/run.sh` MX; `matrix.tsv` in archived bundle | 16/16 cells passed on 2026-11-04; provided fixture remained event-local in every cell because I1 namespace/lifetime evidence is absent |
| Tap/relay present and absent, proxy/relay present and absent, duplicate and mismatch | `e2e/run.sh` A–F, I, D2 | Passed in final `E2E_SKIP_LONG=1` run |
| Capability absent and synthetically supported; namespace stop/rebind/restart | `e2e/run.sh` G1, G2, H | Passed in final fast run |
| H1 absent, stale, proxy local-only | `e2e/run.sh` H1a–c | Passed in final fast run |
| SSE ordered replay and resume | `e2e/run.sh` S (five events) | Passed five-frame replay; under-load requirement pending |
| Open-group and processing-group crash | `e2e/run.sh` E, K | Open group observed and restart passed; processing crash target missed, pending |
| Incident and purge crash | `e2e/run.sh` K2, KP2 | Restart assertions passed; mid-operation proof pending; KP2 running state missed on final-tree run |
| Mid-projection crash | No synchronized scenario | Pending |
| Retention eligibility, total transaction writes, renewal and isolation | Current M is diagnostic only | Pending |
| v13 supported migration preservation | `e2e/snapshot.py` hashes pre-upgrade columns of raw events, briefings and pseudonym counters | Passed final fast run: 60 raw events, 29 briefings; other supported lineages and representative size pending |
| Newer schema refusal unchanged | `TestOpenRefusesNewerVersion`; `e2e/run.sh` NEGMIG | Passed byte comparison and version 9999 refusal after read-only schema preflight |
| Archived lineage refusal unchanged | Store regression test | Representative archived fixture pending |
| Fake-provider multi-turn, blocked consumers, rule invalidation, reported gaps | No complete E2E scenario | Pending |

The final fast run's synthetic database/log bundle is archived at
[`evidence/e2e-20261104T161745Z.tar.gz`](evidence/e2e-20261104T161745Z.tar.gz)
(SHA-256 `8e5dae92e0925d0305dc901571e8a3d864065369c52013bc26c6ceccf5728b10`).
The 16-cell matrix bundle is
[`evidence/e2e-matrix-20261104T163524Z.tar.gz`](evidence/e2e-matrix-20261104T163524Z.tar.gz)
(SHA-256 `d248b2f59e161251e6d4895cd63fd974577ff15fb6f54d6cb5da8d02fc0ced99`).
The runner keeps separate timestamped failure bundles under `.e2e-failures/`
and writes `.e2e-pending.txt` when a crash target is missed. The final fast
run intentionally skipped M/KP2. A separate final-tree
`E2E_RETENTION_ONLY=1` run exercised M and KP2; M's diagnostic assertions
passed and KP2 again missed the running purge state. Its bundle is
[`evidence/e2e-retention-20261104T162400Z.tar.gz`](evidence/e2e-retention-20261104T162400Z.tar.gz)
(SHA-256 `1028a10118e887691491dfc1def4cfd357a1f27ced6f6e4f9e67f6a2e360d75c`).

## Load and resource gates

`e2e/load.sh` offers a 512-event burst and 50/s for 60 seconds in tap and
proxy, three repetitions, with a direct relay baseline. Each event has a
unique ID and captured body hash. The runner checks HTTP acceptance, unique
stored count per phase, and proxy relay body hash/count after restart sweep.
It writes machine-readable phase JSONL, summaries and `verdicts.json` under
`e2e/results/<timestamp>/`; failure retains process logs and synthetic DBs.
The 2026-11-03 run's compact evidence is in
[`evidence/load-20261103T230510Z`](evidence/load-20261103T230510Z).
The shell traffic run generated all phases, but an edit to the script during
execution caused a shell parse failure before verdicts. The retained artifacts
were then evaluated by the extracted `e2e/load_verdict.py`; this is a
post-run verdict, not a claim that the original shell process exited cleanly.
The paired proxy calculation was added afterward and recomputed from the
retained per-event JSONL; no traffic was replayed for that calculation.
The compact package retains phase records, relay hash manifests, WAL samples,
GC traces, and the verdict. It does not retain the three SQLite databases
from that run, so the stored-count check cannot be independently replayed
from the compact package. The runner now copies databases into local results
on exit for future runs; that correction was not part of this measurement.

| Gate | Current definition and state |
| --- | --- |
| Zero unexplained loss | 3 repetitions: each mode offered/accepted/stored 512 burst + 3,000 sustained; proxy captures match all 3,512 body hashes per repetition. No observed loss in this workload; pending delivery under failure remains a separate gate |
| Tap p99 ≤50 ms | Pass for six measured phases; burst 4.068–5.952 ms, sustained 5.764–7.404 ms, real HTTP response latency |
| Added proxy p99 ≤25 ms | Matched-workload estimate passed six phases: p99 of per-index proxy HTTP latency minus direct-to-relay HTTP latency was 4.351–6.967 ms. The runs were sequential and event-ID prefixes differed, so run-order and small payload-shape noise remain limitations |
| Bridge heap ≤64 MiB | GC trace parsing accepts integer/decimal MiB and records pre/peak/post values; GC-paced sampling does not cover the complete interval, so gate remains pending |
| WAL ≤64 MiB | Measured sample maxima 4.03–4.62 MiB across six bridge runs; checkpoint/restart and overlapping purge coverage pending |
| Purge writes ≤200/transaction | Existing store tests are narrower evidence; representative run instrumentation pending |
| UI ≤200 KiB gzip | `make ui-check` and `make ui-drift` passed; JS+CSS 32,588 bytes gzip (other shipped assets reported in build output) |

Observed GC trace maxima were 10–12 MiB across six bridge runs, with
1,637–2,102 GC samples per run. They are diagnostic observations because
GC-paced traces do not bound all instants. Workload metadata is recorded in
each run's `env.txt`. Sustained arrival
count, dropped unsent arrivals, and response errors must be reviewed; a rate
generator status alone is not a zero-loss verdict. Overlapping purge and
blocked-peer repetitions remain pending.

## Automated verification on this working tree

`gofmt -l .`, `CGO_ENABLED=1 go vet ./...`, `CGO_ENABLED=1 go build
./...`, `CGO_ENABLED=1 go test ./...`, `CGO_ENABLED=1 go test -race
./...`, and `go mod verify` passed after the schema preflight change. The later
test-only archived-lineage byte assertion passed its focused rerun.
`make ui-check` and `make ui-drift` passed after the pinned npm dependencies
were installed in a permitted environment.
`python3 -B e2e/test_load_verdict.py` passed three calculation regressions,
including integer GC trace fields and a missing matched-baseline index.

The focused store benchmarks ran with `-benchmem -benchtime=1x -count=5`:
503-member incident formation 21.9–26.2 ms/op, quiet incident 96–129 µs/op,
and reported projection 19.8–20.6 ms/op. These are five one-operation
measurements with 503 seeded members for the incident cases; they are not a
soak test. The initial default-duration benchmark was interrupted after
387 seconds because fixture setup dominated its runtime; its partial output
is not used as release evidence. No new parser or serializer boundary was
added, so no new fuzz target was run for this change.

## Manual UI gate

The entire `.innerdoc/ui-manual-checklist.md` requires a human walkthrough.
[`manual-ui-results.md`](manual-ui-results.md) records all 75 items as
**Not run**, with date and unavailable reviewer. Automated HTTP/UI checks do
not replace this gate.

## Step 30 provenance and follow-up

[`step30-matrix-3d0356b.md`](step30-matrix-3d0356b.md) archives the prior
working matrix from `/private/tmp/step30-matrix.md` (baseline `3d0356b`,
clean tree, Go 1.26.4 darwin/arm64). Its `OK` entries are prior-run claims,
not reverified for this Step 31 tree. Its `ENV`, `GAP`, `MANUAL`, and `PEND`
entries remain open until independently checked. Step 30 restrictions and
security findings must be reconciled against the final tested tree before
sign-off.

The archived matrix records two redactor leak-scan fixes, signed-envelope
consistency enforcement, and a viewer unknown-API-path fix. It also records
cross-process database ownership as a remaining gap and a scanner-error
branch as inspected but not runnable without fault injection. This Step 31
run exercised loopback E2E under permitted execution and repeated the full
Go/race suites; it did not provide the human browser review or prove
exclusive database ownership across processes.

No release or phase completion is declared.
