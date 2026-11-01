<script>
  import { onDestroy, onMount } from 'svelte';
  import { fetchDevice } from '../api.js';
  import { createFetchGuard } from './freshness.js';
  import { displayTime, classLabel } from './format.js';

  // Reported profile (Designv9 §8.2): upstream facts, provenance, last
  // report, ports in retained events and independently paged history.
  // The history cutoff and any purge boundary come from retention
  // policy on the Health screen — this page never claims a purge from
  // an empty page. Accumulated history rows are capped.
  const HISTORY_CAP = 200;

  // initial carries a prerender snapshot of the profile envelope;
  // history paging continues from its cursor through the API.
  let { id, stream, initial = null } = $props();

  const guard = createFetchGuard();
  onDestroy(() => guard.invalidate());

  let fetched = $state(null);
  let extraHistory = $state([]);
  let historyCursor = $state(/** @type {string|null} */ (null));
  let retired = $state(false);
  let historyError = $state(null);
  let error = $state(null);
  let stale = $state(false);

  // Rendered profile: snapshot until the fetch completes. History is
  // snapshot rows plus fetched pages; a reload retires the snapshot
  // so rows never duplicate.
  const profile = $derived(fetched ?? initial);
  const snapHistory = $derived(retired ? [] : (initial?.history ?? []));
  const history = $derived([...snapHistory, ...extraHistory]);
  const historyAfter = $derived(historyCursor ?? (retired ? '' : (initial?.history_next_after ?? '')));
  const historyExhausted = $derived(profile !== null && historyAfter === '');
  const historyCapped = $derived(!historyExhausted && history.length >= HISTORY_CAP);

  // A reconnect edge marks the page stale without resetting history
  // paging; the operator keeps position and reloads explicitly.
  let wasLive = false;
  $effect(() => {
    const live = stream === 'live';
    const edge = live && !wasLive && profile !== null;
    wasLive = live;
    if (edge) {
      stale = true;
    }
  });

  async function load() {
    guard.invalidate();
    const token = guard.start();
    error = null;
    stale = false;
    retired = true;
    extraHistory = [];
    historyCursor = null;
    try {
      const fresh = await fetchDevice(id);
      if (!guard.current(token)) {
        return;
      }
      fetched = fresh;
      historyError = null;
    } catch (err) {
      if (guard.current(token) && profile === null) {
        error = err instanceof Error ? err.message : String(err);
      }
    }
  }

  async function older() {
    const token = guard.start();
    const cursor = historyAfter;
    historyError = null;
    try {
      const page = await fetchDevice(id, cursor);
      if (!guard.current(token)) {
        return;
      }
      const room = HISTORY_CAP - history.length;
      extraHistory = [...extraHistory, ...page.history.slice(0, Math.max(room, 0))];
      historyCursor = page.history_next_after ?? '';
    } catch (err) {
      if (guard.current(token)) {
        historyError = err instanceof Error ? err.message : String(err);
      }
    }
  }

  onMount(() => {
    if (initial === null) {
      load();
    }
  });

  const first = $derived(profile === null ? null : displayTime(profile.first_report_at));
  const last = $derived(profile === null ? null : displayTime(profile.last_report_at));
</script>

{#if error !== null && profile === null}
  <section aria-label="Reported host unavailable">
    <h2>Reported host unavailable</h2>
    <p>The bridge did not return this profile ({error}).</p>
    <p><a href="#/devices">Back to reported hosts</a> <button type="button" onclick={load}>Retry</button></p>
  </section>
{:else if profile === null}
  <p role="status">Loading reported host…</p>
{:else}
  {@const latestTime = profile.latest === null ? null : displayTime(profile.latest.reported_at)}
  <article aria-label="Reported host detail">
    <p><a href="#/devices">Back to reported hosts</a></p>
    {#if stale}
      <p role="status">Connection recovered — this page may miss new reports. <button type="button" onclick={load}>Reload</button></p>
    {/if}
    <h2>{profile.profile_id} — {classLabel(profile.class)}</h2>
    {#if profile.class === 'legacy'}
      <p role="note">Earlier bridge-inferred association; not used for current identity.</p>
    {:else if profile.class === 'address' || profile.class === 'event'}
      <p>This page describes {profile.class === 'address' ? 'address context' : 'unresolved events'}, not a device.</p>
    {/if}
    <dl>
      <dt>Source scope</dt>
      <dd>{profile.source_scope}</dd>
      <dt>Upstream reference</dt>
      <dd>{profile.reported_id}</dd>
      <dt>Reported by</dt>
      <dd>{profile.reported_source} (confidence: {profile.reported_confidence || 'unknown'})</dd>
      <dt>Association</dt>
      <dd>{profile.association_status}</dd>
      {#if profile.operator_label !== null && profile.operator_label !== undefined}
        <dt>Your label</dt>
        <dd>{profile.operator_label}</dd>
      {/if}
      <dt>First report</dt>
      <dd><time datetime={profile.first_report_at} title={first?.utc}>{first?.local}</time></dd>
      <dt>Last report</dt>
      <dd><time datetime={profile.last_report_at} title={last?.utc}>{last?.local}</time></dd>
    </dl>

    <section aria-label="Latest evidence">
      <h3>Latest evidence</h3>
      {#if profile.latest === null}
        <p>No retained observation.</p>
      {:else}
        <p>Reported <time datetime={profile.latest.reported_at} title={latestTime?.utc}>{latestTime?.local}</time> ({profile.latest.view_role}).</p>
        <dl>
          {#each Object.entries(profile.latest.attributes ?? {}) as [key, value] (key)}
            <dt>{key}</dt>
            <dd>{JSON.stringify(value)}</dd>
          {/each}
        </dl>
        {#if (profile.latest.ports ?? []).length > 0}
          <ul>
            {#each profile.latest.ports as port (`${port.proto}/${port.port}`)}
              <li>{port.proto}/{port.port} {port.state}{port.service ? ` (${port.service})` : ''}</li>
            {/each}
          </ul>
        {/if}
        <p>Snapshot covers {profile.latest.ports_total} ports{profile.latest.ports_truncated ? ' (truncated)' : ''}.</p>
      {/if}
    </section>

    <section aria-label="History">
      <h3>History</h3>
      {#if history.length === 0}
        <p>No retained reports in this window.</p>
      {:else}
        <ol>
          {#each history as obs (obs.observation_id)}
            {@const at = displayTime(obs.reported_at)}
            <li>
              {obs.event_id} ({obs.view_role}) —
              <time datetime={obs.reported_at} title={at.utc}>{at.local}</time>,
              {obs.ports_total} ports{obs.ports_truncated ? ' (truncated)' : ''}
            </li>
          {/each}
        </ol>
      {/if}
      {#if historyError !== null}
        <p role="alert">Older page failed ({historyError}). <button type="button" onclick={older}>Retry</button></p>
      {/if}
      {#if historyCapped}
        <p>History capped at {HISTORY_CAP} rows; use the API for the full set.</p>
      {:else if !historyExhausted}
        <button type="button" onclick={older}>Show older reports</button>
      {:else}
        <p>End of retained history. Retention policy lives on the <a href="#/health">Health</a> screen.</p>
      {/if}
    </section>
  </article>
{/if}

<style>
  dl {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 0.25rem 1rem;
  }
  dt {
    font-weight: 600;
  }
  dd {
    margin: 0;
    overflow-wrap: anywhere;
  }
</style>
