<script>
  import TimeStamp from '@/components/TimeStamp/TimeStamp.svelte';

  // Latest retained observation with its attribute snapshot and port
  // summary. An absent observation is explicit, never a gap claim.
  let { latest } = $props();
</script>

<section aria-label="Latest evidence">
  <h3>Latest evidence</h3>
  {#if latest === null}
    <p>No retained observation.</p>
  {:else}
    <p>Reported <TimeStamp iso={latest.reported_at} /> ({latest.view_role}).</p>
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
