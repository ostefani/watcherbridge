<script>
  import { onMount } from 'svelte';
  import Home from './lib/Home.svelte';
  import Detail from './lib/Detail.svelte';
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

  function connect() {
    stream = 'connecting';
    source = new EventSource('/events');
    source.onopen = () => {
      stream = 'live';
      load();
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
      source?.close();
      source = null;
    };
  });
</script>

<main>
  <h1>WatcherBridge</h1>
  <nav aria-label="Views">
    <a href="#/" aria-current={route.name === 'home' ? 'page' : undefined}>Home</a>
    <a href="#/devices" aria-current={route.name === 'devices' || route.name === 'device' ? 'page' : undefined}>Reported hosts</a>
    <a href="#/health" aria-current={route.name === 'health' ? 'page' : undefined}>Health</a>
    <button type="button" onclick={() => (showHelp = !showHelp)} aria-expanded={showHelp}>Shortcuts</button>
  </nav>
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
  <ConnectionStatus state={stream} />
  {#key routeKey}
    {#if route.name === 'home'}
      <Home {overview} {loadError} />
    {:else if route.name === 'briefing'}
      <Detail id={route.id} />
    {:else if route.name === 'devices'}
      <Devices {stream} />
    {:else if route.name === 'device'}
      <Device id={route.id} {stream} />
    {:else if route.name === 'health'}
      <Health {stream} />
    {/if}
  {/key}
</main>

<style>
  main {
    font-family: system-ui, -apple-system, 'Segoe UI', sans-serif;
    max-width: 48rem;
    margin: 0 auto;
    padding: 0 1rem 2rem;
    line-height: 1.5;
    color: #1a1a1a;
    background: #fff;
  }
  h1 {
    font-size: 1.5rem;
    font-weight: 650;
  }
  nav {
    display: flex;
    gap: 1rem;
    margin-bottom: 1rem;
  }
  nav a[aria-current='page'] {
    font-weight: 700;
  }
  :global(a:focus-visible, button:focus-visible, summary:focus-visible) {
    outline: 2px solid #1a1a1a;
    outline-offset: 2px;
  }
  kbd {
    border: 1px solid #999;
    padding: 0 0.3rem;
    font-size: 0.85rem;
  }
  @media (prefers-color-scheme: dark) {
    main {
      color: #e8e8e8;
      background: #111;
    }
    :global(a) {
      color: #8ab4f8;
    }
    :global(a:focus-visible, button:focus-visible, summary:focus-visible) {
      outline-color: #e8e8e8;
    }
  }
  @media (prefers-reduced-motion: reduce) {
    :global(*) {
      transition: none;
      animation: none;
    }
  }
</style>
