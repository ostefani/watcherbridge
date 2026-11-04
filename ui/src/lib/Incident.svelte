<script>
  import { onDestroy, onMount } from 'svelte';
  import { fetchIncident } from '../api.js';
  import { createFetchGuard } from './freshness.js';
  import { displayTime } from './format.js';
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
      if (guard.current(token)) {
        error = err instanceof Error ? err.message : String(err);
      }
    }
  }

  function reload() {
    load();
  }

  // App invalidates incident state on live frames, reconnect and resync.
  // These events have no sequence ID and cannot use briefing replay.
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
  <p role="alert">Refresh failed: {error} <button type="button" onclick={reload}>Retry</button></p>
{/if}
{#if shown === null}
  <p>Loading incident…</p>
{:else}
  {@const inc = shown.incident}
  {@const opened = displayTime(inc.opened_at)}
  {@const closed = displayTime(inc.closed_at)}
  {@const start = displayTime(inc.window_start)}
  {@const last = displayTime(inc.last_member_at)}
  <h2>{KIND_LABEL[inc.kind] ?? inc.kind}</h2>
  <p>
    <span class="pill">{inc.status}</span>
    <span class="pill">Floor: {inc.floor}</span>
    {#if inc.status === 'closed' && inc.closed_at}
      <span>Closed <time datetime={inc.closed_at} title={closed.utc}>{closed.local}</time></span>
    {:else}
      <span>Open since <time datetime={inc.opened_at} title={opened.utc}>{opened.local}</time></span>
    {/if}
  </p>

  <section aria-label="Scope">
    <h3>Scope</h3>
    <p>Source: {inc.source_scope}</p>
    <p>Target: {inc.target}</p>
    <p>Window start: <time datetime={inc.window_start} title={start.utc}>{start.local}</time> · Last member: <time datetime={inc.last_member_at} title={last.utc}>{last.local}</time></p>
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
          {@const reported = displayTime(m.created_at)}
          <li>
            <a href={`#/briefing/${m.briefing_id}`}>{m.briefing_id}</a>
            <span class="pill">{m.priority}</span>
            <time datetime={m.created_at} title={reported.utc}>{reported.local}</time>
            <span>{m.refs.length} qualified reference{m.refs.length === 1 ? '' : 's'}</span>
          </li>
        {/each}
      </ol>
    {/if}
  </section>

  {#if shown.summary}
    {@const summary = shown.summary}
    <section aria-label="Incident summary">
      <h3>Summary</h3>
      <p><span class="pill">Summary status: {summary.status}</span></p>
      {#if summary.summary}<p>{summary.summary}</p>{/if}
      {#if summary.hypothesis}<p><strong>Hypothesis:</strong> {summary.hypothesis}</p>{/if}
      {#if summary.confidence}<p>Display confidence: {summary.confidence}</p>{/if}
      <p>Priority floor: {summary.floor}. Resolved priority: {summary.final_priority}{summary.suggestion ? ` (suggested ${summary.suggestion}${summary.clamped ? ', clamped' : ''})` : ' (no model raise)'}</p>
      {#if summary.reason}<p>Priority explanation: {summary.reason}</p>{/if}
      <p>Evidence coverage: {summary.sampled} sampled member reports; {summary.omitted} omitted. The sample may not represent every member.</p>
      {#if summary.citations.length}
        <p>Cited reports:</p>
        <ul>{#each summary.citations as citation (citation.member)}<li>{citation.member}: <a href={`#/briefing/${citation.briefing_id}`}>{citation.briefing_id}</a></li>{/each}</ul>
      {/if}
      {#if summary.error}<p>{summary.error}</p>{/if}
    </section>
  {/if}

  <DecisionPanel
    subjectKind="incident"
    subjectId={inc.incident_id}
    kinds={['expected', 'not_expected', 'snooze', 'note']}
    decisions={shown.decisions ?? []}
    effective={shown.effective_review ?? null}
    memberScope
    memberCount={inc.member_count}
    kindHelp={INCIDENT_KIND_HELP}
    reload={load}
  />
{/if}
