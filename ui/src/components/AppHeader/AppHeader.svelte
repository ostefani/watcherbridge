<script>
  import ConnectionStatus from '@/components/ConnectionStatus/ConnectionStatus.svelte';
  import styles from './AppHeader.module.css';

  // App header: brand, view navigation and the connection state line.
  // The route and the connection state arrive as props from App, which
  // owns routing and the event stream; this component only renders.
  let { routeName, helpOpen = false, connectionState = 'connecting', onToggleHelp = () => {} } = $props();
</script>

<header class={styles.header}>
  <a class={styles.brand} href="#/" aria-label="WatcherBridge Home"><span class={styles.brandMark} aria-hidden="true">W</span>WatcherBridge</a>
  <nav aria-label="Views">
    <a href="#/" aria-current={routeName === 'home' ? 'page' : undefined}>Home</a>
    <a href="#/devices" aria-current={routeName === 'devices' || routeName === 'device' ? 'page' : undefined}>Reported hosts</a>
    <a href="#/health" aria-current={routeName === 'health' ? 'page' : undefined}>Health</a>
    <button type="button" onclick={onToggleHelp} aria-expanded={helpOpen}>Shortcuts</button>
  </nav>
  <ConnectionStatus state={connectionState} />
</header>
