// Step 23 smoke: server-side renders Home against fixture envelopes
// and asserts the wire copy lands on the page. No browser needed —
// Svelte's server renderer exercises the components, the stream and
// fetch paths stay in App (covered by lint/build, verified by hand).
import { readdirSync, readFileSync, writeFileSync, mkdirSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL, fileURLToPath } from 'node:url';
import { compile } from 'svelte/compiler';
import { render } from 'svelte/server';

const root = fileURLToPath(new URL('..', import.meta.url));
const lib = join(root, 'src', 'lib');
// Inside the project so bare 'svelte' imports resolve to ui/node_modules.
const out = join(root, '.smoke-tmp');
rmSync(out, { recursive: true, force: true });
mkdirSync(out, { recursive: true });

for (const name of readdirSync(lib)) {
  if (!name.endsWith('.svelte')) {
    continue;
  }
  const source = readFileSync(join(lib, name), 'utf8');
  const { js } = compile(source, { generate: 'server', filename: name });
  // Compiled output keeps the .svelte specifier; rewrite to the compiled sibling.
  const code = js.code
    .replaceAll(/from '(\.\/[^']+)\.svelte'/g, "from '$1.js'")
    .replaceAll("from '../api.js'", "from './api.js'");
  writeFileSync(join(out, name.replace(/\.svelte$/, '.js')), code);
}
// Plain JS siblings the screens import at runtime, copied next to the
// compiled output so bare relative specifiers resolve inside .smoke-tmp.
for (const [from, to] of [
  [join(root, 'src', 'api.js'), join(out, 'api.js')],
  [join(lib, 'freshness.js'), join(out, 'freshness.js')],
  [join(lib, 'format.js'), join(out, 'format.js')],
  [join(lib, 'roving.js'), join(out, 'roving.js')],
  [join(lib, 'paging.js'), join(out, 'paging.js')],
  [join(lib, 'purge.js'), join(out, 'purge.js')]
]) {
  writeFileSync(to, readFileSync(from, 'utf8'));
}

// The specifier is built from constants (own temp dir, fixed name),
// never from network or user input.
// eslint-disable-next-line no-unsanitized/method
const { default: Home } = await import(pathToFileURL(join(out, 'Home.js')).href);
// eslint-disable-next-line no-unsanitized/method
const { default: Detail } = await import(pathToFileURL(join(out, 'Detail.js')).href);
// eslint-disable-next-line no-unsanitized/method
const { default: Devices } = await import(pathToFileURL(join(out, 'Devices.js')).href);
// eslint-disable-next-line no-unsanitized/method
const { default: Device } = await import(pathToFileURL(join(out, 'Device.js')).href);
// eslint-disable-next-line no-unsanitized/method
const { default: Health } = await import(pathToFileURL(join(out, 'Health.js')).href);

const zeroDigest = {
  weak_appearance: 0,
  weak_disappearance: 0,
  port_closed: 0,
  hostname_change: 0,
  os_refinement: 0,
  other: 0
};

function base(over = {}) {
  return {
    verdict: 'quiet',
    headline: 'Nothing needs review',
    attention: [],
    digest: { ...zeroDigest },
    receipts: 0,
    window: '24h0m0s',
    coverage: 'ok',
    liveness: 'unknown',
    sources: [],
    reasons: [],
    ...over
  };
}

const attentionItem = {
  id: 'brf-grp-1',
  kind: 'briefing',
  priority: 'high',
  change_kind: 'port_closed',
  created_at: '2026-11-01T00:00:00Z',
  status: 'attention'
};

const hostile = `<img src=x onerror=alert(1)>`;
const hostileAttr = { [`key${hostile}`]: `<b>${hostile}</b>` };

const detailFixture = {
  briefing_id: 'brf-grp-1',
  group_id: 'grp-1',
  host: 'mac:aa:bb:cc:dd:ee:ff',
  baseline: 'low',
  model_suggestion: 'medium',
  priority: 'medium',
  priority_reason: `Escalated because ${hostile}`,
  baseline_rule: 'LOW_NEW_HOST',
  kind: 'port_closed',
  source: 'model',
  clamped: true,
  fallback_used: false,
  what_changed: `Port 22 closed ${hostile}`,
  context: 'New host with one closed port.',
  recommended_check: 'Verify the host with your asset list.',
  created_at: '2026-11-01T00:00:00Z',
  event_ids: ['evt-1', 'evt-2'],
  seq: 7,
  profiles: [{ profile_id: 'prof-0000000000000001', role: 'current' }],
  profiles_truncated: true,
  referenced_events: [
    { event_id: 'evt-1', event_type: 'host.appeared' },
    { event_id: 'evt-2', event_type: 'host.changed' }
  ],
  observations: [
    {
      event_id: 'evt-2',
      view_role: 'current',
      reported_at: '2026-11-01T00:00:00Z',
      received_at: '2026-11-01T00:01:00Z',
      attributes: hostileAttr,
      ports: [{ proto: 'tcp', port: 22, state: 'closed', service: '' }],
      ports_total: 600,
      ports_truncated: true
    }
  ],
  observations_truncated: false
};

const legacyDevice = {
  profile_id: 'prof-0000000000000002',
  class: 'legacy',
  source_scope: 'lab',
  namespace: '',
  reported_id: 'mac:aa:bb:cc:dd:ee:ff',
  reported_source: 'subnetlens-pro',
  reported_confidence: 'high',
  association_status: 'active',
  associations: {},
  operator_label: `My printer ${hostile}`,
  first_report_at: '2026-10-01T00:00:00Z',
  last_report_at: '2026-11-01T00:00:00Z',
  latest: null,
  history: [
    {
      observation_id: 3,
      event_id: 'evt-9',
      view_role: 'host',
      reported_at: '2026-11-01T00:00:00Z',
      received_at: '2026-11-01T00:01:00Z',
      attributes: { hostname: `evil${hostile}` },
      ports_total: 0,
      ports_truncated: false
    }
  ],
  history_next_after: ''
};

const healthFixture = {
  sources: [
    {
      scope: 'lab',
      status: 'degraded',
      liveness: 'unknown',
      reasons: ['reported delivery gap'],
      heartbeat_enabled: false
    }
  ],
  coverage: 'degraded',
  coverage_reasons: ['reported delivery gap'],
  fallback_1h: { used: 1, total: 4, ratio: 0.25 },
  parked_groups: 1,
  forward_errors_1h: 2,
  purge: {
    totals: { raw_events: 10 },
    last: { status: 'failed', started_at: '2026-11-01T00:00:00Z', rows_deleted: 5, error: 'disk I/O' },
    last_success: { status: 'completed', started_at: '2026-10-31T00:00:00Z', rows_deleted: 9 }
  },
  retention: { reports_seconds: 2592000, subjects_seconds: 7776000 },
  process: {
    started_at: '2026-11-01T00:00:00Z',
    ingest_accepted: 100,
    ingest_duplicates: 2,
    ingest_rejected: 1,
    ingest_saturated: 0,
    provider_calls: 5,
    provider_fallbacks: 1,
    redaction_blocked: 0,
    writer_saturated: 0
  },
  queue_depth: 0,
  queue_cap: 64
};

const cases = [
  {
    component: Detail,
    name: 'detail full with hostile text',
    props: { id: 'brf-grp-1', initial: detailFixture },
    expect: [
      'Why this priority',
      'Baseline floor',
      'clamped to one step above baseline',
      'Referenced events',
      'evt-1 — host.appeared',
      'Reported evidence',
      'Retained reported context',
      'prof-0000000000000001',
      'More profile links exist',
      'Recommended check',
      'No agent trace is available',
      'Provenance',
      'Snapshot covers 600 ports (truncated)'
    ],
    absent: ['<img', '<b>']
  },
  {
    component: Devices,
    name: 'devices list',
    props: {
      stream: 'live',
      initial: {
        devices: [
          { ...legacyDevice, history: undefined },
          {
            profile_id: 'prof-0000000000000003',
            class: 'address',
            source_scope: 'lab',
            last_report_at: '2026-11-01T00:00:00Z'
          }
        ],
        next_after: ''
      }
    },
    expect: ['Reported hosts', 'Legacy profile', 'Address context', 'End of retained list'],
    absent: ['<img']
  },
  {
    component: Device,
    name: 'legacy device with hostile label',
    props: { id: legacyDevice.profile_id, stream: 'live', initial: legacyDevice },
    expect: [
      'Earlier bridge-inferred association; not used for current identity',
      'Your label',
      'Latest evidence',
      'No retained observation',
      'evt-9 (host)',
      '0 ports',
      'End of retained history',
      'Health'
    ],
    absent: ['<img']
  },
  {
    component: Health,
    name: 'health with purge failure',
    props: { stream: 'live', initial: healthFixture },
    expect: [
      'Coverage: degraded',
      'Stuck groups awaiting operator: 1',
      'Latest purge run failed',
      'disk I/O',
      'Latest fully successful purge',
      'Configured retention: 30 days for reports, 90 days for subjects',
      'unknown — Step 21 has not reported one',
      'Heartbeat',
      'disabled',
      'Ingest accepted'
    ]
  },
  {
    name: 'quiet empty',
    props: { overview: base(), loadError: null },
    expect: [
      'Nothing needs review',
      'No receipts in the last 24h0m0s',
      'quiet here means no data',
      'Coverage: ok',
      'Liveness: unknown',
      'No sources registered yet'
    ]
  },
  {
    name: 'quiet with receipts',
    props: { overview: base({ receipts: 5, last_change: '2026-11-01T00:00:00Z' }), loadError: null },
    expect: ['Nothing needs review', '5 receipts']
  },
  {
    name: 'review',
    props: {
      overview: base({
        verdict: 'review',
        headline: '1 things to review',
        receipts: 1,
        attention: [{ ...attentionItem, priority: 'medium' }]
      }),
      loadError: null
    },
    expect: ['1 things to review', 'brf-grp-1', 'Priority: medium']
  },
  {
    name: 'act with degraded coverage',
    props: {
      overview: base({
        verdict: 'act',
        headline: '1 need a check now',
        receipts: 2,
        attention: [attentionItem],
        coverage: 'degraded',
        liveness: 'unknown',
        reasons: ['fallback ratio high'],
        digest: { ...zeroDigest, port_closed: 2 },
        sources: [
          {
            scope: 'lab',
            status: 'ok',
            liveness: 'unknown',
            reasons: [],
            last_report_at: '2026-11-01T00:00:00Z'
          }
        ]
      }),
      loadError: null
    },
    expect: [
      '1 need a check now',
      'port_closed',
      'fallback ratio high',
      'Coverage is degraded',
      'lab',
      'Status: ok',
      'Liveness: unknown'
    ]
  },
  {
    name: 'load error',
    props: { overview: null, loadError: 'overview 500' },
    expect: ['Overview unavailable', 'overview 500']
  },
  {
    name: 'stale error keeps overview',
    props: { overview: base({ verdict: 'review', headline: '1 things to review', receipts: 1 }), loadError: 'overview 500' },
    expect: ['1 things to review'],
    absent: ['Overview unavailable']
  }
];

let failed = 0;
for (const c of cases) {
  const { body } = render(c.component ?? Home, { props: c.props });
  // Strip SSR hydration markers, which split copy across expressions.
  const stripped = body.replaceAll(/<!--.*?-->/g, '');
  // Server text is HTML-escaped; unescape the entities our copy uses.
  const text = stripped
    .replaceAll('&quot;', '"')
    .replaceAll('&#39;', "'")
    .replaceAll('&amp;', '&')
    .replaceAll('&lt;', '<')
    .replaceAll('&gt;', '>');
  // Absence checks run on entity-escaped text: escaped hostile input
  // (&lt;img) must never read as a literal tag.
  for (const s of c.expect) {
    if (!text.includes(s)) {
      console.error(`[${c.name}] missing ${JSON.stringify(s)}`);
      failed++;
    }
  }
  for (const s of c.absent ?? []) {
    if (stripped.includes(s)) {
      console.error(`[${c.name}] must not contain ${JSON.stringify(s)}`);
      failed++;
    }
  }
  // Hostile strings would render escaped; our copy never injects HTML.
  if (/<script/.test(body)) {
    console.error(`[${c.name}] unexpected script element`);
    failed++;
  }
}
rmSync(out, { recursive: true, force: true });
if (failed > 0) {
  console.error(`${failed} smoke assertion(s) failed`);
  process.exit(1);
}
console.log(`smoke: ${cases.length} states render`);

// Fetch/stream freshness: a stale fetch completion must not overwrite
// newer state, and a stream frame must invalidate fetches started
// before it.
{
  const { createFetchGuard } = await import('../src/lib/freshness.js');
  const check = (name, cond) => {
    if (!cond) {
      console.error(`[freshness] ${name}`);
      failed++;
    }
  };
  const g = createFetchGuard();
  const first = g.start();
  const second = g.start();
  check('older fetch dropped', !g.current(first));
  check('newest fetch current', g.current(second));
  g.streamed();
  check('stream invalidates in-flight fetch', !g.current(second));
  const third = g.start();
  check('fetch after frame current', g.current(third));
  const h = createFetchGuard();
  const only = h.start();
  check('clean completion current', h.current(only));
  h.invalidate();
  check('invalidate voids in-flight fetch', !h.current(only));
  const after = h.start();
  check('fetch after invalidate current', h.current(after));

  // Paging merges by key with first-seen positions: repeats across
  // mutable-order pages drop out, the cap binds the total.
  // eslint-disable-next-line no-unsanitized/method
  const { nextPageRows } = await import(pathToFileURL(join(lib, 'paging.js')).href);
  const key = (r) => r.id;
  const cur = [{ id: 'a' }, { id: 'b' }];
  const appended = nextPageRows(cur, [{ id: 'b' }, { id: 'c' }, { id: 'a' }], key, 10);
  check('repeats dropped, order kept', JSON.stringify(appended) === JSON.stringify([{ id: 'c' }]));
  const capped = nextPageRows(cur, [{ id: 'c' }, { id: 'd' }], key, 3);
  check('cap binds total', JSON.stringify(capped) === JSON.stringify([{ id: 'c' }]));

  // A completed purge run can still carry error text (skipped token
  // expiry): status alone never proves clean work.
  // eslint-disable-next-line no-unsanitized/method
  const { latestRunNeedsAttention } = await import(pathToFileURL(join(lib, 'purge.js')).href);
  check('failed run needs attention', latestRunNeedsAttention({ status: 'failed', error: 'x' }));
  check('completed-with-error needs attention', latestRunNeedsAttention({ status: 'completed', error: 'skipped token expiry' }));
  check('clean completed needs none', !latestRunNeedsAttention({ status: 'completed', error: '' }));
  check('absent run needs none', !latestRunNeedsAttention(null));
}
if (failed > 0) {
  console.error(`${failed} freshness assertion(s) failed`);
  process.exit(1);
}
console.log('smoke: freshness guard holds');
