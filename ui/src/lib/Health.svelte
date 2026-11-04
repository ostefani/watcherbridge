<script>
  import { onDestroy, onMount } from 'svelte';
  import { fetchHealth } from '../api.js';
  import { createFetchGuard } from './freshness.js';
  import { latestRunNeedsAttention } from './purge.js';
  import { displayTime } from './format.js';

  // Health (Designv9 §8.2): concise summary with expandable structured
  // details — never a raw JSON dump. Source limitations, active errors
  // and purge failures stay visible without expansion. Counters are
  // labeled process-lifetime; unknown capabilities are explicit.
  // initial carries a prerender snapshot; the reconnect edge still
  // refreshes from the API once one is provided.
  let { stream, initial = null } = $props();

  const guard = createFetchGuard();
  onDestroy(() => guard.invalidate());

  let health = $state(null);
  let error = $state(null);
  let refreshError = $state(null);

  // Rendered health: fetched completion wins, snapshot fills in until
  // (or instead of) the fetch.
  const shown = $derived(health ?? initial);

  // A failed refresh keeps existing data visible with a notice and
  // Retry; the first load still fails into the error state.
  async function load() {
    guard.invalidate();
    const token = guard.start();
    error = null;
    refreshError = null;
    try {
      const fresh = await fetchHealth();
      if (guard.current(token)) {
        health = fresh;
      }
    } catch (err) {
      if (guard.current(token)) {
        const message = err instanceof Error ? err.message : String(err);
        if (shown === null) {
          error = message;
        } else {
          refreshError = message;
        }
      }
    }
  }

  onMount(() => {
    if (initial === null) {
      load();
    }
  });

  // Refresh live health on the reconnect edge only; the immutable
  // detail screens never refetch on their own.
  let wasLive = false;
  $effect(() => {
    const live = stream === 'live';
    const edge = live && !wasLive && health !== null;
    wasLive = live;
    if (edge) {
      load();
    }
  });

  /** @param {number} seconds */
  function days(seconds) {
    return `${Math.floor(seconds / 86400)} days`;
  }
</script>

<section aria-label="Health">
  <h2>Health</h2>
  <p class="section-note">Source coverage, processing, and retention. Liveness describes the source, not host presence.</p>
  {#if error !== null && shown === null}
    <p role="alert">The bridge did not return health ({error}).</p>
    <button type="button" onclick={load}>Retry</button>
  {:else if shown === null}
    <p role="status">Loading health…</p>
  {:else}
    <section aria-label="Summary">
      <h3>Summary</h3>
      <dl class="health-metrics">
        <div><dt>Coverage</dt><dd><span class="pill" data-state={shown.coverage}>{shown.coverage}</span></dd></div>
        <div><dt>Fallback briefings · 1h</dt><dd>{shown.fallback_1h.used} <small>of {shown.fallback_1h.total}</small></dd></div>
        <div><dt>Awaiting operator</dt><dd>{shown.parked_groups}</dd></div>
        <div><dt>Forward errors · 1h</dt><dd>{shown.forward_errors_1h}</dd></div>
        <div><dt>Ingest queue</dt><dd>{shown.queue_depth} <small>of {shown.queue_cap}</small></dd></div>
      </dl>
      {#if (shown.coverage_reasons ?? []).length > 0}
        <ul>
          {#each shown.coverage_reasons as reason (reason)}
            <li>{reason}</li>
          {/each}
        </ul>
      {/if}
      {#if latestRunNeedsAttention(shown.purge.last)}
        {@const finished = displayTime(shown.purge.last.finished_at ?? shown.purge.last.started_at)}
        <p role="alert">
          Latest purge run {shown.purge.last.status}
          ({finished.local}){shown.purge.last.error ? `: ${shown.purge.last.error}` : ''}.
        </p>
      {/if}
      {#if refreshError !== null}
        <p role="alert">Refresh failed ({refreshError}) — showing retained data. <button type="button" onclick={load}>Retry</button></p>
      {/if}
    </section>

    <details open>
      <summary>Sources ({shown.sources.length})</summary>
      {#if shown.sources.length === 0}
        <p>No sources registered.</p>
      {:else}
        {#each shown.sources as src (src.scope)}
          <section aria-label={`Source ${src.scope}`}>
            <h4>{src.scope} — {src.status}</h4>
            <dl>
              <dt>Liveness</dt>
              <dd>{src.liveness}{src.liveness === 'unknown' ? ' (no heartbeat capability; silence is not absence)' : ''}</dd>
              <dt>Identity namespace</dt>
              <dd>{src.identity_namespace || 'unknown — stable identity contract unavailable'}</dd>
              <dt>Identity capability</dt>
              <dd>{src.capabilities || 'not reported — heartbeat is not identity proof'}</dd>
              <dt>Heartbeat</dt>
              <dd>
                {src.heartbeat_enabled ? 'enabled (delivery opt-in, not identity proof)' : 'disabled'}
                {#if src.last_heartbeat_at}— last at {displayTime(src.last_heartbeat_at).local}{/if}
              </dd>
              {#if src.heartbeat}
                {@const scan = displayTime(src.heartbeat.completed_at)}
                <dt>Last completed scan</dt>
                <dd><time datetime={src.heartbeat.completed_at} title={scan.utc}>{scan.local}</time></dd>
                <dt>Scan result</dt>
                <dd>{src.heartbeat.status}, {src.heartbeat.issue_count} {src.heartbeat.issue_count === 1 ? 'issue' : 'issues'}, {src.heartbeat.duration_ms} ms</dd>
                <dt>Reported host records</dt>
                <dd>{src.heartbeat.hosts_seen} — scan records, not a current inventory or device count</dd>
                <dt>Signing clock offset</dt>
                <dd>{src.heartbeat.clock_skew_seconds} seconds (includes delivery delay)</dd>
              {/if}
              {#if src.last_report_at}
                {@const at = displayTime(src.last_report_at)}
                <dt>Last report</dt>
                <dd><time datetime={src.last_report_at} title={at.utc}>{at.local}</time></dd>
              {/if}
              {#if src.delivery_status}
                <dt>Delivery</dt>
                <dd>{src.delivery_status}</dd>
              {/if}
            </dl>
            {#if (src.reasons ?? []).length > 0}
              <ul>
                {#each src.reasons as reason (reason)}
                  <li>{reason}</li>
                {/each}
              </ul>
            {/if}
          </section>
        {/each}
      {/if}
    </details>

    <details>
      <summary>Pipeline (process lifetime)</summary>
      <dl>
        <dt>Started</dt>
        <dd>{displayTime(shown.process.started_at).local}</dd>
        <dt>Ingest accepted</dt>
        <dd>{shown.process.ingest_accepted}</dd>
        <dt>Ingest duplicates</dt>
        <dd>{shown.process.ingest_duplicates}</dd>
        <dt>Ingest rejected</dt>
        <dd>{shown.process.ingest_rejected}</dd>
        <dt>Ingest saturated</dt>
        <dd>{shown.process.ingest_saturated}</dd>
        <dt>Provider calls</dt>
        <dd>{shown.process.provider_calls}</dd>
        <dt>Provider fallbacks</dt>
        <dd>{shown.process.provider_fallbacks}</dd>
        <dt>Redaction blocked</dt>
        <dd>{shown.process.redaction_blocked}</dd>
        <dt>Writer saturated</dt>
        <dd>{shown.process.writer_saturated}</dd>
      </dl>
    </details>

    <details>
      <summary>Purge</summary>
      <dl>
        {#each Object.entries(shown.purge.totals ?? {}) as [table, count] (table)}
          <dt>{table}</dt>
          <dd>{count}</dd>
        {/each}
      </dl>
      {#if shown.retention}
        <p>
          Configured retention: {days(shown.retention.reports_seconds)} for reports{#if shown.retention.subjects_seconds !== undefined}, {days(shown.retention.subjects_seconds)} for subjects{/if};
          policy only — it never proves a per-profile boundary. Ask the API for per-profile history.
        </p>
      {/if}
      {#if shown.purge.last_success}
        {@const ok = displayTime(shown.purge.last_success.finished_at ?? shown.purge.last_success.started_at)}
        <p>Latest fully successful purge: {ok.local}.</p>
      {:else}
        <p>No fully successful purge recorded yet.</p>
      {/if}
    </details>

    {#if shown.publisher}
      <details>
        <summary>Publisher</summary>
        <dl>
          <dt>Posted</dt>
          <dd>{shown.publisher.posted}</dd>
          <dt>Dropped</dt>
          <dd>{shown.publisher.dropped}</dd>
          <dt>Failed</dt>
          <dd>{shown.publisher.failed}</dd>
        </dl>
      </details>
    {/if}
  {/if}
</section>
