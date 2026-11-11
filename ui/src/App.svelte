<script>
  import { onMount } from 'svelte';
  import { fetchOverview } from '@/api.js';
  import { createFetchGuard } from '@/lib/freshness.js';
  import { createEventStream } from '@/lib/events/stream.js';
  import { setConnection } from '@/lib/events/context.js';
  import { createShortcutRegistry } from '@/lib/shortcuts/registry.js';
  import { parseRoute, routeTitle } from '@/lib/router/routes.js';
  import { moveRowFocus, typingTarget } from '@/lib/roving.js';
  import HomeContainer from './containers/HomeContainer/HomeContainer.svelte';
  import IncidentMembersContainer from './containers/IncidentMembersContainer/IncidentMembersContainer.svelte';
  import BriefingContainer from './containers/BriefingContainer/BriefingContainer.svelte';
  import IncidentContainer from './containers/IncidentContainer/IncidentContainer.svelte';
  import DevicesContainer from './containers/DevicesContainer/DevicesContainer.svelte';
  import DeviceContainer from './containers/DeviceContainer/DeviceContainer.svelte';
  import HealthContainer from './containers/HealthContainer/HealthContainer.svelte';
  import SkipLink from './components/SkipLink/SkipLink.svelte';
  import AppHeader from './components/AppHeader/AppHeader.svelte';
  import AppFooter from './components/AppFooter/AppFooter.svelte';
  import ShortcutsHelp from './components/ShortcutsHelp/ShortcutsHelp.svelte';

  // Thin shell: hash routes, the single event stream (shared through
  // context so screens read connection state without prop drilling),
  // the global shortcut registry behind the help panel, and layout.
  // The bridge owns all verdict logic — the app renders envelopes and
  // refreshes them from `overview` stream frames, refetching only on
  // connect. Briefing screens are immutable once briefed; live screens
  // refetch or mark stale on the reconnect edge themselves.
  let routeKey = $state(window.location.hash || '#/');
  let overview = $state(null);
  let loadError = $state(null);
  let showHelp = $state(false);
  let pendingG = false;
  let pendingTimer = 0;

  const guard = createFetchGuard();
  const shortcuts = createShortcutRegistry();
  const stream = createEventStream({
    onOverview: (frame) => {
      overview = frame;
      guard.streamed();
      loadError = null;
    },
    onIncident: () => {
      load();
    }
  });
  // Single connection for the tree: screens read it from context, so
  // no stream prop is drilled. Never a module-level singleton, so
  // prerender stays connection-free.
  setConnection(stream);
  const { connection: connectionState } = stream;

  shortcuts.register('global', '?', 'Toggle this shortcut list', (ev) => {
    showHelp = !showHelp;
    ev.preventDefault();
  });
  shortcuts.register('global', 'j', 'Move between rows in a list', (ev) => {
    if (moveRowFocus(1)) {
      ev.preventDefault();
    }
  });
  shortcuts.register('global', 'k', 'Move between rows in a list', (ev) => {
    if (moveRowFocus(-1)) {
      ev.preventDefault();
    }
  });

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

  // Navigation-only shortcuts. Multi-key sequences (g h, g d) stay
  // here — they are navigation, not screen actions. Anything else goes
  // through the registry, which stays silent while typing and never
  // claims modifier combos.
  function onKey(/** @type {KeyboardEvent} */ ev) {
    if (typingTarget(ev)) {
      pendingG = false;
      return;
    }
    if (ev.key === 'g' && !ev.metaKey && !ev.ctrlKey && !ev.altKey) {
      pendingG = true;
      window.clearTimeout(pendingTimer);
      pendingTimer = window.setTimeout(() => {
        pendingG = false;
      }, 1000);
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
    shortcuts.dispatch('global', ev);
  }

  onMount(() => {
    const onHash = () => {
      routeKey = window.location.hash || '#/';
    };
    window.addEventListener('hashchange', onHash);
    window.addEventListener('keydown', onKey);
    routeKey = window.location.hash || '#/';
    load();
    stream.connect();
    return () => {
      window.removeEventListener('hashchange', onHash);
      window.removeEventListener('keydown', onKey);
      window.clearTimeout(pendingTimer);
      stream.disconnect();
    };
  });
</script>

<SkipLink />
<div class="app-shell">
  <AppHeader routeName={route.name} helpOpen={showHelp} connectionState={$connectionState} onToggleHelp={() => (showHelp = !showHelp)} />
  {#if showHelp}
    <ShortcutsHelp {shortcuts} />
  {/if}
  <main id="content" class="shell-main" tabindex="-1">
    <h1 class="sr-only">{routeTitle(route.name)}</h1>
    {#key routeKey}
      {#if route.name === 'home'}
        <HomeContainer {overview} {loadError}>
          {#snippet members(id, open)}
            <IncidentMembersContainer incidentId={id} expanded={open} />
          {/snippet}
          {#snippet pane(sel, ontitle)}
            {#if sel.kind === 'briefing'}
              <BriefingContainer id={sel.id} layout="embedded" {ontitle} {shortcuts} />
            {:else}
              <IncidentContainer id={sel.id} />
            {/if}
          {/snippet}
        </HomeContainer>
      {:else if route.name === 'briefing'}
        <BriefingContainer id={route.id} {shortcuts} />
      {:else if route.name === 'incident'}
        <IncidentContainer id={route.id} />
      {:else if route.name === 'devices'}
        <DevicesContainer />
      {:else if route.name === 'device'}
        <DeviceContainer id={route.id} />
      {:else if route.name === 'health'}
        <HealthContainer />
      {/if}
    {/key}
  </main>
  <AppFooter />
</div>
