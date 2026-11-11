<script>
  import { onDestroy, onMount } from 'svelte';
  import { fetchIncident } from '@/api.js';
  import { getConnectionOrNull } from '@/lib/events/context.js';

  // Incident members container: fetches and caches the member briefings
  // for one expanded incident row. Rendered through AttentionList's
  // `members` snippet, so the list itself stays pure. The cache clears
  // on update/reconnect/resync while the row stays open, and a fetch
  // failure surfaces on the row, never as navigation. Collapsed rows
  // render nothing but stay mounted, so reopening is instant until the
  // next invalidation.
  let { incidentId, expanded = false } = $props();

  let members = $state(undefined);
  let loadError = $state(undefined);
  let loading = $state(false);
  let alive = true;
  let generation = 0;

  // Live updates arrive through the shared connection's incident
  // revision; standalone renders without a provider keep the legacy
  // window subscription as a fallback.
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
      refreshMembers();
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
    if (!expanded) {
      return;
    }
    // Clear first like the old list did, so a slow reload reads as
    // loading instead of showing rows from before the invalidation.
    members = undefined;
    loadError = undefined;
    loadMembers();
  }

  onDestroy(() => {
    alive = false;
  });

  $effect(() => {
    if (expanded && alive && members === undefined && loadError === undefined && !loading) {
      loadMembers();
    }
  });

  async function loadMembers() {
    const token = ++generation;
    loadError = undefined;
    loading = true;
    try {
      const detail = await fetchIncident(incidentId);
      if (alive && token === generation) {
        members = detail.members;
      }
    } catch (err) {
      if (alive && token === generation) {
        loadError = err instanceof Error ? err.message : String(err);
      }
    } finally {
      if (alive && token === generation) {
        loading = false;
      }
    }
  }
</script>

{#if expanded}
  {#if loading && members === undefined}
    <p role="status">Loading members…</p>
  {:else if loadError !== undefined}
    <p role="alert">{loadError}</p>
  {:else if members !== undefined}
    <ol>
      {#each members as m (m.briefing_id)}
        <li>
          <a href={`#/briefing/${m.briefing_id}`}>{m.briefing_id}</a>
          <span class="pill">{m.priority}</span>
        </li>
      {/each}
    </ol>
  {/if}
{/if}
