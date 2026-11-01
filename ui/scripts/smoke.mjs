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
  const code = js.code.replaceAll(/from '(\.\/[^']+)\.svelte'/g, "from '$1.js'");
  writeFileSync(join(out, name.replace(/\.svelte$/, '.js')), code);
}

// The specifier is built from constants (own temp dir, fixed name),
// never from network or user input.
// eslint-disable-next-line no-unsanitized/method
const { default: Home } = await import(pathToFileURL(join(out, 'Home.js')).href);

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

const cases = [
  {
    name: 'quiet empty',
    props: { overview: base(), loadError: null },
    expect: ['No reports in the last 24h0m0s', 'no receipts', 'Coverage: ok', 'liveness:', 'unknown']
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
        reasons: ['fallback ratio high'],
        digest: { ...zeroDigest, port_closed: 2 }
      }),
      loadError: null
    },
    expect: ['1 need a check now', 'port_closed', 'fallback ratio high', 'Coverage is degraded']
  },
  {
    name: 'load error',
    props: { overview: null, loadError: 'overview 500' },
    expect: ['Overview unavailable', 'overview 500']
  }
];

let failed = 0;
for (const c of cases) {
  const { body } = render(Home, { props: c.props });
  // Server text is HTML-escaped; unescape the entities our copy uses.
  const text = body
    .replaceAll('&quot;', '"')
    .replaceAll('&#39;', "'")
    .replaceAll('&amp;', '&')
    .replaceAll('&lt;', '<')
    .replaceAll('&gt;', '>');
  for (const s of c.expect) {
    if (!text.includes(s)) {
      console.error(`[${c.name}] missing ${JSON.stringify(s)}`);
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
