<script>
  import Pill from '@/components/Pill/Pill.svelte';
  import { windowLabel } from '@/lib/format.js';
  import styles from './VerdictHeader.module.css';
  // Verdict header: the server owns the verdict; this only presents it.
  // Coverage sits beside the verdict as text, never hidden behind a
  // green badge. Quiet always carries its context (receipts, window,
  // last change) and never claims security or presence.
  let { verdict, headline, coverage, liveness, reasons = [], receipts, window, lastChange } = $props();
</script>

<section aria-label="Review verdict" class="verdict" data-verdict={verdict}>
  <div class="eyebrow">Review overview · last {windowLabel(window)}</div>
  <div class="verdict-heading"><h2>{headline}</h2>
    <ol class="verdict-scale" aria-label="Verdict scale">
      {#each ['quiet', 'review', 'act'] as level (level)}
        <li aria-current={verdict === level ? 'true' : undefined}>{level}</li>
      {/each}
    </ol>
  </div>
  <ul class={styles.list}>
    <li><Pill>Verdict: {verdict}</Pill></li>
    <li><Pill data-state={coverage}>Coverage: {coverage}</Pill></li>
    <li><Pill>Liveness: {liveness}</Pill></li>
  </ul>
  {#if reasons.length > 0}
    <ul class="reasons">
      {#each reasons as reason (reason)}
        <li>{reason}</li>
      {/each}
    </ul>
  {/if}
  <p class="context">
    {receipts} {receipts === 1 ? 'receipt' : 'receipts'} in the last {windowLabel(window)}{#if lastChange}, last
      change <time datetime={lastChange} title={lastChange}
        >{new Date(lastChange).toLocaleString()}</time
      >{/if}
  </p>
</section>
