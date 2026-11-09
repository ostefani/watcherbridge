<script>
  import { displayTime, randomizedLabel, classLabel } from './format.js';
  let { ref, views = [], profileLinks = [] } = $props();
  const reported = $derived(displayTime(ref.occurred_at));
  const received = $derived(displayTime(ref.received_at));
  const value = (v) => typeof v === 'string' && v.trim() !== '' ? v : 'Not reported';
</script>

<details class="report-card" data-evidence={`event:${ref.event_id}`} tabindex="-1" aria-label={`Saved report ${ref.event_id}`}>
  <summary>
    <span class="report-heading"><span class="mono report-id">{ref.event_id}</span><span class="pill">{ref.event_type}</span></span>
    <span class="report-time">Reported <time datetime={ref.occurred_at} title={reported.utc}>{reported.local}</time></span>
  </summary>
  <div class="report-body">
    <p class="section-note">Reported scan target: <span class="mono">{ref.target || 'Not reported'}</span>{#if ref.target_truncated} (truncated){/if}<br />Received <time datetime={ref.received_at} title={received.utc}>{received.local}</time></p>
    {#if views.length === 0}
      <p>No retained observation for this event. Its reference is retained, but a saved scanner view is unavailable.</p>
    {:else}
      {#each views as view (`${view.event_id}:${view.view_role}`)}
        {@const attrs = view.attributes ?? {}}
        <section class="report-view" aria-label={`${view.view_role} view`}>
          <h4>{view.view_role === 'previous' ? 'Before the reported change' : view.view_role === 'current' ? 'After the reported change' : 'Reported host'}</h4>
          <dl class="report-identity">
            <div><dt>Hostname</dt><dd>{value(attrs.hostname)}</dd></div>
            <div><dt>IP address</dt><dd class="mono">{value(attrs.ip)}</dd></div>
            <div><dt>MAC address</dt><dd class="mono">{value(attrs.mac)}</dd></div>
            {#if attrs.vendor}<div><dt>Reported vendor</dt><dd>{attrs.vendor}</dd></div>{/if}
            {#if attrs.os}<div><dt>Reported OS</dt><dd>{attrs.os}</dd></div>{/if}
            {#if attrs.device}<div><dt>Reported device</dt><dd>{attrs.device}</dd></div>{/if}
          </dl>
          <p class="confidence-note"><span class="pill">Scanner-reported identity confidence: {attrs.identity_confidence || 'unknown'}</span><span class="section-note">Identity source: {attrs.identity_source || 'unknown'}</span></p>
          <p class="section-note">{randomizedLabel(attrs.randomized)}</p>
          {#if attrs.attributes_truncated}<p class="section-note">Some reported fields were truncated at ingest.</p>{/if}
          <details class="attributes"><summary>Saved report view · technical attributes</summary><pre>{JSON.stringify(attrs, null, 2)}</pre></details>
          <p class="section-note">Snapshot covers {view.ports_total} ports{view.ports_truncated ? ' (truncated)' : ''}.</p>
          {#if (view.ports ?? []).length > 0}
            <ul>{#each view.ports as port (`${port.proto}/${port.port}`)}<li>{port.proto}/{port.port} {port.state}{port.service ? ` (${port.service})` : ''}</li>{/each}</ul>
          {/if}
          {#if view.profile_id}
            <a class="profile-chip" href={`#/devices/${encodeURIComponent(view.profile_id)}`}><span>Open reported context →</span><span class="section-note">{classLabel(view.reference_class)} · {profileLinks.find((link) => link.profile_id === view.profile_id)?.role ?? view.view_role}</span></a>
          {:else}<p class="section-note">No linked profile is available for this saved view.</p>{/if}
        </section>
      {/each}
    {/if}
  </div>
</details>

<style>
  .report-card { border: 1px solid var(--border); border-radius: 11px; padding: 0 16px; margin: 12px 0 0; background: var(--surface); overflow-wrap: anywhere; }
  summary { padding: 16px 0; }
  .report-heading { display: inline-flex; flex-wrap: wrap; align-items: center; gap: 8px; max-width: 100%; vertical-align: top; }
  .report-id { font-weight: 600; }
  .report-time { display: block; font-size: 12px; font-weight: 400; color: var(--muted); margin: 7px 0 0 16px; }
  .report-body { padding: 16px 0 6px; border-top: 1px solid var(--border); }
  .report-view { border-top: 1px solid var(--border); padding: 16px 0 0; margin: 16px 0; }
  .report-view h4 { font-size: 14px; }
  .report-view .report-identity { display: grid; grid-template-columns: repeat(auto-fit,minmax(min(100%,180px),1fr)); gap: 14px; margin: 0 0 16px; }
  .report-identity dt { font-size: 11px; text-transform: uppercase; letter-spacing: .05em; margin-bottom: 4px; }
  .report-identity dd { margin: 0; }
  .confidence-note { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
  .attributes { padding: 0; border: 0; }
  .attributes summary { font-size: 12px; color: var(--accent); }
  .profile-chip { display: flex; flex-wrap: wrap; gap: 5px 12px; border: 1px solid var(--border); background: var(--accent-soft); border-radius: 9px; padding: 10px 12px; text-decoration: none; font-size: 13px; }
  /* .mono now lives in styles/utilities.css (single definition). */
</style>
