<script>
  import Notice from '@/components/Notice/Notice.svelte';
  import { onMount, untrack } from 'svelte';
  import { fetchDevice } from '@/api.js';
  import { getConnectionOrNull } from '@/lib/context.js';
  import { createResource } from '@/lib/resource.svelte.js';
  import { createPager } from '@/lib/pager.svelte.js';
  import DeviceProfile from '@/components/DeviceProfile/DeviceProfile.svelte';
  import LatestEvidence from '@/components/LatestEvidence/LatestEvidence.svelte';
  import HistoryList from '@/components/HistoryList/HistoryList.svelte';
  // Shared review widget (plan step 33): screens embed DecisionsContainer
  // with subject props; page containers stay App-composed (see CONVENTIONS).
  // eslint-disable-next-line no-restricted-imports -- shared-widget exception above
  import DecisionsContainer from '@/containers/DecisionsContainer/DecisionsContainer.svelte';

  // Device container: owns the profile fetch, history paging, snapshot
  // seeding and the reconnect edge; rendering belongs to the components
  // below. Operator review lives in DecisionsContainer, which notifies
  // this container through ondecided so it owns its own refetching.
  // `initial` carries a prerender snapshot of the profile envelope;
  // history paging continues from its cursor through the API. A failed
  // refresh keeps existing data with a notice and Retry; the snapshot
  // retires and history restarts only on success, so a failure never
  // empties the page.
  let { id, initial = null } = $props();

  // Liveness comes from the shared connection (context), never a
  // drilled prop; standalone renders simply never observe a live edge.
  const connection = getConnectionOrNull();

  const HISTORY_CAP = 200;

  const profileResource = createResource(() => fetchDevice(id), () => initial);
  const profile = $derived(profileResource.shown);
  const notice = $derived(profileResource.notice);

  const historyPager = createPager({
    fetchPage: async (cursor) => {
      const page = await fetchDevice(id, cursor);
      return { rows: page.history ?? [], nextAfter: page.history_next_after ?? '' };
    },
    keyOf: (obs) => obs.observation_id,
    cap: HISTORY_CAP,
    // Thunk: the prop is init-only (parents pass it once), but reading
    // it at component top level trips the init-capture rule.
    initial: () => (initial ? { rows: initial.history ?? [], nextAfter: initial.history_next_after ?? '' } : null)
  });

  let stale = $state(false);

  async function load() {
    const status = await profileResource.refresh();
    if (status === 'applied') {
      const fresh = profileResource.data;
      historyPager.replace(fresh?.history ?? [], fresh?.history_next_after ?? '');
      stale = false;
    }
  }

  async function older() {
    await historyPager.loadMore();
  }

  onMount(() => {
    if (initial === null) {
      load();
    }
    return () => {
      profileResource.invalidate();
      historyPager.invalidate();
    };
  });

  let wasLive = false;
  $effect(() => {
    if (!connection) {
      return;
    }
    return connection.connection.subscribe((state) => untrack(() => {
      const live = state === 'live';
      const edge = live && !wasLive && profile !== null;
      wasLive = live;
      if (edge) {
        stale = true;
      }
    }));
  });
</script>

{#if profileResource.error !== null && profile === null}
  <section aria-label="Reported host unavailable">
    <h2>Reported host unavailable</h2>
    <p>The bridge did not return this profile ({profileResource.error}).</p>
    <p><a href="#/devices">Back to reported hosts</a> <button type="button" onclick={load}>Retry</button></p>
  </section>
{:else if profile === null}
  <Notice>Loading reported host…</Notice>
{:else}
  <article aria-label="Reported host detail">
    <p><a href="#/devices">Back to reported hosts</a></p>
    {#if stale}
      <Notice actionLabel="Reload" onAction={load}>Connection recovered — this page may miss new reports.</Notice>
    {/if}
    {#if notice !== null}
      <Notice kind="alert" actionLabel="Retry" onAction={load}>Refresh failed ({notice}) — showing retained data.</Notice>
    {/if}
    <DeviceProfile {profile} />
    <DecisionsContainer
      subjectKind="reported_subject"
      subjectId={profile.profile_id}
      kinds={['note', 'label']}
      decisions={profile.decisions ?? []}
      ondecided={load}
    />
    <LatestEvidence latest={profile.latest} />
    <HistoryList
      observations={historyPager.rows}
      historyError={historyPager.error}
      exhausted={historyPager.exhausted}
      capped={historyPager.capped}
      cap={HISTORY_CAP}
      onOlder={older}
      onRetry={older}
    />
  </article>
{/if}
