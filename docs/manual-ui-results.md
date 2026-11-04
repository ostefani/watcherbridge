# Step 31 manual UI results

Source: Designv9 §8.4. No UI runner, browser automation or screenshot
tooling is introduced; a human walks this list against a local build
(`go run ./cmd/watcherbridge`, open `http://127.0.0.1:8081/`).

How to record: each check ends with a verdict line. Mark **Pass**,
**Fail** (file an issue, do not ship around it) or **Not run**.
Leave every applicable check as **Not run** until you walk it.
Deferred items belong to a later step and must never count as passing.
Unrun items never count as passing.

Release under test: Release 2 candidate, baseline 385fcb6 plus uncommitted Step 31 corrections. Date: 2026-11-03. Walked by: unavailable (human walkthrough pending).

## 1. Stream, startup and shutdown

- [ ] Cold start with an empty database shows Home quiet state
  ("Nothing needs review", zero-receipts wording), not an error.
  Verdict: **Not run**
- [ ] Kill the server mid-stream (`kill`), restart: the viewer
  reconnects within seconds, ConnectionStatus returns to live, and
  the overview refreshes. Verdict: **Not run**
- [ ] Disconnect handling: open two tabs, close one mid-stream
  during a burst; the other tab keeps receiving frames and the
  server log shows no writer stall. Verdict: **Not run**
- [ ] Stalled reader: start `curl -N` on `/events`, suspend it
  (Ctrl-Z) mid-burst so it stays connected but stops consuming;
  the server stays healthy for other tabs and the writer never
  stalls. (Closing a tab only exercises disconnect handling, not
  backpressure.) Verdict: **Not run**
- [ ] Shutdown with tabs open: in-flight fetches fail soft (error
  copy with Retry, never a blank page or a crash overlay).
  Verdict: **Not run**
- [ ] Empty snapshot: with receipts but no attention items, Home
  shows the verdict and the empty attention section ("Nothing
  flagged…"), and detail links are absent rather than dead.
  Verdict: **Not run**

## 2. Host, Origin and invalid queries

- [ ] `curl` `/api/briefing` without `?id=` returns 400, not 500.
  Verdict: **Not run**
- [ ] `/api/briefing?id=does-not-exist` returns 404 with JSON error.
  Verdict: **Not run**
- [ ] `/api/devices/nope`, `/api/devices/prof-XYZ`,
  `/api/devices/<id>?history_limit=51`,
  `/api/devices/<id>?history_after=nope` all return 400 (never a
  store lookup on a malformed ID). Verdict: **Not run**
- [ ] `/api/devices/prof-0123456789abcdef` (well-formed, unknown)
  returns 404. Verdict: **Not run**
- [ ] `/api/briefings?before=5:not-a-profile` returns 400.
  Verdict: **Not run**
- [ ] `Host` allowlisting: a foreign `Host` header on the viewer
  subtree is refused (no data served off loopback). Allowlisting
  is a scope check, not caller authentication.
  Verdict: **Not run**
- [ ] `Origin` handling: read-only viewer GETs do not require
  `Origin`; only state-changing routes enforce the Step 16 guard.
  Verdict: **Not run**

## 3. Every Home state

- [ ] Quiet, no receipts: "No receipts in the last … — quiet here
  means no data, not a secure network." Verdict: **Not run**
- [ ] Quiet with receipts, review, act: headline, counts and the
  attention list match `watcherbridge status` output (CLI/API
  parity). Verdict: **Not run**
- [ ] Degraded and blind: the amber banner names coverage, reasons
  list below the verdict, sources show per-source status.
  Verdict: **Not run**
- [ ] Load error with no overview: "Overview unavailable" plus the
  status code; the stream keeps retrying. Verdict: **Not run**
- [ ] Timer transitions without events: leave the tab open across a
  window boundary; headline and receipts update, no full-page
  reload. Verdict: **Not run**

## 4. Detail, device and health screens

- [ ] From Home attention, follow a briefing link: route is
  `#/briefing/<id>`, sections run Summary, Why this priority,
  Referenced events, Reported evidence, Retained reported context,
  Recommended check, How I checked this, Provenance.
  Verdict: **Not run**
- [ ] Stepper reads Baseline floor, model suggestion (or "none —
  baseline stands"), final priority; clamping is explicit when it
  happened; source badge matches the API `source` value, legacy
  reads "Earlier version (source unknown)". Verdict: **Not run**
- [ ] "How I checked this" reads "No agent trace is available for
  this briefing." — no empty panel, no investigation claim.
  Verdict: **Not run**
- [ ] Referenced events pair IDs with types; observations below them
  carry the retained view for exactly those events (event ID and
  role in each heading), never a profile's latest view.
  A changed event's previous and current links for one profile
  render as two rows. Verdict: **Not run**
- [ ] Profile links come only from persisted links; a briefing with
  no links says so; truncated sections say so. Verdict: **Not run**
- [ ] Recommended check is text. There is no button, form or link
  that scans, enforces or otherwise acts on the network.
  Verdict: **Not run**
- [ ] Reported hosts list pages oldest-first; "Load more" appends
  without duplicating rows even when a profile gains another
  report mid-paging; exhaustion reads "End of retained list"; the
  200-row cap notice appears instead of a silent stop.
  Verdict: **Not run**
- [ ] Profile page: class wording distinguishes device, address
  context, unresolved events and legacy; legacy carries the "not
  used for current identity" notice. Verdict: **Not run**
- [ ] Profile page shows attributed vendor/type/OS/randomization
  only when reported, "unknown" otherwise; operator label reads
  "Your label". Verdict: **Not run**
- [ ] History pages independently ("Load more reports"); the end
  reads "End of retained history" with a pointer to Health for the
  retention policy — never a purge claim from an empty page.
  A normal (non-snapshot) load shows the first history page, not
  an empty section. Verdict: **Not run**
- [ ] Health: summary fits one glance (coverage, fallback 1h,
  parked, forward errors, queue); purge failure, partial or
  error-bearing completed run is visible without expanding;
  counters read as process-lifetime; unknown capabilities read as
  unknown. Verdict: **Not run**
- [ ] Health retention reads "Configured retention: N days …;
  policy only — it never proves a per-profile boundary"; the
  latest fully successful purge is shown separately and only from
  the last-success record. Verdict: **Not run**
- [ ] Failed refresh keeps data: with Health/Device(s) loaded,
  serving a 500 (proxy) and triggering reload shows a notice with
  Retry while existing rows stay visible. Verdict: **Not run**

## 5. Hostile text on every view

Use a scanner report containing `<img src=x onerror=alert(1)>`,
backtick/quote breaks and a 300-rune hostname in: hostname,
banner, evidence text and operator label positions.

- [ ] Home (headline, reasons, source reasons): inert text, no
  alert, no broken layout. Verdict: **Not run**
- [ ] Detail (every section incl. attribute keys/values and ports):
  inert text. Verdict: **Not run**
- [ ] Device list, profile (incl. "Your label"), history rows:
  inert text. Verdict: **Not run**
- [ ] Health (source reasons, purge error text): inert text.
  Verdict: **Not run**
- [ ] View source of each page: no unescaped `<script`, `<img` or
  event-handler attributes from report content. (The SSR smoke
  covers the same property; this is the live confirmation.)
  Verdict: **Not run**

## 6. Keyboard, focus, selection and scroll

Only the working shortcuts are listed in-app (`?` panel). Decision
shortcuts arrive with Step 25 and are not tested here.

Known gaps in this commit (do not mark passing): independent
master/detail pane scrolling (single-column layout) and text
clamping with Show more (`<details>` disclosure used instead).

- [ ] `j`/`k` move focus between rows in the attention list and the
  reported-hosts list; focus is visible; `Enter` follows the
  focused row's link. Verdict: **Not run**
- [ ] `g h` goes Home, `g d` goes to Reported hosts, `?` toggles the
  shortcut list. Verdict: **Not run**
- [ ] Shortcuts stay silent while typing (focus a button label
  filter if present, or any input): typing `j`, `g`, `?` enters
  text and navigates nowhere. Verdict: **Not run**
- [ ] Paging ("Load more", "Load more reports") and reconnect
  refreshes keep scroll position; selection (open `<details>`,
  focused row) survives updates that do not replace it.
  Verdict: **Not run**
- [ ] Every list row is reachable and operable by Tab/Enter alone;
  `aria-current` marks the active nav item. Verdict: **Not run**

## 7. Phone width, dark mode, reduced motion, time

- [ ] At 360px width: content stacks vertically, no horizontal
  scroll, definition lists wrap. Verdict: **Not run**
- [ ] Dark OS theme: all copy readable, focus outlines visible,
  pills and links keep contrast (no color-alone meaning).
  Verdict: **Not run**
- [ ] `prefers-reduced-motion`: no transitions or animation.
  Verdict: **Not run**
- [ ] Every timestamp shows local time with the UTC instant on
  hover (`title`); invalid instants read "unknown", never "Invalid
  Date". Verdict: **Not run**

## 8. History paging, reset and retry

- [ ] History and list paging survive a failed page: the error
  names the failure, Retry re-requests only that page, earlier
  rows stay. Verdict: **Not run**
- [ ] Route changes, profile changes and unmounts invalidate stale
  responses: navigating away mid-load never paints the previous
  screen's data onto the new one. Verdict: **Not run**
- [ ] Reconnect marks Devices/Device stale with a Reload control
  (position kept); Health refreshes on the reconnect edge.
  Verdict: **Not run**
- [ ] Reload resets paging without duplicating rows.
  Verdict: **Not run**

## 9. Wording and identity honesty

- [ ] No screen claims a physical-device count, normal ports, an
  active-hours pattern, live/dead status from silence, or host
  presence from coverage. Verdict: **Not run**
- [ ] "Earlier history was purged" (or equivalent) appears only
  where backed by retention metadata; empty pages say "end of
  retained", never "purged". Verdict: **Not run**
- [ ] Matching attribute values across profiles never merge them:
  two profiles sharing a MAC/hostname stay separate pages with
  separate histories. Verdict: **Not run**
- [ ] Upstream metadata absence reads as unknown ("unknown until
  heartbeat capability", "not reported"), never as a negative
  claim about the scanner. Verdict: **Not run**
- [ ] Device vs address vs legacy wording matches §4 above on
  every screen that names a class. Verdict: **Not run**

## 10. Retention cutoffs and purge health

- [ ] With an aged database past the report horizon: old rows are
  gone, the Health purge section records the completed run with
  per-table counts, and device pages show the policy line.
  Verdict: **Not run**
- [ ] With a failed purge run: Health shows the failure and error
  without expanding, and the last success stays reported
  separately. Verdict: **Not run**

## 11. Decisions (Step 25)

- [ ] Briefing dossier: expected / not-expected / snooze / note
  record and round-trip; the audit trail lists them newest-first
  with undone markers; Undo appends an undo row (no tombstones).
  Verdict: **Not run**
- [ ] Profile page: note and label record; label head shows under
  "Your label"; undoing the head restores the prior label;
  undoing a stale head is refused, never a silent overwrite.
  Verdict: **Not run**
- [ ] Briefings never offer label; profiles never offer review
  kinds. A label attempt on a briefing and a review kind on a
  profile are refused by the server. Verdict: **Not run**
- [ ] Snooze without a time takes the configured default horizon;
  a past horizon is refused; a lapsed snooze resumes the earlier
  review. Verdict: **Not run**
- [ ] Status pills: dossier shows Reviewed / Snoozed / Flagged /
  No operator review; digest shows expected counts per kind;
  attention rows keep their flagged pills. Verdict: **Not run**
- [ ] Flagged items floor the verdict at least at review without
  mutating priority: flag a briefing not-expected, check Home reads
  review (or act when another high-priority briefing needs a check)
  and the priority is unchanged. Verdict: **Not run**
- [ ] Shortcuts e / s / x on a briefing preset the composer kind
  and focus submit; nothing submits on its own; keys stay silent
  while typing. Verdict: **Not run**
- [ ] Hostile decision text (markup in note/label) renders as
  text, never elements, on dossier, profile and digest screens.
  Verdict: **Not run**
- [ ] Retry safety: submit, lose the network, submit again —
  one audit row, not two. Verdict: **Not run**

## 12. Incidents (Step 29)

- [ ] Attention shows incident rows with floor pills and links to
  `#/incidents/<id>`; Show members expands the member briefings
  inline; member links open their briefing pages.
  Verdict: **Not run**
- [ ] Incident detail shows scope (source/target), open/closed
  state, floor, report vs qualified-reference counts with the
  shared-evidence note when they differ, member evidence with
  priorities, and truncation notices. Verdict: **Not run**
- [ ] Incident decisions require the member-inclusion choice:
  unchecked records incident-only (members stay in attention);
  checked fans out to the snapshot (later joiners stay untouched).
  Undo of a fan-out names the member decisions without touching
  them. Verdict: **Not run**
- [ ] Live incident SSE updates the attention cut and an open
  detail screen; disconnect/reconnect refetches overview and the
  incident list. Verdict: **Not run**
- [ ] Hostile incident text (markup in namespace/revision/note)
  renders as text, never elements, on attention, detail and
  member rows. Verdict: **Not run**
- [ ] Keyboard: j/k move between attention rows; incident expand
  is a native button; shortcuts stay silent while typing.
  Verdict: **Not run**

## 13. Production assets and CSP

- [ ] `make ui-check` is green: lint (fatal a11y), build, smoke,
  ≤200 KiB gzip. Verdict: **Not run**
- [ ] `make ui-drift` is green against the committed dist.
  Verdict: **Not run**
- [ ] Browser console on every screen: no CSP violations, no
  errors, no failed asset loads. Verdict: **Not run**
- [ ] Served HTML contains no inline scripts; assets load from the
  embedded bundle only. Verdict: **Not run**

All 75 items remain Not run; automated checks do not replace the human walkthrough.
