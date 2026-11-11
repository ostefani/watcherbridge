<script>
  import TimeStamp from '@/components/TimeStamp/TimeStamp.svelte';
  import Pill from '@/components/Pill/Pill.svelte';
  import { randomizedLabel, classLabel } from '@/lib/format.js';
  import styles from './ReportView.module.css';
  let { ref, views = [], profileLinks = [] } = $props();
  const value = (v) => typeof v === 'string' && v.trim() !== '' ? v : 'Not reported';
</script>

<details class={styles.card} data-evidence={`event:${ref.event_id}`} tabindex="-1" aria-label={`Saved report ${ref.event_id}`}>
  <summary>
    <span class={styles.heading}><span class={`mono ${styles.reportId}`}>{ref.event_id}</span><Pill>{ref.event_type}</Pill></span>
    <span class={styles.reportTime}>Reported <TimeStamp iso={ref.occurred_at} /></span>
  </summary>
  <div class={styles.body}>
    <p class="section-note">Reported scan target: <span class="mono">{ref.target || 'Not reported'}</span>{#if ref.target_truncated} (truncated){/if}<br />Received <TimeStamp iso={ref.received_at} /></p>
    {#if views.length === 0}
      <p>No retained observation for this event. Its reference is retained, but a saved scanner view is unavailable.</p>
    {:else}
      {#each views as view (`${view.event_id}:${view.view_role}`)}
        {@const attrs = view.attributes ?? {}}
        <section class={styles.view} aria-label={`${view.view_role} view`}>
          <h4>{view.view_role === 'previous' ? 'Before the reported change' : view.view_role === 'current' ? 'After the reported change' : 'Reported host'}</h4>
          <dl class={styles.identity}>
            <div><dt>Hostname</dt><dd>{value(attrs.hostname)}</dd></div>
            <div><dt>IP address</dt><dd class="mono">{value(attrs.ip)}</dd></div>
            <div><dt>MAC address</dt><dd class="mono">{value(attrs.mac)}</dd></div>
            {#if attrs.vendor}<div><dt>Reported vendor</dt><dd>{attrs.vendor}</dd></div>{/if}
            {#if attrs.os}<div><dt>Reported OS</dt><dd>{attrs.os}</dd></div>{/if}
            {#if attrs.device}<div><dt>Reported device</dt><dd>{attrs.device}</dd></div>{/if}
          </dl>
          <p class={styles.confidence}><Pill>Scanner-reported identity confidence: {attrs.identity_confidence || 'unknown'}</Pill><span class="section-note">Identity source: {attrs.identity_source || 'unknown'}</span></p>
          <p class="section-note">{randomizedLabel(attrs.randomized)}</p>
          {#if attrs.attributes_truncated}<p class="section-note">Some reported fields were truncated at ingest.</p>{/if}
          <details class={styles.attributes}><summary>Saved report view · technical attributes</summary><pre>{JSON.stringify(attrs, null, 2)}</pre></details>
          <p class="section-note">Snapshot covers {view.ports_total} ports{view.ports_truncated ? ' (truncated)' : ''}.</p>
          {#if (view.ports ?? []).length > 0}
            <ul>{#each view.ports as port (`${port.proto}/${port.port}`)}<li>{port.proto}/{port.port} {port.state}{port.service ? ` (${port.service})` : ''}</li>{/each}</ul>
          {/if}
          {#if view.profile_id}
            <a class={styles.profileChip} href={`#/devices/${encodeURIComponent(view.profile_id)}`}><span>Open reported context →</span><span class="section-note">{classLabel(view.reference_class)} · {profileLinks.find((link) => link.profile_id === view.profile_id)?.role ?? view.view_role}</span></a>
          {:else}<p class="section-note">No linked profile is available for this saved view.</p>{/if}
        </section>
      {/each}
    {/if}
  </div>
</details>
