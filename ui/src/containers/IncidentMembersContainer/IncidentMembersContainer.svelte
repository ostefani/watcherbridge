<script>
  import Notice from '@/components/Notice/Notice.svelte';
  import Pill from '@/components/Pill/Pill.svelte';
  import { onDestroy, onMount, untrack } from 'svelte';
  import { createResource } from '@/lib/resource.svelte.js';
  import { fetchIncident } from '@/api.js';
  import { getConnectionOrNull } from '@/lib/context.js';

  // Incident members container: fetches and caches the member briefings
  // for one expanded incident row. Rendered through AttentionList's
  // `members` snippet, so the list itself stays pure. The cache clears
  // on update/reconnect/resync while the row stays open, and a fetch
  // failure surfaces on the row, never as navigation. Collapsed rows
  // render nothing but stay mounted, so reopening is instant until the
  // next invalidation.
  let { incidentId, expanded = false } = $props();

  const resource = createResource(async () => (await fetchIncident(incidentId)).members);

  const connection = getConnectionOrNull();
  const incidentRevision = connection?.incidentRevision ?? null;
  let seenRevision = -1;
  $effect(() => {
    if (!incidentRevision) {
      return;
    }
    const rev = $incidentRevision;
    if (seenRevision === -1) {
      seenRevision = rev;
      return;
    }
    if (rev !== seenRevision) {
      seenRevision = rev;
      untrack(refreshMembers);
    }
  });

  onMount(() => {
    if (incidentRevision) {
      return;
    }
    window.addEventListener('watcherbridge:incident', refreshMembers);
    return () => window.removeEventListener('watcherbridge:incident', refreshMembers);
  });

  function refreshMembers() {
    if (expanded) {
      resource.clear();
      resource.refresh();
    }
  }

  onDestroy(resource.invalidate);

  $effect(() => {
    if (expanded && resource.data === null && resource.error === null && !resource.loading) {
      untrack(resource.refresh);
    }
  });
</script>

{#if expanded}
  {#if resource.loading}
    <Notice>Loading members…</Notice>
  {:else if resource.error !== null}
    <Notice kind="alert">{resource.error}</Notice>
  {:else if resource.data !== null}
    <ol>
      {#each resource.data as m (m.briefing_id)}
        <li>
          <a href={`#/briefing/${m.briefing_id}`}>{m.briefing_id}</a>
          <Pill>{m.priority}</Pill>
        </li>
      {/each}
    </ol>
  {/if}
{/if}
