<script>
  import { onMount } from 'svelte';
  import Home from './lib/Home.svelte';
  import Detail from './lib/Detail.svelte';
  import Incident from './lib/Incident.svelte';
  import { forwardIncidentEvents } from './lib/incidentEvents.js';
  import Devices from './lib/Devices.svelte';
  import Device from './lib/Device.svelte';
  import Health from './lib/Health.svelte';
  import ConnectionStatus from './lib/ConnectionStatus.svelte';
  import { createFetchGuard } from './lib/freshness.js';
  import { moveRowFocus, typingTarget } from './lib/roving.js';
  import { fetchOverview } from './api.js';

  // Step 24 shell: hash routes for home, briefing detail, reported
  // hosts (list + profile) and health. The bridge owns all verdict
  // logic — the app renders envelopes and refreshes them from
  // `overview` stream frames, refetching only on connect. Detail
  // screens are immutable once briefed; live screens refetch or mark
  // stale on the reconnect edge themselves.
  let routeKey = $state(window.location.hash || '#/');
  let overview = $state(null);
  let loadError = $state(null);
  let stream = $state('connecting');
  let showHelp = $state(false);
  let source = null;
  let retryTimer = 0;
  let pendingG = false;
  let pendingTimer = 0;

  const guard = createFetchGuard();

  function parseRoute(hash) {
    const h = hash || '#/';
    let m = h.match(/^#\/briefing\/(.+)$/);
    if (m) {
      return { name: 'briefing', id: decodeURIComponent(m[1]) };
    }
    m = h.match(/^#\/incidents\/(.+)$/);
    if (m) {
      return { name: 'incident', id: decodeURIComponent(m[1]) };
    }
    m = h.match(/^#\/devices\/(.+)$/);
    if (m) {
      return { name: 'device', id: decodeURIComponent(m[1]) };
    }
    if (h === '#/devices' || h === '#/devices/') {
      return { name: 'devices' };
    }
    if (h === '#/health') {
      return { name: 'health' };
    }
    return { name: 'home' };
  }

  const route = $derived(parseRoute(routeKey));

  async function load() {
    const token = guard.start();
    try {
      const fresh = await fetchOverview();
      if (guard.current(token)) {
        overview = fresh;
        loadError = null;
      }
    } catch (err) {
      // A stale failure must never hide a valid overview; a current
      // failure only surfaces when no overview exists yet.
      if (guard.current(token) && overview === null) {
        loadError = err instanceof Error ? err.message : String(err);
      }
    }
  }

  let releaseIncidentEvents = () => {};

  function connect() {
    stream = 'connecting';
    source = new EventSource('/events');
    releaseIncidentEvents = forwardIncidentEvents(source, window, load);
    source.onopen = () => {
      stream = 'live';
    };
    source.addEventListener('overview', (/** @type {MessageEvent} */ ev) => {
      try {
        overview = JSON.parse(ev.data);
        guard.streamed();
        loadError = null;
      } catch {
        // A malformed frame degrades to the next refresh, never a crash.
      }
    });
    source.onerror = () => {
      releaseIncidentEvents();
      stream = 'retrying';
      source?.close();
      source = null;
      retryTimer = window.setTimeout(connect, 2000);
    };
  }

  // Navigation-only shortcuts, added where their controls exist.
  // Briefing decision keys live in Detail, scoped to that screen.
  function onKey(/** @type {KeyboardEvent} */ ev) {
    if (ev.key === '?' && !typingTarget(ev)) {
      showHelp = !showHelp;
      ev.preventDefault();
      return;
    }
    if (typingTarget(ev)) {
      pendingG = false;
      return;
    }
    if (pendingG) {
      pendingG = false;
      window.clearTimeout(pendingTimer);
      if (ev.key === 'h') {
        window.location.hash = '#/';
      } else if (ev.key === 'd') {
        window.location.hash = '#/devices';
      }
      return;
    }
    if (ev.key === 'g') {
      pendingG = true;
      window.clearTimeout(pendingTimer);
      pendingTimer = window.setTimeout(() => {
        pendingG = false;
      }, 1000);
      return;
    }
    if (ev.key === 'j' || ev.key === 'k') {
      if (moveRowFocus(ev.key === 'j' ? 1 : -1)) {
        ev.preventDefault();
      }
    }
  }

  onMount(() => {
    const onHash = () => {
      routeKey = window.location.hash || '#/';
    };
    window.addEventListener('hashchange', onHash);
    window.addEventListener('keydown', onKey);
    routeKey = window.location.hash || '#/';
    load();
    connect();
    return () => {
      window.removeEventListener('hashchange', onHash);
      window.removeEventListener('keydown', onKey);
      window.clearTimeout(retryTimer);
      window.clearTimeout(pendingTimer);
      releaseIncidentEvents();
      source?.close();
      source = null;
    };
  });
</script>

<a class="skip-link" href="#content" onclick={(event) => { event.preventDefault(); document.getElementById('content')?.focus(); }}>Skip to content</a>
<div class="app-shell">
<header class="app-header">
  <a class="brand" href="#/" aria-label="WatcherBridge Home"><span class="brand-mark" aria-hidden="true">W</span>WatcherBridge</a>
  <nav aria-label="Views">
    <a href="#/" aria-current={route.name === 'home' ? 'page' : undefined}>Home</a>
    <a href="#/devices" aria-current={route.name === 'devices' || route.name === 'device' ? 'page' : undefined}>Reported hosts</a>
    <a href="#/health" aria-current={route.name === 'health' ? 'page' : undefined}>Health</a>
    <button type="button" onclick={() => (showHelp = !showHelp)} aria-expanded={showHelp}>Shortcuts</button>
  </nav>
  <ConnectionStatus state={stream} />
</header>
  {#if showHelp}
    <section aria-label="Keyboard shortcuts">
      <h2>Keyboard shortcuts</h2>
      <ul>
        <li><kbd>j</kbd> / <kbd>k</kbd> — move between rows in a list</li>
        <li><kbd>g</kbd> then <kbd>h</kbd> — Home</li>
        <li><kbd>g</kbd> then <kbd>d</kbd> — Reported hosts</li>
        <li><kbd>?</kbd> — toggle this list</li>
        <li><kbd>e</kbd> / <kbd>s</kbd> / <kbd>x</kbd> — on a briefing: preset expected / snooze / not-expected, then confirm</li>
      </ul>
      <p>Shortcuts stay silent while typing. Briefing keys preset the composer; nothing submits on its own.</p>
    </section>
  {/if}
  <main id="content" tabindex="-1">
  <h1 class="sr-only">{({ home: 'Review overview', devices: 'Reported hosts', device: 'Reported host detail', briefing: 'Briefing', incident: 'Incident', health: 'Health' })[route.name]}</h1>
  {#key routeKey}
    {#if route.name === 'home'}
      <Home {overview} {loadError} />
    {:else if route.name === 'briefing'}
      <Detail id={route.id} />
    {:else if route.name === 'incident'}
      <Incident id={route.id} />
    {:else if route.name === 'devices'}
      <Devices {stream} />
    {:else if route.name === 'device'}
      <Device id={route.id} {stream} />
    {:else if route.name === 'health'}
      <Health {stream} />
    {/if}
  {/key}
</main>
<footer class="app-footer"><span>WatcherBridge · Local review workspace</span><span>Scanner reports are evidence, not a live inventory.</span></footer>
</div>

<style>
  .app-header {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 12px 28px;
    padding: 12px 28px;
    background: var(--surface);
    border-bottom: 1px solid var(--border);
  }
  .brand {
    display: inline-flex;
    align-items: center;
    gap: 10px;
    color: var(--text);
    font-size: 18px;
    font-weight: 700;
    text-decoration: none;
    letter-spacing: -0.5px;
  }
  .brand-mark {
    display: grid;
    place-items: center;
    width: 30px;
    height: 30px;
    border-radius: 8px;
    background: var(--text);
    color: var(--surface);
    font-size: 17px;
  }
  nav {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }
  nav a, nav button {
    display: inline-flex;
    align-items: center;
    padding: 8px 14px;
    min-height: 44px;
    border: 0;
    border-radius: 8px;
    background: transparent;
    text-decoration: none;
    color: var(--muted);
  }
  nav a[aria-current='page'] {
    background: var(--accent-soft);
    color: var(--accent);
    font-weight: 650;
  }
  main {
    max-width: 1500px;
    margin: 0 auto;
    padding: 28px;
    min-height: calc(100dvh - 160px);
  }
  main:focus {
    outline: none;
  }
  .app-footer {
    display: flex;
    flex-wrap: wrap;
    justify-content: space-between;
    gap: 8px;
    padding: 16px 28px;
    border-top: 1px solid var(--border);
    font-size: 12px;
    color: var(--muted);
  }
  .skip-link {
    position: fixed;
    left: 16px;
    top: -100px;
    z-index: 10;
    padding: 12px;
    background: var(--surface);
  }
  .skip-link:focus {
    top: 12px;
  }
  section[aria-label='Keyboard shortcuts'] {
    margin: 16px 28px;
    padding: 20px;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
  }
  @media (max-width: 640px) {
    .app-header {
      padding: 12px 16px;
      gap: 10px;
    }
    nav {
      order: 3;
      width: 100%;
    }
    nav a, nav button {
      padding: 8px 10px;
    }
    main {
      padding: 16px;
    }
    .app-footer {
      padding: 16px;
    }
  }
</style>
