<script>
  import { onDestroy, onMount } from 'svelte';
  import { fetchIncident } from '../api.js';
  import { createFetchGuard } from './freshness.js';
  import { displayTime, sourceLabel } from './format.js';
  import DecisionPanel from './DecisionPanel.svelte';

  // Presentation incident detail (Planv4 Step 29): one burst bundle
  // with its member evidence, qualification counts and decisions.
  // Member priorities and references are accept-time snapshots; the
  // incident never rewrites member briefings. Text renders as text,
  // never HTML, so hostile scanner strings stay inert.
  // initial carries a server snapshot for prerender/tests: when set
  // the screen renders it without fetching, exactly as a completion
  // would. Production mounts omit it and fetch.
  let { id, initial = null } = $props();

  const guard = createFetchGuard();
  onDestroy(() => {
    guard.invalidate();
    // Server rendering runs destroy callbacks without a window.
    if (typeof window !== 'undefined') {
      window.removeEventListener('watcherbridge:incident', reload);
    }
  });

  let detail = $state(null);
  let error = $state(null);

  // Rendered incident: fetched completion wins, snapshot fills in
  // until (or instead of) the fetch. Reads the prop inside a
  // derived, never as state, so updates stay explicit.
  const shown = $derived(detail ?? initial);

  async function load() {
    guard.invalidate();
    const token = guard.start();
    error = null;
    try {
      const fresh = await fetchIncident(id);
      if (guard.current(token)) {
        detail = fresh;
      }
    } catch (err) {
      if (guard.current(token) && detail === null) {
        error = err instanceof Error ? err.message : String(err);
      }
    }
  }

  function reload() {
    load();
  }

  // Live updates arrive as window events: App forwards `incident`
  // SSE frames here because incident updates carry no SSE id and
  // never replay through the briefing stream. Reconnect recovery is
  // the overview refetch (attention stays current); like the
  // briefing detail, this screen does not refetch on reconnect
  // itself — the next incident frame or navigation reloads it.
  onMount(() => {
    if (initial === null) {
      load();
    }
    window.addEventListener('watcherbridge:incident', reload);
  });

  const KIND_LABEL = {
    burst_appearance: 'Burst appearance',
    burst_disappearance: 'Burst disappearance'
  };

  const INCIDENT_KIND_HELP = {
    expected: 'Mark reviewed: this incident needs no further action. Members stay untouched unless included below.',
    not_expected: 'Flag for follow-up: this incident still needs a check.',
    snooze: 'Hide until a time. Empty keeps the server default (24h).',
    note: 'Free-text note, kept with the audit (max 2000 characters).'
  };

  function countLine(/** @type {import('../wire.js').Incident} */ inc) {
    const reports = `${inc.member_count} member report${inc.member_count === 1 ? '' : 's'}`;
    const refs = `${inc.qualified_refs} qualified upstream reference${inc.qualified_refs === 1 ? '' : 's'}`;
    return `${reports} · ${refs}`;
  }
</script>

<p><a href="#/">Back to overview</a></p>

{#if error !== null}
  <p role="alert">{error}</p>
{:else if shown === null}
  <p>Loading incident…</p>
{:else}
  {@const inc = shown.incident}
  <h2>{KIND_LABEL[inc.kind] ?? inc.kind}</h2>
  <p>
    <span class="pill">{inc.status}</span>
    <span class="pill">Floor: {inc.floor}</span>
    {#if inc.status === 'closed' && inc.closed_at}
      <span>Closed {displayTime(inc.closed_at)}</span>
    {:else}
      <span>Open since {displayTime(inc.opened_at)}</span>
    {/if}
  </p>

  <section aria-label="Scope">
    <h3>Scope</h3>
    <p>Source: {sourceLabel(inc.source_scope)}</p>
    <p>Target: {inc.target}</p>
    <p>Window start: {displayTime(inc.window_start)} · Last member: {displayTime(inc.last_member_at)}</p>
  </section>

  <section aria-label="Evidence">
    <h3>Evidence</h3>
    <p>{countLine(inc)}</p>
    {#if inc.qualified_refs < inc.member_count}
      <p>Some members share identity evidence; only distinct qualified references count toward the burst threshold.</p>
    {/if}
    {#if shown.truncated}
      <p>More members exist than shown.</p>
    {/if}
    {#if shown.members.length === 0}
      <p>No member evidence retained.</p>
    {:else}
      <ol>
        {#each shown.members as m (m.briefing_id)}
          <li>
            <a href={`#/briefing/${m.briefing_id}`}>{m.briefing_id}</a>
            <span class="pill">{m.priority}</span>
            <span>{displayTime(m.created_at)}</span>
            <span>{m.refs.length} qualified reference{m.refs.length === 1 ? '' : 's'}</span>
          </li>
        {/each}
      </ol>
    {/if}
  </section>

  <DecisionPanel
    subjectKind="incident"
    subjectId={inc.incident_id}
    kinds={['expected', 'not_expected', 'snooze', 'note']}
    decisions={shown.decisions ?? []}
    effective={shown.effective_review ?? null}
    memberScope
    memberCount={shown.members.length}
    kindHelp={INCIDENT_KIND_HELP}
    reload={load}
  />
{/if}

<style>
  .pill {
    border: 1px solid #999;
    padding: 0 0.4rem;
    font-size: 0.8rem;
    margin-right: 0.4rem;
  }
</style>
