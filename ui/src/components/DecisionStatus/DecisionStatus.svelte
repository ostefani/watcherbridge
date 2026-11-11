<script>
  import styles from './DecisionStatus.module.css';

  // Decision status: the panel title and the effective-review pill.
  // The pill wording is the list state, never a claim about the future.
  // The card variant restyles the title and the review pill; plain
  // panels keep the global heading and pill.
  let { title, effective = null, card = false } = $props();

  function statusLine() {
    if (effective === null || effective === undefined) {
      return 'No operator review';
    }
    if (effective.kind === 'snooze') {
      return `Snoozed${effective.until ? ` until ${effective.until}` : ''}`;
    }
    if (effective.kind === 'expected') {
      return 'Reviewed: expected';
    }
    return `Flagged: ${effective.kind.replace('_', ' ')}`;
  }
</script>

<h3 class={card ? styles.title : undefined}>{title}</h3>
<p class={styles.status}><span class={card ? `pill ${styles.review}` : 'pill'} data-review={effective?.kind ?? 'unreviewed'}>Status: {statusLine()}</span></p>
