<script>
  import TimeStamp from '@/components/TimeStamp/TimeStamp.svelte';
  import Pill from '@/components/Pill/Pill.svelte';

  // Incident identity: kind, lifecycle status, priority floor and the
  // open/closed timestamp. Status wording is the list state, never a
  // claim about the upstream scanner.
  let { incident } = $props();

  const KIND_LABEL = {
    burst_appearance: 'Burst appearance',
    burst_disappearance: 'Burst disappearance'
  };
</script>

<h2>{KIND_LABEL[incident.kind] ?? incident.kind}</h2>
<p>
  <Pill>{incident.status}</Pill>
  <Pill>Floor: {incident.floor}</Pill>
  {#if incident.status === 'closed' && incident.closed_at}
    <span>Closed <TimeStamp iso={incident.closed_at} /></span>
  {:else}
    <span>Open since <TimeStamp iso={incident.opened_at} /></span>
  {/if}
</p>
