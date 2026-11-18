<script>
  import Notice from '@/components/Notice/Notice.svelte';
  import { onMount } from 'svelte';
  import { fetchHealth } from '@/api.js';
  import { createResource } from '@/lib/resource.svelte.js';
  import { watchLive } from '@/lib/stream.svelte.js';
  import { getConnectionOrNull } from '@/lib/context.js';
  import HealthSummary from '@/components/HealthSummary/HealthSummary.svelte';
  import SourceList from '@/components/SourceList/SourceList.svelte';
  import PipelineDetails from '@/components/PipelineDetails/PipelineDetails.svelte';
  import PurgeDetails from '@/components/PurgeDetails/PurgeDetails.svelte';
  import PublisherDetails from '@/components/PublisherDetails/PublisherDetails.svelte';

  // Health container: owns fetching, snapshot handling and the
  // reconnect edge; rendering belongs to the components above.
  // `initial` carries a prerender snapshot. A failed refresh keeps
  // rendered data with a notice and Retry; only a data-less failure
  // becomes an error.
  let { initial = null } = $props();

  // Liveness comes from the shared connection (context), never a
  // drilled prop; standalone renders simply never refresh on an edge.
  const connection = getConnectionOrNull();

  const resource = createResource(fetchHealth, () => initial);
  const shown = $derived(resource.shown);

  onMount(() => {
    if (initial === null) {
      resource.refresh();
    }
    return resource.invalidate;
  });

  watchLive(connection, () => {
    if (shown !== null) {
      resource.refresh();
    }
  });
</script>

<section aria-label="Health">
  <h2>Health</h2>
  <p class="section-note">Source coverage, processing, and retention. Liveness describes the source, not host presence.</p>
  {#if resource.error !== null && shown === null}
    <Notice kind="alert">The bridge did not return health ({resource.error}).</Notice>
    <button type="button" onclick={() => resource.refresh()}>Retry</button>
  {:else if shown === null}
    <Notice>Loading health…</Notice>
  {:else}
    <HealthSummary health={shown} refreshError={resource.notice} onRetry={() => resource.refresh()} />
    <SourceList sources={shown.sources} />
    <PipelineDetails process={shown.process} />
    <PurgeDetails purge={shown.purge} retention={shown.retention} />
    {#if shown.publisher}
      <PublisherDetails publisher={shown.publisher} />
    {/if}
  {/if}
</section>
