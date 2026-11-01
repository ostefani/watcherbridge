<script>
  import { onMount } from 'svelte';
  import Home from './lib/Home.svelte';
  import ConnectionStatus from './lib/ConnectionStatus.svelte';
  import { fetchOverview } from './api.js';

  // Step 23 app: hash routes (#/ home now; #/devices/:id and #/health
  // land in Step 24). The bridge owns all verdict logic — the app
  // renders the overview envelope and refreshes it from `overview`
  // stream events, refetching only on connect.
  let route = $state(window.location.hash || '#/');
  let overview = $state(null);
  let loadError = $state(null);
  let stream = $state('connecting');
  let source = null;
  let retryTimer = 0;

  function currentRoute() {
    const hash = window.location.hash || '#/';
    if (hash.startsWith('#/devices/')) {
      return 'devices';
    }
    if (hash === '#/health') {
      return 'health';
    }
    return 'home';
  }

  async function load() {
    try {
      overview = await fetchOverview();
      loadError = null;
    } catch (err) {
      loadError = err instanceof Error ? err.message : String(err);
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

  onMount(() => {
    const onHash = () => {
      route = currentRoute();
    };
    window.addEventListener('hashchange', onHash);
    route = currentRoute();
    load();
    connect();
    return () => {
      window.removeEventListener('hashchange', onHash);
      window.clearTimeout(retryTimer);
      source?.close();
      source = null;
    };
  });
</script>

<main>
  <h1>WatcherBridge</h1>
  <nav aria-label="Views">
    <a href="#/" aria-current={route === 'home' ? 'page' : undefined}>Home</a>
    <a href="#/devices" aria-current={route === 'devices' ? 'page' : undefined}>Reported hosts</a>
    <a href="#/health" aria-current={route === 'health' ? 'page' : undefined}>Health</a>
  </nav>
  <ConnectionStatus state={stream} />
  {#if route === 'home'}
    <Home {overview} {loadError} />
  {:else}
    <section aria-label="Not yet available">
      <h2>{route === 'devices' ? 'Reported hosts' : 'Health'}</h2>
      <p>This screen arrives in Step 24. The bridge API already serves its data.</p>
    </section>
  {/if}
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
  :global(a:focus-visible, button:focus-visible) {
    outline: 2px solid #1a1a1a;
    outline-offset: 2px;
  }
  @media (prefers-color-scheme: dark) {
    main {
      color: #e8e8e8;
      background: #111;
    }
    :global(a) {
      color: #8ab4f8;
    }
    :global(a:focus-visible, button:focus-visible) {
      outline-color: #e8e8e8;
    }
  }
</style>
