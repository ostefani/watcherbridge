<script>
  import Notice from '@/components/Notice/Notice.svelte';
  import DeviceRow from '@/components/DeviceRow/DeviceRow.svelte';

  // Pure list rendering: rows and list states in, intents out. The
  // container owns fetching, snapshot seeding and the reconnect edge.
  // Pages merge by profile ID with first-seen positions: the backend
  // orders by mutable last_report_at, so a profile gaining another
  // report can resurface on a later page.
  let {
    devices = [],
    stale = false,
    error = null,
    exhausted = false,
    capped = false,
    cap = 200,
    onReload = () => {},
    onLoadMore = () => {},
    onRetry = () => {}
  } = $props();
</script>

{#if stale}
  <Notice actionLabel="Reload" onAction={onReload}>Connection recovered — this list may miss new reports.</Notice>
{/if}
{#if devices.length === 0}
  <p>No reported subjects retained.</p>
{:else}
  <ul class="host-list" data-rows>
    {#each devices as dev (dev.profile_id)}
      <DeviceRow {dev} />
    {/each}
  </ul>
{/if}
{#if error !== null}
  <Notice kind="alert" actionLabel="Retry" onAction={onRetry}>Page failed ({error}) — earlier rows kept.</Notice>
{/if}
{#if capped}
  <p>List capped at {cap} rows; use the API for the full set.</p>
{:else if !exhausted}
  <button type="button" onclick={onLoadMore}>Load more</button>
{:else}
  <p>End of retained list.</p>
{/if}
