<script>
  import styles from './Pill.module.css';

  // Pill: the single badge owner. `priority` selects the priority
  // treatment; every other attribute (data-*, aria-*, title) passes
  // straight through, so status and identity pills keep their hooks.
  let { priority = false, large = false, children, ...rest } = $props();
</script>

<span class={`${styles.pill}${priority ? ` ${styles.priority}` : ''}${large ? ` ${styles.large}` : ''}`} {...rest}>{@render children()}</span>
