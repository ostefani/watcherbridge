// Characterization for the folder split (plan steps 6-7). Plain node,
// no test runner: SSR-incompatible runtime behavior is pinned here at
// the unit level, and component wiring bugs are pinned structurally
// until Phase 5 replaces them with behavioral tests. Anything marked
// BUG-* fails today by design; Phase 2/5 flips those pins green.
//
// Covered by scripts/smoke.mjs already (not duplicated here): SSE
// incident recovery, undo member continuation, purge attention flags,
// guard latest-wins basics. Covered only by the human checklist:
// focus movement, reconnect rendering, decision submit/undo round-trips.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { createFetchGuard } from '../src/lib/freshness.js';
import { nextPageRows } from '../src/lib/paging.js';
import { typingTarget } from '../src/lib/roving.js';
import {
  displayTime, sourceLabel, classLabel, changeLabel, windowLabel,
  reportRange, reviewTitle, singleViewIdentity, randomizedLabel
} from '../src/lib/format.js';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
let pass = 0;
const bugs = [];
function check(name, cond) {
  if (!cond) {
    console.error(`FAIL ${name}`);
    process.exitCode = 1;
    return;
  }
  pass += 1;
}
// A failing pin documents a suspected bug with a reproducible case.
// It stays red until the owning migration fixes the root cause.
function bugPin(id, name, cond) {
  if (cond) {
    pass += 1;
    return;
  }
  bugs.push(`${id}: ${name}`);
}

// Latest-wins fencing across reload and paging intents.
{
  const guard = createFetchGuard();
  const first = guard.start();
  guard.streamed();
  const second = guard.start();
  check('stream frame retires in-flight fetch', !guard.current(first) && guard.current(second));
  guard.invalidate();
  check('unmount retires everything', !guard.current(second));
}

// Paging merges by key, keeps first-seen order, stops at the cap.
{
  const rows = nextPageRows(
    [{ profile_id: 'a' }, { profile_id: 'b' }],
    [{ profile_id: 'b' }, { profile_id: 'c' }],
    (d) => d.profile_id, 3
  );
  check('page dedupes and appends', rows.length === 1 && rows[0].profile_id === 'c');
  const capped = nextPageRows([{ profile_id: 'a' }], [{ profile_id: 'b' }, { profile_id: 'c' }], (d) => d.profile_id, 2);
  check('page stops at cap', capped.length === 1);
}

// Shortcuts stay silent while typing, live elsewhere.
{
  const key = (target) => typingTarget({ target });
  check('typing targets silence shortcuts',
    key({ tagName: 'INPUT' }) && key({ tagName: 'TEXTAREA' }) && key({ tagName: 'SELECT' }) &&
    key({ tagName: 'DIV', isContentEditable: true }) && key(null));
  check('buttons and rows keep shortcuts',
    !key({ tagName: 'BUTTON' }) && !key({ tagName: 'A' }) && !key({ tagName: 'BODY' }));
}

// Display helpers: unknown stays unknown, hostile text passes through
// verbatim (it renders inert because every call site binds text).
{
  const hostile = '<img src=x onerror=alert(1)>';
  check('bad instant reads unknown', displayTime('nope').local === 'unknown');
  check('good instant carries utc tooltip', displayTime('2026-11-01T00:00:00Z').utc.includes('2026'));
  check('unknown source stays unknown', sourceLabel('sponsor') === 'Unknown source');
  check('unknown class stays unknown', classLabel('device') === 'Unknown class');
  check('unknown change kind passes through', changeLabel(hostile) === hostile);
  check('window label shortens hours', windowLabel('24h0m0s') === '24 hours');
  check('empty refs have no range', reportRange([]) === null);
  const single = reportRange([{ event_id: 'e1', occurred_at: '2026-11-01T00:00:00Z' }]);
  check('single event range has no dash', single !== null && !single.local.includes('–') && !single.partial);
  const range = reportRange([
    { event_id: 'e1', occurred_at: '2026-11-01T00:00:00Z' },
    { event_id: 'e2', occurred_at: 'garbage' }
  ]);
  check('unparseable time marks partial range', range !== null && range.partial);
  const obs = [
    { event_id: 'e1', view_role: 'previous', attributes: { identity_confidence: 'high' } },
    { event_id: 'e1', view_role: 'current', attributes: { identity_confidence: 'low' } }
  ];
  check('confidence decrease is named', reviewTitle('other', obs, []) === 'Reported identity confidence decreased');
  check('kind wins over views', reviewTitle('port_closed', obs, []) === 'Ports closed');
  check('single view resolves identity',
    singleViewIdentity([{ event_id: 'e1', view_role: 'host', attributes: { hostname: '', ip: '10.0.0.1', mac: null } }])?.ip === '10.0.0.1');
  check('conflicting views collapse to per-event rendering',
    singleViewIdentity([
      { event_id: 'e1', view_role: 'current', attributes: {} },
      { event_id: 'e2', view_role: 'current', attributes: {} }
    ]) === null);
  check('randomization tri-state', randomizedLabel(true).startsWith('Randomized') &&
    randomizedLabel(false).startsWith('Not randomized') && randomizedLabel(null) === 'Randomization unknown');
}

// BUG-1: Devices reload and load-more share one guard. A reload that
// starts first and lands after a page start is dropped, so the list
// keeps stale rows and appends the page onto them. Phase 2 gives each
// operation its own guard (latestOnly per operation).
{
  const guard = createFetchGuard();
  const reloadToken = guard.start();
  guard.start(); // concurrent load-more intent takes the single slot
  bugPin('BUG-1', 'reload completion applies after a concurrent page start', guard.current(reloadToken));
}

// BUG-2: Detail keeps the previous briefing rendered while the next
// id fetches (embedded selected-review switches briefings without
// resetting). Structural pin: the id-change effect must clear the
// rendered briefing before refetching. Phase 5 replaces this with a
// keyed resource reset covered by a behavioral test.
{
  const detail = readFileSync(join(root, 'src/lib/Detail.svelte'), 'utf8');
  const effect = detail.slice(detail.indexOf('Refetch only when the briefing ID changes'));
  bugPin('BUG-2', 'id change clears the rendered briefing before refetch',
    /detail\s*=\s*null/.test(effect.split('$effect')[1] ?? ''));
}

// BUG-3: Health mounted from a snapshot never refreshes on reconnect:
// the edge requires `health !== null`, but snapshot screens keep
// `health` null forever (only `shown` sees the snapshot). Structural
// pin until HealthContainer owns reconnect policy via createResource.
{
  const health = readFileSync(join(root, 'src/lib/Health.svelte'), 'utf8');
  bugPin('BUG-3', 'reconnect edge admits snapshot state',
    /live\s*&&\s*!wasLive\s*&&\s*shown\s*!==\s*null/.test(health));
}

console.log(`characterize: ${pass} checks passed`);
if (bugs.length > 0) {
  for (const b of bugs) {
    console.log(`KNOWN BUG (failing pin): ${b}`);
  }
  process.exitCode = 1;
}
