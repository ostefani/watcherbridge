<script>
  import { latestRunNeedsAttention } from '@/lib/purge.js';
  import { displayTime } from '@/lib/format.js';
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
    <div><dt>Coverage</dt><dd><span class="pill" data-state={health.coverage}>{health.coverage}</span></dd></div>
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
    {@const finished = displayTime(health.purge.last.finished_at ?? health.purge.last.started_at)}
    <p role="alert">
      Latest purge run {health.purge.last.status}
      ({finished.local}){health.purge.last.error ? `: ${health.purge.last.error}` : ''}.
    </p>
  {/if}
  {#if refreshError !== null}
    <p role="alert">Refresh failed ({refreshError}) — showing retained data. <button type="button" onclick={onRetry}>Retry</button></p>
  {/if}
</section>
