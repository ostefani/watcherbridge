<script>
  import Notice from '@/components/Notice/Notice.svelte';
  import { onMount } from 'svelte';
  import { watchLive } from '@/lib/stream.svelte.js';
  import { fetchDevices } from '@/api.js';
  import { createPager } from '@/lib/pager.svelte.js';
  import { getConnectionOrNull } from '@/lib/context.js';
  import DeviceList from '@/components/DeviceList/DeviceList.svelte';

  // Devices container: owns paging, snapshot seeding and the reconnect
  // edge; rendering belongs to DeviceList/DeviceRow below.
  // `initial` carries a prerender snapshot {devices, next_after}.
  // A failed (re)load keeps existing rows with a notice and Retry; only
  // a data-less failure becomes an error. A manual reload retires the
  // snapshot so rows never duplicate. The reconnect edge marks the list
  // stale without resetting paging: the operator keeps position and
  // reloads explicitly. Liveness comes from the shared connection
  // (context), never a drilled prop; standalone renders simply never
  // observe a live edge.
  let { initial = null } = $props();

  const connection = getConnectionOrNull();

  const ROW_CAP = 200;

  const pager = createPager({
    fetchPage: async (cursor) => {
      const page = await fetchDevices(cursor);
      return { rows: page.devices, nextAfter: page.next_after ?? '' };
    },
    keyOf: (dev) => dev.profile_id,
    cap: ROW_CAP,
    // Thunk: the prop is init-only (parents pass it once), but reading
    // it at component top level trips the init-capture rule.
    initial: () => (initial ? { rows: initial.devices ?? [], nextAfter: initial.next_after ?? '' } : null)
  });
  const empty = $derived(pager.rows.length === 0);
  const loading = $derived(!pager.started && empty && pager.error === null);

  let stale = $state(false);

  async function reload() {
    const status = await pager.reset();
    if (status === 'applied') {
      stale = false;
    }
  }

  async function page() {
    await pager.loadMore();
  }

  function retry() {
    if (pager.failure?.reset === true) {
      reload();
    } else if (pager.failure?.reset === false) {
      page();
    }
  }

  onMount(() => {
    if (initial === null) {
      reload();
    }
    return pager.invalidate;
  });

  watchLive(connection, () => {
    if (pager.rows.length > 0) {
      stale = true;
    }
  });
</script>

<section aria-label="Reported hosts">
  <h2>Reported hosts</h2>
  <p class="section-note">Scanner-reported subjects. Classes distinguish device references, address context, unresolved events and legacy profiles.</p>
  {#if loading}
    <Notice>Loading reported hosts…</Notice>
  {:else if pager.error !== null && empty}
    <Notice kind="alert">The bridge did not return reported hosts ({pager.error}).</Notice>
    <button type="button" onclick={retry}>Retry</button>
  {:else}
    <DeviceList
      devices={pager.rows}
      {stale}
      error={pager.error}
      exhausted={pager.exhausted}
      capped={pager.capped}
      cap={ROW_CAP}
      onReload={reload}
      onLoadMore={page}
      onRetry={retry}
    />
  {/if}
</section>
