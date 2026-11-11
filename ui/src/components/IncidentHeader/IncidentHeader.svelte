<script>
  import { displayTime } from '@/lib/format.js';

  // Incident identity: kind, lifecycle status, priority floor and the
  // open/closed timestamp. Status wording is the list state, never a
  // claim about the upstream scanner.
  let { incident } = $props();
  const opened = $derived(displayTime(incident.opened_at));
  const closed = $derived(displayTime(incident.closed_at));

  const KIND_LABEL = {
    burst_appearance: 'Burst appearance',
    burst_disappearance: 'Burst disappearance'
  };
</script>

<h2>{KIND_LABEL[incident.kind] ?? incident.kind}</h2>
<p>
  <span class="pill">{incident.status}</span>
  <span class="pill">Floor: {incident.floor}</span>
  {#if incident.status === 'closed' && incident.closed_at}
    <span>Closed <time datetime={incident.closed_at} title={closed.utc}>{closed.local}</time></span>
  {:else}
    <span>Open since <time datetime={incident.opened_at} title={opened.utc}>{opened.local}</time></span>
  {/if}
</p>
