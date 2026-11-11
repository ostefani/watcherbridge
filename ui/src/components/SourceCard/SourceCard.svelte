<script>
  import TimeStamp from '@/components/TimeStamp/TimeStamp.svelte';

  // One source card: capability limits, heartbeat, last report and
  // reasons. Unknown stays unknown — a missing capability never
  // implies the scanner is offline, and silence is not absence.
  let { source } = $props();
</script>

<section aria-label={`Source ${source.scope}`}>
  <h4>{source.scope} — {source.status}</h4>
  <dl>
    <dt>Liveness</dt>
    <dd>{source.liveness}{source.liveness === 'unknown' ? ' (no heartbeat capability; silence is not absence)' : ''}</dd>
    <dt>Identity namespace</dt>
    <dd>{source.identity_namespace || 'unknown — stable identity contract unavailable'}</dd>
    <dt>Identity capability</dt>
    <dd>{source.capabilities || 'not reported — heartbeat is not identity proof'}</dd>
    <dt>Heartbeat</dt>
    <dd>
      {source.heartbeat_enabled ? 'enabled (delivery opt-in, not identity proof)' : 'disabled'}
      {#if source.last_heartbeat_at}— last at <TimeStamp iso={source.last_heartbeat_at} />{/if}
    </dd>
    {#if source.heartbeat}
      <dt>Last completed scan</dt>
      <dd><TimeStamp iso={source.heartbeat.completed_at} /></dd>
      <dt>Scan result</dt>
      <dd>{source.heartbeat.status}, {source.heartbeat.issue_count} {source.heartbeat.issue_count === 1 ? 'issue' : 'issues'}, {source.heartbeat.duration_ms} ms</dd>
      <dt>Reported host records</dt>
      <dd>{source.heartbeat.hosts_seen} — scan records, not a current inventory or device count</dd>
      <dt>Signing clock offset</dt>
      <dd>{source.heartbeat.clock_skew_seconds} seconds (includes delivery delay)</dd>
    {/if}
    {#if source.last_report_at}
      <dt>Last report</dt>
      <dd><TimeStamp iso={source.last_report_at} /></dd>
    {/if}
    {#if source.delivery_status}
      <dt>Delivery</dt>
      <dd>{source.delivery_status}</dd>
    {/if}
  </dl>
  {#if (source.reasons ?? []).length > 0}
    <ul>
      {#each source.reasons as reason (reason)}
        <li>{reason}</li>
      {/each}
    </ul>
  {/if}
</section>
