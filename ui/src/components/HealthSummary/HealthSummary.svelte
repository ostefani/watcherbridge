<script>
  import Notice from '@/components/Notice/Notice.svelte';
  import TimeStamp from '@/components/TimeStamp/TimeStamp.svelte';
  import Pill from '@/components/Pill/Pill.svelte';
  import { latestRunNeedsAttention } from '@/lib/purge.js';
  import styles from './HealthSummary.module.css';

  // Concise health summary: coverage, 1h fallback, parked groups,
  // forward errors, queue. Unknown capabilities stay explicit.
  // `health` is the shown snapshot or fetch; refresh failures arrive
  // as `refreshError` with a retry owned by the container.
  let { health, refreshError = null, onRetry = () => {} } = $props();
</script>

<section aria-label="Summary">
  <h3>Summary</h3>
  <!--
    `health-metrics` is a hook class only: its rules moved here from
    base.css, and the name keeps the global `dl:not(.digest,…)`
    default grid from claiming this list.
  -->
  <dl class="health-metrics {styles.metrics}">
    <div><dt>Coverage</dt><dd><Pill data-state={health.coverage}>{health.coverage}</Pill></dd></div>
    <div><dt>Fallback briefings · 1h</dt><dd>{health.fallback_1h.used} <small>of {health.fallback_1h.total}</small></dd></div>
    <div><dt>Awaiting operator</dt><dd>{health.parked_groups}</dd></div>
    <div><dt>Forward errors · 1h</dt><dd>{health.forward_errors_1h}</dd></div>
    <div><dt>Ingest queue</dt><dd>{health.queue_depth} <small>of {health.queue_cap}</small></dd></div>
  </dl>
  {#if (health.coverage_reasons ?? []).length > 0}
    <ul>
      {#each health.coverage_reasons as reason (reason)}
        <li>{reason}</li>
      {/each}
    </ul>
  {/if}
  {#if latestRunNeedsAttention(health.purge.last)}
    <Notice kind="alert">
      Latest purge run {health.purge.last.status}
      (<TimeStamp iso={health.purge.last.finished_at ?? health.purge.last.started_at} />){health.purge.last.error ? `: ${health.purge.last.error}` : ''}.
    </Notice>
  {/if}
  {#if refreshError !== null}
    <Notice kind="alert" actionLabel="Retry" onAction={onRetry}>Refresh failed ({refreshError}) — showing retained data.</Notice>
  {/if}
</section>
