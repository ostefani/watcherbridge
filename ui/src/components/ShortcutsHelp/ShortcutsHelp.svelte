<script>
  import styles from './ShortcutsHelp.module.css';

  // Shortcut help: generated from the working registry, so documented
  // and working shortcuts cannot drift apart. Multi-key sequences live
  // in the App shell outside the registry and are listed statically.
  let { shortcuts } = $props();
  const globalKeys = $derived(shortcuts.describe('global'));
  const briefingKeys = $derived(shortcuts.describe('briefing'));
</script>

<section class={styles.help} aria-label="Keyboard shortcuts">
  <h2>Keyboard shortcuts</h2>
  <ul>
    {#each globalKeys as binding (binding.key)}
      <li><kbd>{binding.key}</kbd> — {binding.label}</li>
    {/each}
    <li><kbd>g</kbd> then <kbd>h</kbd> — Home</li>
    <li><kbd>g</kbd> then <kbd>d</kbd> — Reported hosts</li>
    {#each briefingKeys as binding (binding.key)}
      <li><kbd>{binding.key}</kbd> — on a briefing: {binding.label}, then confirm</li>
    {/each}
  </ul>
  <p>Shortcuts stay silent while typing. Briefing keys preset the composer; nothing submits on its own.</p>
</section>
