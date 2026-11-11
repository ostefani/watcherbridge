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
const src = join(root, 'src');
// Inside the project so bare 'svelte' imports resolve to ui/node_modules.
const out = join(root, '.smoke-tmp');
rmSync(out, { recursive: true, force: true });
mkdirSync(out, { recursive: true });

// Stub for CSS Modules: SSR has no Vite, so `styles.x` resolves to the
// class key itself. Text/hostile-content assertions are unaffected. Only
// string keys resolve — symbol lookups (`then`, `toPrimitive`) stay
// undefined so the stub never impersonates a thenable or primitive.
writeFileSync(join(out, 'css-stub.js'),
  'export default new Proxy({}, { get: (_, prop) => (typeof prop === \'string\' ? prop : undefined) });\n');

/** Recursively collect .svelte files; basenames must stay unique. */
function collectSvelte(dir) {
  const found = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      found.push(...collectSvelte(full));
    } else if (entry.name.endsWith('.svelte')) {
      found.push(full);
    }
  }
  return found;
}

const seen = new Set();
for (const full of collectSvelte(src)) {
  const base = full.slice(full.lastIndexOf('/') + 1);
  if (seen.has(base)) {
    throw new Error(`smoke: duplicate component basename ${base}; basenames must stay unique`);
  }
  seen.add(base);
  const source = readFileSync(full, 'utf8');
  const { js } = compile(source, { generate: 'server', filename: base });
  // Compiled output keeps .svelte specifiers; `@/` aliases resolve only
  // under Vite, so map everything onto the flat copies below. Order
  // matters: the async rule precedes the generic lib rule.
  const code = js.code
    .replaceAll(/from '(\.\/[^']+)\.svelte'/g, "from '$1.js'")
    .replaceAll(/from '@\/(?:components|containers)\/[^/]+\/([^']+)\.svelte'/g, "from './$1.js'")
    .replaceAll("from '@/api.js'", "from './api.js'")
    .replaceAll(/from '@\/lib\/(?:[^']*\/)?([^'/]+)\.svelte'/g, "from './$1.js'")
    .replaceAll(/from '@\/lib\/(?:[^']*\/)?([^'/]+)'/g, "from './$1'")
    .replaceAll(/from '[^']*\.module\.css'/g, "from './css-stub.js'");
  writeFileSync(join(out, base.replace(/\.svelte$/, '.js')), code);
}
// Plain JS siblings the screens import at runtime. Every `src/lib`
// helper is copied flat (basenames share the uniqueness rule above),
// so a new helper import never needs a harness edit — it just lands.
const lib = join(src, 'lib');
// api.js lives beside lib/, not inside it.
writeFileSync(join(out, 'api.js'), readFileSync(join(src, 'api.js'), 'utf8'));
seen.add('api.js');
function collectJs(dir) {
  const found = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      found.push(...collectJs(full));
    } else if (entry.name.endsWith('.js')) {
      found.push(full);
    }
  }
  return found;
}
for (const full of collectJs(lib)) {
  const base = full.slice(full.lastIndexOf('/') + 1);
  // Barrel files are bundler ergonomics; SSR imports concrete modules.
  if (base === 'index.js') {
    continue;
  }
  if (seen.has(base)) {
    throw new Error(`smoke: duplicate helper basename ${base}; basenames must stay unique`);
  }
  seen.add(base);
  // Helpers import each other through `@/`, which plain node cannot
  // resolve here — map them onto the flat copies like the compiled
  // components above, so a new helper import just lands.
  const helperCode = readFileSync(full, 'utf8')
    .replaceAll("from '@/api.js'", "from './api.js'")
    .replaceAll(/from '@\/lib\/(?:[^']*\/)?([^'/]+)'/g, "from './$1'");
  writeFileSync(join(out, base), helperCode);
}
// NOTE: the flat `.smoke-tmp` layout is a permanent constraint — every
// compiled component and helper shares one directory, so basenames must
// stay unique across `src/` forever, not just during the migration.

// The specifier is built from constants (own temp dir, fixed name),
// never from network or user input.
// eslint-disable-next-line no-unsanitized/method
const { default: HomeContainer } = await import(pathToFileURL(join(out, 'HomeContainer.js')).href);
// eslint-disable-next-line no-unsanitized/method
const { default: BriefingContainer } = await import(pathToFileURL(join(out, 'BriefingContainer.js')).href);
// eslint-disable-next-line no-unsanitized/method
const { default: DevicesContainer } = await import(pathToFileURL(join(out, 'DevicesContainer.js')).href);
// eslint-disable-next-line no-unsanitized/method
const { default: DeviceContainer } = await import(pathToFileURL(join(out, 'DeviceContainer.js')).href);
// eslint-disable-next-line no-unsanitized/method
const { default: HealthContainer } = await import(pathToFileURL(join(out, 'HealthContainer.js')).href);
// eslint-disable-next-line no-unsanitized/method
const { default: IncidentContainer } = await import(pathToFileURL(join(out, 'IncidentContainer.js')).href);

// eslint-disable-next-line no-unsanitized/method
const { default: AttentionList } = await import(pathToFileURL(join(out, 'AttentionList.js')).href);

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
    explained: { ...zeroDigest },
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
  effective_review: { kind: 'expected', decision_id: 'dec-11' },
  decisions: [
    {
      decision_id: 'dec-12',
      subject_kind: 'briefing',
      subject_id: 'brf-grp-1',
      kind: 'note',
      text: `operator note ${hostile}`,
      created_at: '2026-11-01T01:00:00Z',
      undone: false
    },
    {
      decision_id: 'dec-11',
      subject_kind: 'briefing',
      subject_id: 'brf-grp-1',
      kind: 'expected',
      created_at: '2026-11-01T00:30:00Z',
      undone: false
    }
  ],
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

const traceFixture = {
  ...detailFixture,
  briefing_id: 'brf-agent-1',
  source: 'agent',
  likely_explanation: `Likely explanation ${hostile}`,
  confidence: 'high',
  // Validation permits repeated citations, so the cited-evidence
  // list must survive duplicates (client keyed-each keys are
  // occurrence-unique).
  evidence_refs: [{ event_id: 'evt-1' }, { event_id: 'evt-1' }, { tool_step: 0 }, { tool_step: 0 }],
  input_tokens: 1200,
  output_tokens: 300,
  cost_usd: 0.002,
  traces: [
    {
      step: 0,
      tool: 'fetch_event',
      args: `{"event_id":"evt-1"} ${hostile}`,
      result: '{"status":"ok"}',
      status: 'ok',
      duration_ms: 12
    }
  ]
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
  decisions: [
    {
      decision_id: 'dec-21',
      subject_kind: 'reported_subject',
      subject_id: 'prof-0000000000000002',
      kind: 'label',
      text: `My printer ${hostile}`,
      created_at: '2026-11-01T00:00:00Z',
      undone: false
    }
  ],
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

const incidentFixture = {
  incident: {
    incident_id: 'inc-11-22',
    kind: 'burst_disappearance',
    source_scope: 'lab',
    target: 'lan-a',
    status: 'open',
    floor: 'medium',
    qualified_refs: 2,
    member_count: 3,
    window_start: '2026-11-01T00:00:00Z',
    last_member_at: '2026-11-01T00:02:00Z',
    opened_at: '2026-11-01T00:03:00Z',
    updated_at: '2026-11-01T00:03:00Z',
    cursor: '1:2'
  },
  members: [
    {
      briefing_id: 'brf-grp-1',
      created_at: '2026-11-01T00:00:00Z',
      priority: 'low',
      refs: [{ reference: 'mac:aa', namespace: 'ns-1', revision: `r1 ${hostile}`, confidence: 'high' }]
    },
    {
      briefing_id: 'brf-grp-2',
      created_at: '2026-11-01T00:01:00Z',
      priority: 'low',
      refs: [{ reference: 'mac:bb', namespace: 'ns-1', revision: 'r1', confidence: 'medium' }]
    },
    {
      briefing_id: 'brf-grp-3',
      created_at: '2026-11-01T00:02:00Z',
      priority: 'low',
      refs: [{ reference: 'mac:aa', namespace: 'ns-1', revision: 'r1', confidence: 'high' }]
    }
  ],
  truncated: false,
  decisions: [
    {
      decision_id: 'dec-31',
      subject_kind: 'incident',
      subject_id: 'inc-11-22',
      kind: 'note',
      text: `burst reviewed ${hostile}`,
      created_at: '2026-11-01T00:04:00Z',
      undone: false
    }
  ],
  effective_review: { kind: 'expected', decision_id: 'dec-32' }
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
    component: BriefingContainer,
    name: 'detail full with hostile text',
    props: { id: 'brf-grp-1', initial: detailFixture },
    expect: [
      'Why this priority',
      'Baseline floor',
      'Suggestion adjusted to respect the baseline floor and one-step raise limit.',
      'Scanner snapshots',
      'evt-1',
      'host.appeared',
      'Analysis and retained context',
      'prof-0000000000000001',
      'Open reported context',
      'More profile links exist',
      'Recommended check',
      'Your decision',
      'Status: Reviewed: expected',
      'Record decision',
      'Choose a verdict to record it.',
      'operator note',
      'Undo',
      'Show older decisions',
      'No agent trace is available',
      'Technical details',
      'Provenance',
      'Snapshot covers 600 ports (truncated)'
    ],
    absent: ['<img', '<b>']
  },
  {
    component: BriefingContainer,
    name: 'detail with agent trace',
    props: { id: 'brf-agent-1', initial: traceFixture },
    expect: [
      'Possible explanation',
      'Explanation confidence (model self-assessment): high',
      '1200 input tokens',
      '300 output tokens',
      'Event evt-1',
      'Tool step 0',
      'fetch_event',
      'step 0 · ok · 12 ms',
      'Cited evidence',
      'Executed tool steps'
    ],
    absent: ['<img', '<b>', 'No agent trace is available'],
    // Both occurrences of each repeated citation must render.
    expectCount: [
      ['Event evt-1', 2],
      ['Tool step 0', 2]
    ]
  },
  {
    component: DevicesContainer,
    name: 'devices list',
    props: {
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
    component: DeviceContainer,
    name: 'legacy device with hostile label',
    props: { id: legacyDevice.profile_id, initial: legacyDevice },
    expect: [
      'Earlier bridge-inferred association; not used for current identity',
      'Your label',
      'Operator decisions',
      'Status: No operator review',
      'Record decision',
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
    component: HealthContainer,
    name: 'health with purge failure',
    props: { initial: healthFixture },
    expect: [
      'data-state="degraded"',
      'Awaiting operator</dt><dd>1',
      'Latest purge run failed',
      'disk I/O',
      'Latest fully successful purge',
      'Configured retention: 30 days for reports, 90 days for subjects',
      'unknown — stable identity contract unavailable',
      'Heartbeat',
      'disabled',
      'Ingest accepted'
    ]
  },
  {
    component: HealthContainer,
    name: 'health with partial heartbeat',
    props: { initial: {
      ...healthFixture,
      sources: [{
        scope: 'lab', status: 'degraded', liveness: 'reporting', heartbeat_enabled: true,
        heartbeat: { completed_at: '2026-11-03T12:00:00Z', status: 'partial', issue_count: 2,
          duration_ms: 1200, hosts_seen: 4, clock_skew_seconds: -2 },
        reasons: ['scanner reported partial scan']
      }]
    } },
    expect: ['Last completed scan', 'partial, 2 issues, 1200 ms', 'Reported host records',
      'not a current inventory or device count', 'Signing clock offset', 'heartbeat is not identity proof']
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
    expect: ['1 things to review', 'brf-grp-1', 'data-priority="medium"']
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
        explained: { ...zeroDigest, port_closed: 1 },
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
      'Ports closed',
      'expected 1',
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
    component: IncidentContainer,
    name: 'incident detail with shared evidence',
    props: { id: 'inc-11-22', initial: incidentFixture },
    expect: [
      'Burst disappearance',
      'open',
      'Floor: medium',
      'Source:',
      'Target: lan-a',
      '3 member reports · 2 qualified upstream references',
      'Some members share identity evidence',
      'brf-grp-1',
      'brf-grp-2',
      'brf-grp-3',
      '1 qualified reference',
      'Operator decisions',
      'Status: Reviewed: expected',
      'Also apply to the 3 member briefings',
      'Record decision',
      'burst reviewed'
    ],
    absent: ['<img', 'More members exist than shown']
  },
  {
    component: IncidentContainer,
    name: 'incident closed and truncated',
    props: {
      id: 'inc-11-22',
      initial: {
        ...incidentFixture,
        incident: { ...incidentFixture.incident, status: 'closed', closed_at: '2026-11-01T01:00:00Z' },
        truncated: true
      }
    },
    expect: ['closed', 'Closed', 'More members exist than shown'],
    absent: ['Open since']
  },
  {
    name: 'review with incident row',
    props: {
      overview: base({
        verdict: 'review',
        headline: '1 things to review',
        receipts: 1,
        attention: [{
          id: 'inc-11-22',
          kind: 'incident',
          priority: 'medium',
          change_kind: 'burst_disappearance',
          created_at: '2026-11-01T00:03:00Z',
          status: 'attention'
        }]
      }),
      loadError: null
    },
    expect: ['1 things to review', 'inc-11-22', 'class="attention-row"', 'Show members']
  },
  {
    name: 'stale error keeps overview',
    props: { overview: base({ verdict: 'review', headline: '1 things to review', receipts: 1 }), loadError: 'overview 500' },
    expect: ['1 things to review'],
    absent: ['Overview unavailable']
  }
];

let failed = 0;
const confidenceViews = (before, after, currentEvent = 'evt-1') => [
  { event_id: 'evt-1', view_role: 'previous', attributes: { identity_confidence: before } },
  { event_id: currentEvent, view_role: 'current', attributes: { identity_confidence: after } }
];
const confidenceTitle = 'Reported identity confidence decreased';
const { reviewTitle } = await import('../src/lib/format.js');
for (const [name, views, expected] of [
  ['confidence decrease', confidenceViews('high', 'low'), confidenceTitle],
  ['confidence increase', confidenceViews('low', 'high'), 'Reported details changed'],
  ['unchanged confidence', confidenceViews('high', 'high'), 'Reported details changed'],
  ['unknown confidence', confidenceViews('high', 'unknown'), 'Reported details changed'],
  ['missing previous', confidenceViews('high', 'low').slice(1), 'Reported details changed'],
  ['different events', confidenceViews('high', 'low', 'evt-2'), 'Reported details changed']
]) {
  if (reviewTitle('other', views) !== expected) {
    throw new Error(`Incorrect review title: ${name}`);
  }
}
if (reviewTitle('port_closed', confidenceViews('high', 'low')) !== 'Ports closed') {
  throw new Error('Specific reported-change categories must retain their title');
}
cases.push(
  { component: BriefingContainer, name: 'appearance review uses reported event title', props: { id: 'brf-grp-1', initial: { ...detailFixture, kind: 'other', observations: [], referenced_events: [{ event_id: 'evt-1', event_type: 'host.appeared' }] } }, expect: ['New host reported</h2>'], absent: ['Other changes', confidenceTitle] },
  { component: AttentionList, name: 'unloaded other review has neutral heading', props: { items: [{ ...attentionItem, change_kind: 'other' }] }, expect: ['Reported details changed'], absent: ['Other changes', confidenceTitle] },
  { component: BriefingContainer, name: 'reported confidence decrease heading', props: { id: 'brf-grp-1', initial: { ...detailFixture, kind: 'other', observations: confidenceViews('high', 'low') } }, expect: [`${confidenceTitle}</h2>`] },
  { component: AttentionList, name: 'loaded review title in attention list', props: { items: [{ ...attentionItem, change_kind: 'other' }], titles: { [attentionItem.id]: confidenceTitle } }, expect: [confidenceTitle], absent: ['Other changes'] }
);
for (const [types, expected] of [
  [['host.appeared'], 'New host reported'],
  [['host.disappeared'], 'Host disappearance reported'],
  [['host.changed'], 'Reported host details changed'],
  [['host.appeared', 'host.changed'], 'Reported network changes'],
  [['unexpected'], 'Reported details changed']
]) {
  if (reviewTitle('other', [], types.map((event_type) => ({ event_type }))) !== expected) {
    throw new Error('Review title must follow reported event types');
  }
}
cases.push(
  { component: AttentionList, name: 'standalone review navigation', props: { items: [attentionItem] }, expect: ['href="#/briefing/brf-grp-1"', 'Ports closed'] },
  { component: AttentionList, name: 'standalone incident navigation', props: { items: [{ ...attentionItem, id: 'inc-11-22', kind: 'incident' }] }, expect: ['href="#/incidents/inc-11-22"', 'Show members'] },
  { component: AttentionList, name: 'selected review button', props: { items: [attentionItem], selectedId: attentionItem.id, onselect: () => {} }, expect: ['<button', 'aria-current="true"', 'data-row'], absent: ['href="#/briefing/'] },
  { component: BriefingContainer, name: 'embedded briefing keeps evidence and decisions', props: { id: 'brf-grp-1', initial: detailFixture, layout: 'embedded' }, expect: ['Saved scanner reports', 'Record decision', 'Recommended check'], absent: ['Back to Home', '<img'] },
  { component: BriefingContainer, name: 'null model suggestion stands on baseline', props: { id: 'brf-grp-1', initial: { ...detailFixture, model_suggestion: null } }, expect: ['No model suggestion — baseline stands'] },
  { component: BriefingContainer, name: 'unlinked observation renders without a guessed link', props: { id: 'brf-grp-1', initial: { ...detailFixture, referenced_events: [{ event_id: 'evt-9', event_type: 'host.appeared' }], observations: [{ event_id: 'evt-9', view_role: 'host', reported_at: '2026-11-01T00:00:00Z', received_at: '2026-11-01T00:01:00Z', attributes: { hostname: 'lonely' }, ports_total: 0, ports_truncated: false }] } }, expect: ['No linked profile is available'] },
  { component: BriefingContainer, name: 'server-classified unknown subject renders honestly', props: { id: 'brf-agent-1', initial: { ...traceFixture, traces: [{ step: 0, tool: 'get_reported_history', args: '{"host":"HOST_1"}', result: '{"tool":"get_reported_history","result":{"error":"unknown subject"}}', status: 'ok', duration_ms: 3, outcome: 'unknown_subject', summary: 'The investigator could not resolve this lookup to a permitted reported subject; history remains unknown.' }] } }, expect: ['unknown subject', 'The investigator could not resolve this lookup'] },
  { component: BriefingContainer, name: 'missing report time stays explicit', props: { id: 'brf-grp-1', initial: { ...detailFixture, kind: 'other', observations: [], referenced_events: [{ event_id: 'evt-1', event_type: 'host.appeared' }] } }, expect: ['Report time', 'Not reported'] },
);

const appearanceFixture = {
  ...detailFixture, kind: 'other', source: 'agent', clamped: false,
  referenced_events: [{ event_id: 'appearance-1', event_type: 'host.appeared', occurred_at: '2026-11-05T15:38:41Z', target: '192.0.2.0/24' }],
  observations: [{ event_id: 'appearance-1', view_role: 'host', attributes: { hostname: 'review-demo', ip: '192.0.2.10', mac: '02:00:00:00:00:10', vendor: '', os: '', identity_confidence: 'high' }, ports_total: 0 }],
  decisions: [], effective_review: null
};
cases.push(
  { component: BriefingContainer, name: 'appearance provides actionable identity and explicit review', props: { id: 'appearance', initial: appearanceFixture }, expect: ['Reported host', 'review-demo', '192.0.2.10', '02:00:00:00:00:10', '192.0.2.0/24', 'Your decision', 'No operator review', 'Scanner snapshot', 'aria-label="Copy IP address"', 'data-evidence="event:appearance-1"'], absent: ['href="#evt-', 'href="#step-', 'checked', 'Locally administered', 'Demo data', 'Reported by'] },
  { component: BriefingContainer, name: 'partial evidence cannot become a single host summary', props: { id: 'appearance', initial: { ...appearanceFixture, observations_truncated: true } }, expect: ['Only part of the retained evidence is shown', 'some saved views are omitted'], absent: ['<dt>Reported host</dt>'] }
);

const { singleViewIdentity, reportRange } = await import('../src/lib/format.js');
if (singleViewIdentity(confidenceViews('high', 'low')) !== null || !reportRange([{ occurred_at: '2026-11-05T15:00:00Z' }, {}]).partial) {
  throw new Error('Previous/current and incomplete time ranges must stay explicit');
}

// Evidence navigation must open ancestors and focus the target without changing routes.
const { revealEvidence } = await import('../src/lib/evidenceNavigation.js');
const rootNode = { querySelectorAll: () => [targetNode] };
const disclosure = { tagName: 'DETAILS', open: false, parentElement: rootNode };
let scrolled = false;
let focused = false;
const targetNode = { dataset: { evidence: 'step:0' }, tagName: 'LI', parentElement: disclosure,
  scrollIntoView: () => { scrolled = true; }, focus: () => { focused = true; } };
if (!revealEvidence(rootNode, 'step:0') || !disclosure.open || !scrolled || !focused || revealEvidence(rootNode, 'missing')) {
  throw new Error('Evidence navigation must reveal and focus the exact target');
}

// Live attention membership changes after decisions, window expiry or a new
// top-N cut. The chosen review remains available even when its row disappears.
for (const kind of ['briefing', 'incident']) {
  const id = kind === 'briefing' ? attentionItem.id : 'inc-11-22';
  for (const attention of [[], [{ ...attentionItem, id: 'brf-another' }]]) {
    cases.push({
      name: `${kind} selection survives ${attention.length ? 'replacement' : 'empty'} attention`,
      props: { overview: base({ attention }), selected: { id, kind } },
      expect: ['aria-label="Selected review"', `href="#/${kind === 'briefing' ? 'briefing' : 'incidents'}/${id}"`, 'Close review', 'data-empty="false"'],
      absent: ['Select a review</h2>', 'data-empty="true"']
    });
  }
}
cases.push({ name: 'closed review allows empty layout', props: { overview: base(), selected: null }, expect: ['data-empty="true"'], absent: ['aria-label="Selected review"', 'Close review'] });

for (const c of cases) {
  const { body } = render(c.component ?? HomeContainer, { props: c.props });
  // Strip SSR hydration markers, which split copy across expressions.
  const stripped = body.replaceAll(/<!--.*?-->/g, '');
  if (c.component === IncidentContainer && (stripped.includes('[object Object]') || !stripped.includes('Source: lab'))) {
    console.error(`[${c.name}] incident timestamps or source scope rendered incorrectly`);
    failed++;
  }
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
  for (const [s, n] of c.expectCount ?? []) {
    const got = text.split(s).length - 1;
    if (got !== n) {
      console.error(`[${c.name}] expected ${n}x ${JSON.stringify(s)}, saw ${got}x`);
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

// Live incident invalidation now travels through the event stream's
// incidentRevision counter (covered behaviorally by characterize);
// the window-event forwarder it replaced is deleted, and screens
// subscribe from context with a standalone fallback.

// Exercise the same bounded continuation state the undo panel uses.
{
  const { memberUndoReview, memberUndoNote, continueMemberUndo } = await import('../src/lib/memberUndo.js');
  const initial = memberUndoReview('dec-1-2', {
    generated_decision_ids: Array.from({ length: 500 }, (_, i) => `dec-${i}`),
    generated_next: 600
  });
  if (!memberUndoNote(initial).includes('At least 500')) {
    throw new Error('partial undo count must be labelled');
  }
  let failed = false;
  try {
    await continueMemberUndo(initial, async () => { throw new Error('offline'); });
  } catch { failed = true; }
  if (!failed || initial.count !== 500 || initial.next !== 600) {
    throw new Error('failed continuation must preserve retry state');
  }
  const final = await continueMemberUndo(initial, async (id, after) => {
    if (id !== 'dec-1-2' || after !== 600) { throw new Error('wrong snapshot cursor'); }
    return { members: [{ decision_id: 'dec-501', subject_id: 'brf-501', kind: 'expected' }] };
  });
  if (final.count !== 501 || final.next !== 0 || final.members.length !== 1 || memberUndoNote(final).includes('At least')) {
    throw new Error('completed undo count must include remaining members');
  }
  console.log('smoke: undo member continuation holds');
}
