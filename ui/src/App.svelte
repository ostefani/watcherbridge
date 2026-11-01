<script>
  import { onMount } from 'svelte';

  // Step 22 shell: proves the built app loads under the viewer's CSP
  // and that the bridge event stream connects and recovers. No screens,
  // no verdict or attention rendering — that is Step 23.
  let state = $state('connecting');
  let lastEvent = $state('none');
  let source = null;

  const STATUS = {
    connecting: 'Connecting to the bridge event stream…',
    live: 'Connected: live briefings stream.',
    retrying: 'Connection lost — retrying automatically…'
  };

  function connect() {
    state = 'connecting';
    source = new EventSource('/events');
    source.onopen = () => {
      state = 'live';
    };
    source.onerror = () => {
      state = 'retrying';
      source?.close();
      source = null;
      window.setTimeout(connect, 2000);
    };
    for (const name of ['briefing', 'group', 'overview', 'resync']) {
      source.addEventListener(name, () => {
        lastEvent = `${name} @ ${new Date().toLocaleTimeString()}`;
      });
    }
  }

  onMount(() => {
    connect();
    return () => source?.close();
  });
</script>

<main>
  <h1>WatcherBridge</h1>
  <p role="status">{STATUS[state] ?? state}</p>
  <p>Last stream event: {lastEvent}</p>
</main>

<style>
  main {
    font-family: system-ui, -apple-system, 'Segoe UI', sans-serif;
    max-width: 40rem;
    margin: 2rem auto;
    padding: 0 1rem;
    line-height: 1.5;
    color: #1a1a1a;
  }
  h1 {
    font-size: 1.5rem;
    font-weight: 650;
  }
</style>
