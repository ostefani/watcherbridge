<script>
  import { displayTime } from '@/lib/format.js';

  // Latest retained observation with its attribute snapshot and port
  // summary. An absent observation is explicit, never a gap claim.
  let { latest } = $props();
  const at = $derived(latest === null ? null : displayTime(latest.reported_at));
</script>

<section aria-label="Latest evidence">
  <h3>Latest evidence</h3>
  {#if latest === null}
    <p>No retained observation.</p>
  {:else}
    <p>Reported <time datetime={latest.reported_at} title={at?.utc}>{at?.local}</time> ({latest.view_role}).</p>
    <dl>
      {#each Object.entries(latest.attributes ?? {}) as [key, value] (key)}
        <dt>{key}</dt>
        <dd>{JSON.stringify(value)}</dd>
      {/each}
    </dl>
    {#if (latest.ports ?? []).length > 0}
      <ul>
        {#each latest.ports as port (`${port.proto}/${port.port}`)}
          <li>{port.proto}/{port.port} {port.state}{port.service ? ` (${port.service})` : ''}</li>
        {/each}
      </ul>
    {/if}
    <p>Snapshot covers {latest.ports_total} ports{latest.ports_truncated ? ' (truncated)' : ''}.</p>
  {/if}
</section>
