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
import { registerHooks } from 'node:module';
import { createFetchGuard } from '../src/lib/freshness.js';
import { nextPageRows } from '../src/lib/paging.js';
import { typingTarget } from '../src/lib/roving.js';

// `src/lib` modules import each other through the `@` alias, which
// plain node cannot resolve. This installs the zero-dependency resolve
// hook before the dynamic imports below (all static imports above are
// alias-free, so linking succeeds first).
registerHooks({
  resolve(specifier, context, next) {
    if (specifier === '@' || specifier.startsWith('@/')) {
      return { url: new URL(`../src/${specifier.slice(2)}`, import.meta.url).href, shortCircuit: true };
    }
    return next(specifier, context);
  }
});
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

// BUG-1 (fixed by the Devices migration): reload and load-more run on
// independent operations inside createPager, so a reload that lands
// after a page start still applies instead of being dropped.
{
  const { get } = await import('svelte/store');
  const { createPager } = await import('../src/lib/async/pager.js');
  const pending = [];
  const pager = createPager({
    fetchPage: (cursor) => new Promise((resolve) => pending.push({ cursor, resolve })),
    keyOf: (r) => r.id, cap: 10
  });
  const pReset = pager.loadMore(true); // reload starts first
  const pPage = pager.loadMore(); // page starts after, lands first
  pending[1].resolve({ rows: [{ id: 'p' }], nextAfter: 'p1' });
  check('page landing first applies', (await pPage) === 'applied' && get(pager.rows).length === 1);
  pending[0].resolve({ rows: [{ id: 'r' }], nextAfter: '' });
  check('reload landing after a page start still applies',
    (await pReset) === 'applied' && get(pager.rows).length === 1 && get(pager.rows)[0].id === 'r' &&
    get(pager.exhausted));
  // Reverse order: an append requested before a successful reset never
  // lands after it — a late page cannot reopen an exhausted list.
  {
    const late = [];
    const relisting = createPager({
      fetchPage: (cursor) => new Promise((resolve) => late.push({ cursor, resolve })),
      keyOf: (r) => r.id, cap: 10
    });
    const pPage = relisting.loadMore(); // append with the old cursor
    const pReload = relisting.reset(); // reset requested after
    late[1].resolve({ rows: [{ id: 'n' }], nextAfter: '' });
    check('reset landing before a pending append still applies',
      (await pReload) === 'applied' && get(relisting.rows).length === 1 && get(relisting.rows)[0].id === 'n');
    late[0].resolve({ rows: [{ id: 'o' }], nextAfter: 'old' });
    check('late append from the previous cursor lands stale',
      (await pPage) === 'stale' && get(relisting.rows).length === 1 && get(relisting.rows)[0].id === 'n' &&
      get(relisting.exhausted));
  }
  // Reload-first order: the append is requested after the reset but
  // still against the old cursor. The reset commits first and retires
  // it; the late page must not reopen the exhausted list.
  {
    const early = [];
    const reloading = createPager({
      fetchPage: (cursor) => new Promise((resolve) => early.push({ cursor, resolve })),
      keyOf: (r) => r.id, cap: 10
    });
    const pReload = reloading.reset(); // reload starts first
    const pPage = reloading.loadMore(); // append still on the old cursor
    check('append runs against the pre-reset cursor', early[1].cursor === '');
    early[0].resolve({ rows: [{ id: 'n' }], nextAfter: '' });
    check('reload committing first still applies',
      (await pReload) === 'applied' && get(reloading.rows).length === 1 && get(reloading.rows)[0].id === 'n');
    early[1].resolve({ rows: [{ id: 'o' }], nextAfter: 'old' });
    check('append landing after a committed reset lands stale',
      (await pPage) === 'stale' && get(reloading.rows).length === 1 && get(reloading.rows)[0].id === 'n' &&
      get(reloading.exhausted));
  }
  // A failed reset changes nothing: a pending append against the
  // still-valid cursor lands and fills the list instead of going
  // stale with it.
  {
    const recovering = [];
    const retrying = createPager({
      fetchPage: (cursor) => new Promise((resolve, reject) => recovering.push({ cursor, resolve, reject })),
      keyOf: (r) => r.id, cap: 10
    });
    const pPage = retrying.loadMore();
    const pReload = retrying.reset();
    recovering[1].reject(new Error('offline'));
    check('failed reset reports failure without rows',
      (await pReload) === 'failed' && get(retrying.rows).length === 0 && get(retrying.error) === 'offline');
    recovering[0].resolve({ rows: [{ id: 'o' }], nextAfter: 'old' });
    check('pending append survives a failed reset',
      (await pPage) === 'applied' && get(retrying.rows).length === 1 && get(retrying.rows)[0].id === 'o' &&
      !get(retrying.exhausted));
  }
  // Retry follows the displayed failure even when the last click
  // started the other operation.
  for (const resetFailsLast of [true, false]) {
    const pendingFailures = [];
    const overlapping = createPager({
      fetchPage: (cursor) => new Promise((resolve, reject) => pendingFailures.push({ cursor, reject })),
      keyOf: (row) => row.id,
      cap: 10,
      initial: { rows: [{ id: 'existing' }], nextAfter: 'next' }
    });
    const reload = overlapping.reset();
    const append = overlapping.loadMore();
    const [reloadRequest, appendRequest] = pendingFailures;
    if (resetFailsLast) {
      appendRequest.reject(new Error('page failed'));
      await append;
      reloadRequest.reject(new Error('reload failed'));
      await reload;
    } else {
      reloadRequest.reject(new Error('reload failed'));
      await reload;
      appendRequest.reject(new Error('page failed'));
      await append;
    }
    const message = resetFailsLast ? 'reload failed' : 'page failed';
    check(`visible failure identifies the operation when ${message} last`,
      reloadRequest.cursor === '' && appendRequest.cursor === 'next' &&
      get(overlapping.error) === message && get(overlapping.failure)?.reset === resetFailsLast);
  }
  // Snapshot seeding: seeded rows render with no fetch; a failed reset
  // keeps them with an error instead of emptying the list.
  const seeded = createPager({
    fetchPage: async () => { throw new Error('offline'); },
    keyOf: (r) => r.id, cap: 10,
    // Thunk form, as .svelte callers pass it (init-capture rule).
    initial: () => ({ rows: [{ id: 's' }], nextAfter: 'c9' })
  });
  check('seed renders without fetching', get(seeded.rows).length === 1 && get(seeded.started) === true);
  check('failed reset keeps the seed with an error',
    (await seeded.reset()) === 'failed' && get(seeded.rows).length === 1 && get(seeded.error) === 'offline');
}

// lib/format/briefingView: pure dossier derivations with unit coverage.
{
  const { priorityDiffers, lookupLimits, lookupReceipts, identityComplete } = await import('../src/lib/format/briefingView.js');
  check('equal values collapse the comparison',
    !priorityDiffers({ clamped: false, baseline: 'low', priority: 'low', model_suggestion: 'low' }));
  check('null suggestion falls back to baseline',
    !priorityDiffers({ clamped: false, baseline: 'low', priority: 'low', model_suggestion: null }));
  check('raise or clamp opens the comparison',
    priorityDiffers({ clamped: false, baseline: 'low', priority: 'medium', model_suggestion: 'medium' }) &&
    priorityDiffers({ clamped: true, baseline: 'low', priority: 'low', model_suggestion: 'low' }));
  check('null briefing collapses', !priorityDiffers(null));
  const traces = [
    { step: 0, summary: 'history', outcome: 'ok' },
    { step: 1, summary: 'history', outcome: 'ok' },
    { step: 2, summary: 'other', outcome: 'unknown_subject' }
  ];
  check('only failing lookups limit context',
    lookupLimits(traces).length === 1 && lookupLimits(traces)[0].step === 2);
  const receipts = lookupReceipts(traces);
  check('receipts group by summary in first-seen order',
    receipts.length === 2 && receipts[0].steps.join(',') === '0,1' && receipts[1].key === 'other');
  check('complete dossier needs full retention',
    identityComplete({ observations_truncated: false, referenced_truncated: false }) &&
    !identityComplete({ observations_truncated: true, referenced_truncated: false }) &&
    !identityComplete(null));
}

// lib/decisions: submission payload shaping with unit coverage. The
// container posts only built payloads, so a bad snooze date surfaces
// as form text without touching the network.
{
  const { buildDecisionPayload } = await import('../src/lib/decisions/payload.js');
  const ctx = { subjectKind: 'briefing', subjectId: 'brf-1', requestId: 'req-1', memberScope: true };
  const note = buildDecisionPayload({ kind: 'note', text: 'look', includeMembers: true }, ctx);
  check('note carries text and membership',
    note.payload.text === 'look' && note.payload.includeMembers === true &&
    note.payload.requestId === 'req-1' && note.error === undefined);
  const snooze = buildDecisionPayload({ kind: 'snooze', untilText: '2030-01-02T03:04' }, ctx);
  check('valid snooze converts to ISO',
    typeof snooze.payload.until === 'string' && !Number.isNaN(Date.parse(snooze.payload.until)));
  const bad = buildDecisionPayload({ kind: 'snooze', untilText: 'not a date' }, ctx);
  check('garbage snooze date never reaches the network',
    bad.error === 'snooze time is not a date' && bad.payload === undefined);
  const plain = buildDecisionPayload({ kind: 'expected' }, { ...ctx, memberScope: false });
  check('non-member scope omits membership',
    plain.payload.includeMembers === undefined && plain.payload.kind === 'expected');
}

// lib/format/evidence: the navigation key contract.
{
  const { eventKey, stepKey, REPORTS_KEY } = await import('../src/lib/format/evidence.js');
  check('evidence keys are explicit',
    eventKey('evt-1') === 'event:evt-1' && stepKey(2) === 'step:2' && REPORTS_KEY === 'reports');
}

// BUG-2 (fixed by the Briefing migration): the briefing resource
// resets when the id changes, so the previous briefing never renders
// while the next one fetches. Resetting is lifecycle, so the pin stays
// structural on the owning mechanism; selection switches stay on the
// manual checklist with the rest of the runtime behavior smoke cannot
// mount.
{
  const container = readFileSync(join(root, 'src/containers/BriefingContainer/BriefingContainer.svelte'), 'utf8');
  const effect = container.slice(container.indexOf('Reset the resource'));
  bugPin('BUG-2', 'id change resets the briefing resource before refetch',
    /createResource/.test(effect) && /invalidate/.test(effect));
}

// Reconnect edge and failure visibility, behaviorally. BUG-3's fix is
// the edge decision plus the snapshot-aware notice — both pure units,
// so no source regex is needed. Container wiring (calling refresh on a
// true edge) stays on the manual reconnect checklist with the rest of
// the runtime behavior smoke cannot mount.
{
  const { shouldRefreshOnReconnect } = await import('../src/lib/events/reconnect.js');
  const edge = (over) => shouldRefreshOnReconnect({ live: false, wasLive: false, data: null, initial: null, ...over });
  check('snapshot plus reconnect edge refreshes',
    edge({ live: true, initial: 'snap' }) && edge({ live: true, data: 'fetched' }));
  check('no edge without liveness change or content',
    !edge({ live: true }) && !edge({ live: true, wasLive: true, initial: 'snap' }) &&
    !edge({ live: false, initial: 'snap' }) && !edge({ live: true, data: null, initial: null }));
  const { failureNotice } = await import('../src/lib/async/resource.js');
  const failed = (over) => failureNotice({ data: null, initial: null, error: null, refreshError: null, ...over });
  check('snapshot plus failing fetch yields a visible notice',
    failed({ initial: 'snap', error: 'offline' }) === 'offline');
  check('fetched data plus failing fetch yields a visible notice',
    failed({ data: 'rows', refreshError: 'offline' }) === 'offline');
  check('data-less failure is an error, not a notice',
    failed({ error: 'offline' }) === null);
  check('quiet state yields no notice',
    failed({ data: 'rows' }) === null && failed({}) === null);
}

// lib/async: per-operation guards never cancel each other, resources
// keep data across failed refreshes, pagers merge by key under a cap.
{
  const { latestOnly } = await import('../src/lib/async/latestOnly.js');
  const reloadOp = latestOnly();
  const pageOp = latestOnly();
  const reloadToken = reloadOp.start();
  pageOp.start();
  check('per-operation guards are independent', reloadOp.current(reloadToken));
  reloadOp.invalidate();
  check('invalidate voids only its operation', !reloadOp.current(reloadToken) && pageOp.current(pageOp.start()));
}
{
  const { get } = await import('svelte/store');
  const { createResource } = await import('../src/lib/async/resource.js');
  const deferred = () => {
    let resolve, reject;
    const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
    return { promise, resolve, reject };
  };
  // Stale refresh loses to a newer one.
  const slow = deferred();
  const r1 = createResource(() => slow.promise);
  const p1 = r1.refresh();
  const p2 = r1.refresh();
  slow.resolve('second');
  check('stale refresh yields', (await p1) === 'stale' && (await p2) === 'applied' && get(r1.data) === 'second');
  // Failure without data is an error; with data it is a notice.
  const r2 = createResource(() => Promise.reject(new Error('offline')));
  check('data-less failure is an error', (await r2.refresh()) === 'failed' && get(r2.error) === 'offline');
  let calls = 0;
  const r3 = createResource(() => ((calls += 1) === 1 ? Promise.resolve('first') : Promise.reject(new Error('offline'))));
  check('first load applies', (await r3.refresh()) === 'applied');
  check('failed refresh keeps data with notice', (await r3.refresh()) === 'failed' &&
    get(r3.data) === 'first' && get(r3.refreshError) === 'offline' && get(r3.error) === null);
  // Invalidate voids in flight without clearing rendered data.
  let answer = () => Promise.resolve('seeded');
  const r4 = createResource(() => answer());
  check('seed applies', (await r4.refresh()) === 'applied');
  const gate = deferred();
  answer = () => gate.promise;
  const pending = r4.refresh();
  r4.invalidate();
  gate.resolve('late');
  check('invalidated refresh keeps rendered data',
    (await pending) === 'stale' && get(r4.data) === 'seeded' &&
    get(r4.error) === null && get(r4.refreshError) === null);
}
{
  const { get } = await import('svelte/store');
  const { createPager } = await import('../src/lib/async/pager.js');
  const pages = {
    '': { rows: [{ id: 'a' }, { id: 'b' }], nextAfter: 'c1' },
    c1: { rows: [{ id: 'b' }, { id: 'c' }], nextAfter: '' }
  };
  const pager = createPager({ fetchPage: async (c) => pages[c], keyOf: (r) => r.id, cap: 10 });
  check('first page applies', (await pager.loadMore()) === 'applied' && get(pager.rows).length === 2);
  check('second page dedupes and exhausts',
    (await pager.loadMore()) === 'applied' && get(pager.rows).length === 3 && get(pager.exhausted));
  check('reset replaces rows', (await pager.reset()) === 'applied' && get(pager.rows).length === 2);
  const failing = createPager({
    fetchPage: async () => { throw new Error('offline'); }, keyOf: (r) => r.id, cap: 10
  });
  check('failed page keeps rows empty with error',
    (await failing.loadMore()) === 'failed' && get(failing.rows).length === 0 && get(failing.error) === 'offline');
  const capped = createPager({
    fetchPage: async () => ({ rows: [{ id: 'x' }], nextAfter: 'more' }),
    keyOf: (r) => r.id, cap: 1
  });
  await capped.loadMore();
  check('cap flag names the limit', get(capped.capped) === true);
  // Replace restarts from applied data: in-flight pages land stale.
  const gated = (() => { let r; const p = new Promise((res) => { r = res; }); return { promise: p, resolve: r }; })();
  const swapping = createPager({ fetchPage: () => gated.promise, keyOf: (r) => r.id, cap: 10 });
  const late = swapping.loadMore();
  swapping.replace([{ id: 'fresh' }], '');
  gated.resolve({ rows: [{ id: 'old' }], nextAfter: 'c' });
  check('replace wins over an in-flight page',
    (await late) === 'stale' && get(swapping.rows).length === 1 && get(swapping.rows)[0].id === 'fresh' &&
    get(swapping.exhausted) === true && get(swapping.error) === null);
}

// lib/events: transport lifecycle with an injectable source. Errors
// never close the source: the browser's native retry carries
// Last-Event-ID so the server resumes bounded replay.
{
  const { get } = await import('svelte/store');
  const { createEventStream, INCIDENT_KINDS } = await import('../src/lib/events/stream.js');
  class FakeSource {
    constructor() { this.handlers = {}; this.closed = false; this.onopen = null; this.onerror = null; this.readyState = 0; }
    addEventListener(kind, fn) { ((this.handlers[kind] ??= [])).push(fn); }
    removeEventListener(kind, fn) { this.handlers[kind] = (this.handlers[kind] ?? []).filter((h) => h !== fn); }
    emit(kind, data) { for (const fn of this.handlers[kind] ?? []) { fn(data === undefined ? {} : { data }); } }
    close() { this.closed = true; }
  }
  const sources = [];
  const scheduled = [];
  const seen = { overviews: [], incidents: [] };
  const stream = createEventStream({
    url: '/events',
    createSource: (url) => { const s = new FakeSource(); s.url = url; sources.push(s); return s; },
    schedule: (fn, ms) => { scheduled.push({ fn, ms }); return scheduled.length; },
    cancelSchedule: (t) => { scheduled[t - 1].cancelled = true; },
    onOverview: (o) => seen.overviews.push(o),
    onIncident: (k) => seen.incidents.push(k)
  });
  stream.connect();
  check('stream opens connecting on the right url', get(stream.connection) === 'connecting' && sources[0].url === '/events');
  sources[0].onopen();
  check('open means live', get(stream.connection) === 'live');
  sources[0].emit('overview', '{"verdict":"quiet"}');
  sources[0].emit('overview', 'not json{{{');
  check('valid frame lands, malformed degrades silently',
    seen.overviews.length === 1 && seen.overviews[0].verdict === 'quiet' && get(stream.connection) === 'live');
  check('incident kinds are open/incident/resync', INCIDENT_KINDS.join(',') === 'open,incident,resync');
  for (const kind of INCIDENT_KINDS) { sources[0].emit(kind); }
  check('every incident kind bumps the revision once',
    get(stream.incidentRevision) === 3 && seen.incidents.join(',') === 'open,incident,resync');
  sources[0].onerror();
  check('mid-stream error marks retrying without closing the source',
    !sources[0].closed && get(stream.connection) === 'retrying' && sources.length === 1 && scheduled.length === 0);
  sources[0].onopen();
  check('native retry on the same source resumes live',
    get(stream.connection) === 'live' && sources.length === 1 && !sources[0].closed);
  sources[0].readyState = 2; // rejected handshake (e.g. the 503 subscriber cap)
  sources[0].onerror();
  check('closed source reconnects explicitly after the specified delay',
    sources[0].closed && get(stream.connection) === 'retrying' && scheduled.length === 1 && scheduled[0].ms === 3000);
  scheduled[0].fn();
  check('explicit retry opens a fresh source', sources.length === 2 && !sources[1].closed);
  sources[1].readyState = 2;
  sources[1].onerror();
  stream.disconnect();
  scheduled[1].fn();
  check('disconnect voids the pending retry', sources.length === 2 && get(stream.connection) === 'retrying');
}

// lib/router: table parity with App's inline parser.
{
  const { parseRoute, routeTitle } = await import('../src/lib/router/routes.js');
  check('briefing route decodes id', JSON.stringify(parseRoute('#/briefing/brf%201')) === JSON.stringify({ name: 'briefing', id: 'brf 1' }));
  check('incident route', parseRoute('#/incidents/inc-1').name === 'incident');
  check('device route', parseRoute('#/devices/prof-1').id === 'prof-1');
  check('devices accepts trailing slash', parseRoute('#/devices/').name === 'devices' && parseRoute('#/devices').name === 'devices');
  check('health route', parseRoute('#/health').name === 'health');
  check('empty and unknown fall home',
    parseRoute('').name === 'home' && parseRoute(null).name === 'home' && parseRoute('#/nope').name === 'home');
  check('titles cover every route',
    ['home', 'devices', 'device', 'briefing', 'incident', 'health'].every((n) => routeTitle(n) !== 'Review overview' || n === 'home') &&
    routeTitle('briefing') === 'Briefing' && routeTitle('mystery') === 'Review overview');
}

// lib/shortcuts: scoped dispatch with typing/modifier silence, help from one table.
{
  const { createShortcutRegistry } = await import('../src/lib/shortcuts/registry.js');
  const reg = createShortcutRegistry();
  const handled = [];
  reg.register('global', 'j', 'Move between rows', () => handled.push('j'));
  reg.register('global', '?', 'Toggle shortcuts', () => handled.push('?'));
  reg.register('briefing', 'e', 'Preset expected', () => handled.push('e'));
  const key = (k, extra = {}) => ({ key: k, target: { tagName: 'BUTTON' }, ...extra });
  check('registered keys dispatch', reg.dispatch('global', key('j')) && reg.dispatch('briefing', key('e')) && handled.join(',') === 'j,e');
  check('scopes stay separate', !reg.dispatch('global', key('e')) && !reg.dispatch('briefing', key('j')));
  check('typing and modifiers stay silent',
    !reg.dispatch('global', { key: 'j', target: { tagName: 'INPUT' } }) &&
    !reg.dispatch('global', key('j', { metaKey: true })) &&
    !reg.dispatch('global', key('j', { ctrlKey: true })) &&
    !reg.dispatch('global', key('z')));
  const help = reg.describe('global');
  check('help derives from bindings',
    help.length === 2 && help.some((h) => h.key === '?' && h.label === 'Toggle shortcuts'));
}

console.log(`characterize: ${pass} checks passed`);
if (bugs.length > 0) {
  for (const b of bugs) {
    console.log(`KNOWN BUG (failing pin): ${b}`);
  }
  process.exitCode = 1;
}
