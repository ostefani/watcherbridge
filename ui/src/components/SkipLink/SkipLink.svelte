<script>
  import styles from './SkipLink.module.css';

  // Skip link: first tab stop, jumps straight to the content pane.
  // UI-only behavior; visible on focus only.
</script>

<a class={styles.skip} href="#content" onclick={(event) => { event.preventDefault(); document.getElementById('content')?.focus(); }}>Skip to content</a>
