<script>
  import { sourceLabel } from '@/lib/format.js';
  import CopyValue from '@/components/CopyValue/CopyValue.svelte';

  // Single-view identity grid: reported host, addresses, scan target,
  // time range and analysis source. Partial evidence falls back to
  // per-report views with an explicit notice — never a guessed link.
  let {
    identity = null,
    identityComplete = false,
    primaryEvent = null,
    eventCount = 0,
    range = null,
    referencedTruncated = false,
    source
  } = $props();
</script>

<dl class="identity-grid">
  {#if identity !== null && identityComplete}
    <div><dt>Reported host</dt><dd>{identity.hostname ?? 'Not reported'}</dd></div>
    <div><dt>IP address</dt><dd class="mono">{identity.ip ?? 'Not reported'}{#if identity.ip !== null}<CopyValue value={identity.ip} name="IP address" />{/if}</dd></div>
    <div><dt>MAC address</dt><dd class="mono">{identity.mac ?? 'Not reported'}{#if identity.mac !== null}<CopyValue value={identity.mac} name="MAC address" />{/if}</dd></div>
  {:else}
    <div class="identity-group"><dt>Reported identities</dt><dd>See the individual report views below<p class="section-note">{identityComplete ? 'Grouped changes can describe different host views.' : 'Only part of the retained evidence is shown.'}</p></dd></div>
  {/if}
  <div><dt>Reported scan target</dt><dd class="mono">{primaryEvent?.target || (eventCount > 1 ? 'Shown per report below' : 'Not reported')}{#if primaryEvent?.target_truncated}<span class="section-note"> (truncated)</span>{/if}</dd></div>
  <div><dt>{range?.partial || referencedTruncated ? 'Available report times' : eventCount > 1 ? 'Report time range' : 'Report time'}</dt><dd>{#if range !== null}<span title={range.utc}>{range.local}</span>{:else}Not reported{/if}</dd></div>
  <div><dt>Analysis source</dt><dd>{sourceLabel(source)}</dd></div>
</dl>
