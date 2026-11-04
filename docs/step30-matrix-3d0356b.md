# Step 30 requirement-to-evidence matrix (working file)

Baseline: commit 3d0356b, clean tree, go1.26.4 darwin/arm64 + clang.
Legend: OK = existing test verified by running; ADD = new test needed;
VERIFY = exists, must run; GAP = no coverage found; MANUAL = human check;
ENV = blocked in sandbox, needs permitted env; PEND = pending.

## 1. Source authentication and scope

| # | Requirement (Plan §30.1) | Surface | Evidence / test | Status |
|---|---|---|---|---|
| 1.1 | Valid HMAC delivery accepted | POST /webhook | TestContractTable | OK |
| 1.2 | Bad signature 401 | POST /webhook | TestContractTable | OK |
| 1.3 | Stale/future timestamp 401 | POST /webhook | TestContractTable | OK |
| 1.4 | Timestamp boundary (exactly at skew edge) | validate.go | TestAuthTimestampBoundary (unit+handler) | OK |
| 1.5 | Missing signature 401 | POST /webhook | TestAuthMissingAndMalformedSignature | OK |
| 1.6 | Malformed signature encoding 401 | POST /webhook | TestAuthMissingAndMalformedSignature | OK |
| 1.7 | Signed header/body metadata mismatch | POST /webhook | TestAuthMissingAndMalformedSignature | OK |
| 1.8 | Stale replay rejected, legitimate duplicate follows idempotency contract | POST /webhook ×2 | TestContractTable | OK |
| 1.9 | Auth precedes admission/processing | server.go | TestContractTable + handler order | OK |
| 1.10 | Scope from receiver config, not payload | BindSourceScope(main)/ingest | TestScopeSmugglingRejected + StoredEvent scope | OK |
| 1.11 | Payload names/targets/addrs/secrets can't change source | ingest | TestScopeSmugglingRejected | OK |
| 1.12 | Single-source default durable across restart | config+store | TestBindSourceScopeAcceptsOwnScopeAcrossRestart | OK |
| 1.13 | Ambiguous multi-source config refused | config.Validate | TestBindSourceScopeRefusesForeignScope | OK |
| 1.14 | Receipt keys include scope + signed event ID | store/raw_events | raw_events(event_id PK = signed ID, source_scope col) + TestScopeSmugglingRejected + contract duplicate-idempotency | OK |
| 1.15 | Matching attrs never cross-source associate | projection | TestSameReferenceNeverAssociatesAcrossScopes | OK |

## 2. Producer contract and provenance

| # | Requirement | Surface | Evidence | Status |
|---|---|---|---|---|
| 2.1 | Serializer field comparison recorded | scanner-contract.md §6 | §6 field comparison re-verified vs Pro ff96be3 + models 6bb2a94; null-view-key + fingerprint-format + always-emitted-key fixture completion; TestProducerFixturesCarrySerializerKeys pins presence, unknown stays negative | OK |
| 2.2 | MAC-reported references | projection | TestProducerContractAcceptance (appeared-mac + changed-prevcur triples) | OK |
| 2.3 | IP-derived low-confidence | projection | TestProducerContractAcceptance (appeared-ip triple) + TestGroupKeyDerivation/ip-derived-stays-address-scoped | OK |
| 2.4 | Provided w/o contract → unavailable | rules/incidents | TestDeriveProposalsNeedsBoundContract + TestGroupKeyDerivation/provided-without-contract-stays-event-local | OK |
| 2.5 | Synthetic qualified refs under test capability | rules/incidents | TestQualifyRuleSubjectMatrix + TestDeriveProposalsSkipsLowConfidence | OK |
| 2.6 | Missing/unknown/inconsistent/oversized/unsupported provenance | ingest+store | TestProducerEnvelopeRejectsTap (413/400/400) + TestProducerInconsistentPrevCurrentPreserved + TestProducerUnknownFieldsPreserved + TestProducerMissingViewsAcceptedEventLocal | OK |
| 2.7 | Prev/current different refs, ns/rev changes | projection | TestGroupKeyDerivation/changed-prefers-current + TestProjectPreviousCurrentStayDistinct + TestProducerInconsistentPrevCurrentPreserved | OK |
| 2.8 | Exact bytes, single storage, runtime qualification | store | TestProducerContractAcceptance (byte compare + envelope + dup) + negatives byte-compare | OK |
| 2.9 | §9 metadata limits (200/256/500/256) | projection | TestOversizedCollectionsDecodeCapped + TestPortFieldTruncationReported + TestGroupKeyDerivation/over-bound-host_id | OK |
| 2.10 | Truncation never creates identity key | projection | TestGroupKeyDerivation/contradictory-refs + oversized-mac-with-ip stay event-local | OK |

## 3. Browser and HTTP boundaries

| # | Requirement | Surface | Evidence | Status |
|---|---|---|---|---|
| 3.1 | Host check before routing canonicalization | viewer mux | TestHostAllowlist, DenyByDefault, LoopbackHosts | OK |
| 3.2 | Exact host/port + explicit-empty denial | viewer | TestHostAllowlist, DenyByDefault | OK |
| 3.3 | Webhook exception keeps method/HMAC behavior | server mux | TestViewerHostGateLeavesWebhookAlone | OK |
| 3.4 | POST: method+JSON+Origin+Fetch-Meta+8KiB | viewer guard | TestPostGuard, TestSameOriginBinding | OK |
| 3.5 | GETs usable w/o Origin, no side effects | viewer | TestGetRoutesHaveNoSideEffectsWithoutOrigin (9 routes 200 + headed, counts unchanged; bare /api/decisions 400 Origin-independent) | OK |
| 3.6 | Numeric/cursor grammar per route incl incidents/rules | viewer | TestIncidentCursorBoundaries (11 malformed 400 + far-future 200-empty) + incidents_test bad-before/limit + decisions 400s + TestDevicesRejects + TestRuleRoutesGuards | OK |
| 3.7 | Error paths keep headers+CSP | viewer | TestViewerSecurityHeaders, TestMuxResponsesGatedAndHeaded + TestMuxResponsesCarryHeaders (server) | OK |
| 3.8 | Unknown paths/redirects | viewer | TestUnknownPathsAndMethodMismatch: unknown GET /api/* → 404 JSON (new GET /api/ handler; was 200 index HTML), non-API typo → SPA 200, no Location anywhere | OK (fix) |
| 3.9 | Socket tests for timeout/slow/stream/shutdown | server/viewer | existing ( HungScanRelay, ShutdownDuringSweep...) | ENV (bind) |

## 4. Sensitive storage and ownership

| # | Requirement | Surface | Evidence | Status |
|---|---|---|---|---|
| 4.1 | 0600 files / 0700 dir, tightening | store.Open | TestSidecarsTightenedTo0600 + TestReopenRecoversExpiredGroupAndSidecarPerms | OK |
| 4.2 | Symlink refusal | store.Open | TestOpenRefusesSymlink | OK |
| 4.3 | Shared-dir conditions | store.Open | TestOpenRefusesWritableDir (refuse w/o sticky bit; sticky-bit shared-temp passes) | OK |
| 4.4 | Cross-process ownership | store.Open | GAP: no test opens one DB from two processes; SQLite locking is the only mechanism and the in-memory dedup gate is per-process, so multi-process deployment is out of contract | GAP (reported) |
| 4.5 | Migration ownership (transactional) | migrations | TestMigrateV1DatabaseWithData + TestMigrateV2BackfillsEventIDs + TestMigrateV3ToV4WithData | OK |
| 4.6 | Serialized runtime writes | Store.write | TestConcurrentWritersSerialize (explicit) | OK |
| 4.7 | Failure/cancellation paths | store | TestCloseConcurrentWaits + TestCloseDrainsQueuedJobs (store); TestCancelDuringPersistThenRetry is socket-harness | OK + ENV |
| 4.8 | Safe diagnostics (no creds/addrs/payloads/headers/bodies) | errors/logs | TestFailureResponsesCarryNoRequestContent (400/413/503 marker sweep) + TestSecretNeverPrints + TestHeadersJSONWhitelistEnforced; log/error audit: static strings + event IDs only | OK |
| 4.9 | Secret-file credentials preserved, no file-only requirement | config | TestSecretFileRules + TestSecretFloorScope + full config suite | OK |

## 5. Provider privacy and output acceptance

| # | Requirement | Surface | Evidence | Status |
|---|---|---|---|---|
| 5.1 | Final request capture all turns | agent | redactor suite OK (TestGoldenBundle, RoundTrip, BatchRestore) + briefing suite OK | OK |
| 5.2 | Hostile forms (v6 zones, mapped, CIDR, MAC, alias, scope, opaque) | redactor | redactor suite OK + 60s fuzz clean (77,935 execs); gate fuzz found 2 soundness holes, both fixed fail-closed in leak.go with corpus seeds kept (see findings) | OK (2 fixes) |
| 5.3 | Recursive tokenization + durable allocation | redactor | suite OK (RoundTrip, BatchRestore, AnchorTokenized, RestoreNoResubstitution) | OK |
| 5.4 | Pre-send serialized scan every send | agent | TestAgentTranscriptScanBlocksResend (TurnCalls==1, fallback with transcript-blocked reason) | OK |
| 5.5 | Leak + scanner error both block | agent | Leak-block e2e (TurnCalls bounded) + cap-block 0 sends; scanner-error caller branch is defensive-only (neutralizeReferenceIDs provably never emits invalid JSON: valid-marshal in, valid-marshal out) — inspected, not runnable; scanner half pinned by TestScannerErrorIsAnError; zero-send mapping shared + covered | OK (inspected) |
| 5.6 | Nothing appended after scanning | agent | TestAgentTranscriptScanBlocksResend (no retransmit) + oversized test (0 sends) | OK |
| 5.7 | Blocked tool → generic result, run continues | agent | briefing suite OK incl TestBlockedToolResultShape | OK |
| 5.8 | Whole-request block → documented fallback | agent | briefing suite OK (ProviderError/DoubleInvalid/FallbackKeepsTraceAndUsage) | OK |
| 5.9 | No creds in prompts/traces/diagnostics | agent/provider | TestProviderKeyHeaderOnly (header set, bodies key-free, status-only errors; stub tripper, no listener) | OK |
| 5.10 | Unknown tokens/invented evidence/malformed/oversized rejected | agent | briefing suite OK (InventedRef, OversizedLikely, DuplicateKeysBlockResend, RepairRecoversInvalidTurn) | OK |
| 5.11 | No critical manufacture / raise cap / floor lowering / severity change | agent | TestResolveMatrix (raise cap: baseline+1 max, never critical) + TestAgentConfidenceNeverSetsPriority + TestModelInjectionCannotEscalate + AgentBelowFloorIsDeterministic | OK |
| 5.12 | Loopback exception: startup validation, resolution controls, UI indication | config/UI | config suite OK (loopbackOrHTTPS reject/allow matrix); no runtime resolution pinning (operator-configured endpoint — accepted limitation); Health.svelte shows provider calls + fallback counts | OK (limitation noted) |

## 6. Hostile metadata and policy impersonation

| # | Requirement | Surface | Evidence | Status |
|---|---|---|---|---|
| 6.1 | Payload can't register source/enable capability | rules | TestProducerPayloadCannotRegisterIdentity (hostile ns/capabilities/scope fields stored verbatim, SourceIdentity stays absent); BindSourceIdentity has no production caller (main binds scope only) | OK |
| 6.2 | Payload can't supply labels/approvals/rules | decisions/rules | TestProducerPayloadCannotSupplyDecisionsOrRules (GetRule/GetRuleByProposal stay unknown, no incidents from ingest; decisions only via guarded AppendDecision) | OK |
| 6.3 | No policy borrowing across source/ns/rev/ref | rules/incidents | TestRuleApplyRejectsMixedRevision + TestQualifyRuleSubjectMatrix + TestRuleStaysInScope + TestSameReferenceNeverAssociatesAcrossScopes (§1) | OK |
| 6.4 | Weak/legacy never qualify counts | incidents | TestIncidentSameReferenceCountsOnce + TestLegacyProfileStateQuarantined + store suite OK | OK |
| 6.5 | No markup/SSE/log/SQL/terminal injection | all renders | TestHostileStringsSurviveAsInertText + TestShippedJSRendersTextOnly (viewer suite OK); error/log audit §4.8 static-only | OK |
| 6.6 | Heartbeat coverage ≠ presence/inventory | overview/health | TestHeartbeatTapExcludesHostPipeline (no receipts/groups, heartbeat-only source row) + TestHeartbeatCoverageAgesWithoutEvents; socket twin ENV | OK + ENV |
| 6.7 | Negative prod capability + positive synthetic exact-match | rules/incidents | TestDeriveProposalsNeedsBoundContract + briefing suite OK | OK |

## 7. Responsibility and outbound audit

| # | Requirement | Surface | Evidence | Status |
|---|---|---|---|---|
| 7.1 | Prohibited-behavior sweep (OUI/MAC-bit/fingerprint/match/rotation/alias/lifecycle/debounce/rescan/presence/merge/confidence) | code+config+deps+tools+UI+startup/purge/fallback | Sweep: zero inference hits — OUI/similarity/presence-estimation/merge/rescan absent; fingerprint hits are passthrough (producer field copy) + change/idempotency hashes; alias hits are redaction tokenizing + exact opaque-key lookup; UI presence hit is a disclaimer comment; quiet-window debounce is contract grouping | OK (audit) |
| 7.2 | Proxy bodies byte-identical | forwarder | TestForwardAuditByteIdenticalAndMinimal (stub transport, no listener) + socket golden TestForwardGoldenBytesAndHeaders ENV | OK + ENV |
| 7.3 | Severity unchanged | forwarder | Byte-identity (no rewrite stage exists between store and relay POST); producer schema carries no bridge-set severity | OK |
| 7.4 | Only permitted annotations to ScanRelay | forwarder | TestForwardAuditByteIdenticalAndMinimal (hostile Authorization/X-Custom/Cookie dropped; only X-SubnetLens-*/Content-Type pass) + TestHeadersJSONWhitelistEnforced | OK |
| 7.5 | Decisions/rules/incidents/traces/viewer-state local | store | Forward body == producer bytes (no augmentation stage); ListIncidents empty post-ingest; creds inject at send time from config only | OK |
| 7.6 | No new outbound/enrichment/telemetry path | deps+code | Outbound clients: provider model API + ScanRelay proxy (contract) + annotation publisher (same loopbackOrHTTPS URL rule, tap mode incl) + CLI status (loopback); go.mod single dep (go-sqlite3); no telemetry/exec/dial code | OK (audit) |

## 8. Legacy quarantine and schema support

| # | Requirement | Surface | Evidence | Status |
|---|---|---|---|---|
| 8.1 | Supported upgrade lineages recorded | migrations | TestMigrateSupportedFromV13 + V1/V2/V3→V4/V10/V13 migration tests | OK |
| 8.2 | Migration/restart, immutable bytes/IDs/seqs | store | Migration suite OK + TestGroupingProvenanceUpgrade + restart tests (§1 scope) | OK |
| 8.3 | Legacy visible but excluded from association/rules/counts | projection/rules/incidents | TestLegacyProfileStateQuarantined + TestIncidentSameReferenceCountsOnce (§6) | OK |
| 8.4 | Archived lineage refused unmodified | store.Open | TestOpenRefusesArchivedDeviceSchema + TestOpenRefusesNewerVersion + TestGroupingVersionChangeClosesPriorGroup | OK |
| 8.5 | Interruption/cancellation/purge refs | store | TestMigrateV13PurgeStateAndTokenUsage + TestPurgeFailedGroupEvents + TestCloseConcurrentWaits/TestCloseDrainsQueuedJobs (§4.7) | OK |

## 9. Manual UI security review

Checklist: .innerdoc/ui-manual-checklist.md (13 sections; human walkthrough
against a local build, no automation stack per constraints).

| # | Requirement | Surface | Evidence | Status |
|---|---|---|---|---|
| 9.1 | Checklist §2 API/host/origin cases automated | viewer routes | TestChecklistInvalidQueries (briefing bare-400/unknown-404, tuple-cursor 400, device malformed-400/unknown-404) + §3 guard/Origin tests | OK |
| 9.2 | Hostile text inert (checklist §5 server-side) | all renders | TestHostileStringsSurviveAsInertText + TestShippedJSRendersTextOnly; live-browser confirmation pending | OK + PENDING |
| 9.3 | Checklist §§1,3,4,6,7,8,9,10,11,12 human walkthrough | browser | Not run — needs human against local build | PENDING |
| 9.4 | Checklist §13 assets/CSP (`ui-check`, `ui-drift`, console, inline scripts) | toolchain/browser | Validation sweep runs ui-check/ui-drift; console/inline-script browser confirmation pending | PENDING (sweep runs tools) |

## Gate decision (Step 30 execution, unauthorized to mark complete)

- Audit-complete: §§1–9 exercised against the Step-29 tree. 73 requirement
  rows OK (of 78); 0 rows fail.
- Passed: all runnable checks green (fmt, vet, build, mod verify, full
  package suites for store/viewer/briefing/config/forwarder/overview/
  provider/redactor/cmd, race on touched packages, ui-check, ui-drift,
  60s redactor fuzz clean, other fuzz seeds replay clean).
- Pending (not passing, not failed): socket-harness server tests
  (ENV — sandbox denies loopback binds; tap twins added where the property
  is transport-independent); checklist §§1,3,4,6,7,8,9,10,11,12 human
  walkthrough + §13 browser-console/inline-script confirmation (PENDING);
  cross-process DB ownership out of contract (GAP reported).
- Fixes landed (uncommitted): redactor leakEagerQuad/leakEagerMAC
  fail-closed scan holes (2 fuzz findings + corpus seeds);
  viewer GET /api/ 404 for unknown API paths; producer fixture
  serializer-fidelity corrections; StoredEvent source_scope (§1).
- Step 30 MUST NOT be marked complete in Planv4 and no release sign-off
  is claimed: human UI walkthrough and a permitted-env socket run remain.
- Step 31 handoff: E2E/load/migration/release checklist may proceed on
  this tree; repeat §§2,6,7,9 slices after any Step 29 follow-up (R1
  surfaces first).

## Review round (P2 x3, gate work committed as 124056d)

1. Split-envelope acceptance (pre-existing, Designv9 §1.1): reproduced
   202 on all three mismatches, then enforced header/body envelope
   equality for host events (heartbeat path already did). Fix:
   checkEnvelopeConsistency in validate.go + call before gate/persist
   in server.go; TestProducerEnvelopeMismatchRejected (valid signature
   each case, 400 + nothing stored). Tap suite + race green.
2. Fixture fidelity: confirmed exact omissions; completed the five
   positive fixtures with always-emitted keys ("" where unreported);
   TestProducerFixturesCarrySerializerKeys pins presence and keeps
   appeared-unknown.json explicitly negative. Contract test green.
3. Scanner-error caller evidence: confirmed unreachable through the
   seam (neutralizeReferenceIDs: valid-marshal in, valid-marshal out),
   so no test can force it without fault injection. Recorded as
   inspected-not-runnable; scanner half pinned; shared zero-send
   mapping covered for leak + cap. No refactor undertaken.
