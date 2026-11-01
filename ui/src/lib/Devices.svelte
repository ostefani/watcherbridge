<script>
  import { onDestroy, onMount } from 'svelte';
  import { fetchDevices } from '../api.js';
  import { createFetchGuard } from './freshness.js';
  import { nextPageRows } from './paging.js';
  import { displayTime, classLabel } from './format.js';

  // Reported hosts list (Designv9 §8.2): oldest-first pages appended
  // through the tuple cursor. Accumulated rows are capped so an
  // unbounded history cannot grow the browser heap without limit.
  const ROW_CAP = 200;

  // initial carries a prerender snapshot {devices, next_after}; paging
  // continues from its cursor through the API.
  let { stream, initial = null } = $props();

  const guard = createFetchGuard();
  onDestroy(() => guard.invalidate());

  let extra = $state([]);
  let cursorOverride = $state(/** @type {string|null} */ (null));
  let retired = $state(false);
  let error = $state(null);
  let loading = $state(true);
  let stale = $state(false);
  let resetting = $state(false);

  // Rendered list: snapshot rows first, fetched pages appended. A
  // manual reload retires the snapshot so rows never duplicate. The
  // cursor prefers an explicit override, then the snapshot cursor.
  const base = $derived(retired ? [] : (initial?.devices ?? []));
  const devices = $derived([...base, ...extra]);
  const nextAfter = $derived(cursorOverride ?? (retired ? '' : (initial?.next_after ?? '')));
  // The loading branch renders first, so an empty cursor means the end
  // of the list here rather than a load in flight.
  const exhausted = $derived(nextAfter === '');
  const capped = $derived(!exhausted && devices.length >= ROW_CAP);

  // A reconnect edge marks the list stale without resetting paging:
  // the operator keeps position and reloads explicitly.
  let wasLive = false;
  $effect(() => {
    const live = stream === 'live';
    const edge = live && !wasLive && devices.length > 0;
    wasLive = live;
    if (edge) {
      stale = true;
    }
  });

  function reload() {
    loading = true;
    resetting = true;
    loadMore(true);
  }

  // Pages merge by profile ID with first-seen positions: the
  // backend orders by mutable last_report_at, so a profile gaining
  // another report can resurface on a later page. A failed (re)load
  // keeps existing rows with a notice and Retry; the snapshot
  // retires only on success, so a failure never empties the list.
  async function loadMore(reset = false) {
    const token = guard.start();
    const cursor = reset ? '' : nextAfter;
    error = null;
    try {
      const page = await fetchDevices(cursor);
      if (!guard.current(token)) {
        return;
      }
      const rows = nextPageRows(reset ? [] : devices, page.devices, (d) => d.profile_id, ROW_CAP);
      if (reset) {
        // Only a reset refreshes existing rows, so only it clears
        // the reconnect warning; paging keeps first-seen rows and
        // leaves the notice in place.
        retired = true;
        extra = rows;
        stale = false;
      } else {
        extra = [...extra, ...rows];
      }
      cursorOverride = page.next_after ?? '';
      loading = false;
      resetting = false;
    } catch (err) {
      if (guard.current(token)) {
        error = err instanceof Error ? err.message : String(err);
        loading = false;
      }
    }
  }

  // One initial page per mount unless a snapshot was provided;
  // paging is explicit through the cursor button, so state updates
  // below never refetch on their own. The prop is read inside this
  // closure, never as state, so snapshots stay explicit.
  onMount(() => {
    if (initial === null) {
      loadMore();
    } else {
      loading = false;
    }
  });
</script>

<section aria-label="Reported hosts">
  <h2>Reported hosts</h2>
  <p>Scanner-reported subjects. Classes distinguish device references, address context, unresolved events and legacy profiles.</p>
  {#if loading && devices.length === 0}
    <p role="status">Loading reported hosts…</p>
  {:else if error !== null && devices.length === 0}
    <p role="alert">The bridge did not return reported hosts ({error}).</p>
    <button type="button" onclick={() => loadMore()}>Retry</button>
  {:else}
    {#if stale}
      <p role="status">Connection recovered — this list may miss new reports. <button type="button" onclick={reload}>Reload</button></p>
    {/if}
    {#if devices.length === 0}
      <p>No reported subjects retained.</p>
    {:else}
      <ul data-rows>
        {#each devices as dev (dev.profile_id)}
          {@const last = displayTime(dev.last_report_at)}
          <li>
            <a data-row href={`#/devices/${dev.profile_id}`}>
              {dev.profile_id} — {classLabel(dev.class)}, last report {last.local}
            </a>
          </li>
        {/each}
      </ul>
    {/if}
    {#if error !== null}
      <p role="alert">Page failed ({error}) — earlier rows kept. <button type="button" onclick={() => loadMore(resetting)}>Retry</button></p>
    {/if}
    {#if capped}
      <p>List capped at {ROW_CAP} rows; use the API for the full set.</p>
    {:else if !exhausted}
      <button type="button" onclick={() => loadMore()}>Load more</button>
    {:else}
      <p>End of retained list.</p>
    {/if}
  {/if}
</section>
