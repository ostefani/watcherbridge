<script>
  import { onMount } from 'svelte';
  import { fetchHealth } from '@/api.js';
  import { createResource, failureNotice } from '@/lib/async/resource.js';
  import { shouldRefreshOnReconnect } from '@/lib/events/reconnect.js';
  import { getConnectionOrNull } from '@/lib/events/context.js';
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

  // Snapshot-first rendering: fetched data wins once it lands, but the
  // mount-time snapshot fills in until (or instead of) the fetch. Reads
  // stay reactive so SSR and the compiler's init-capture rule hold;
  // parents pass `initial` once and never rewrite it.
  const resource = createResource(fetchHealth);
  const { data, error, refreshError } = resource;
  const shown = $derived($data ?? initial);
  // A failed refresh on a snapshot screen must surface: the resource
  // only knows fetch state, so the snapshot-aware notice lives here.
  const notice = $derived(failureNotice({ data: $data, initial, error: $error, refreshError: $refreshError }));

  onMount(() => {
    if (initial === null) {
      resource.refresh();
    }
  });

  // Refresh live health on the reconnect edge — including snapshots.
  // A mounted snapshot is renderable state, not a reason to stay
  // stale (was BUG-3: the edge required fetched data to exist).
  let wasLive = false;
  $effect(() => {
    if (!connection) {
      return;
    }
    return connection.connection.subscribe((state) => {
      const live = state === 'live';
      const edge = shouldRefreshOnReconnect({ live, wasLive, data: $data, initial });
      wasLive = live;
      if (edge) {
        resource.refresh();
      }
    });
  });
</script>

<section aria-label="Health">
  <h2>Health</h2>
  <p class="section-note">Source coverage, processing, and retention. Liveness describes the source, not host presence.</p>
  {#if $error !== null && shown === null}
    <p role="alert">The bridge did not return health ({$error}).</p>
    <button type="button" onclick={() => resource.refresh()}>Retry</button>
  {:else if shown === null}
    <p role="status">Loading health…</p>
  {:else}
    <HealthSummary health={shown} refreshError={notice} onRetry={() => resource.refresh()} />
    <SourceList sources={shown.sources} />
    <PipelineDetails process={shown.process} />
    <PurgeDetails purge={shown.purge} retention={shown.retention} />
    {#if shown.publisher}
      <PublisherDetails publisher={shown.publisher} />
    {/if}
  {/if}
</section>
