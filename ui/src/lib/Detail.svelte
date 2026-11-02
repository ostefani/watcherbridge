<script>
  import { onDestroy, onMount } from 'svelte';
  import { fetchBriefing } from '../api.js';
  import { createFetchGuard } from './freshness.js';
  import { typingTarget } from './roving.js';
  import { displayTime, sourceLabel } from './format.js';
  import DecisionPanel from './DecisionPanel.svelte';

  // Briefing detail (Planv4 Step 24 order): Summary -> Why this
  // priority -> Reported evidence -> Retained reported context ->
  // Recommended check -> Trace -> Provenance. Associations come only
  // from persisted briefing_subjects rows; the host string is display
  // text, never a link source.
  // initial carries a server snapshot for prerender/tests: when set
  // the screen renders it without fetching, exactly as a completion
  // would. Production mounts omit it and fetch.
  let { id, initial = null } = $props();

  const guard = createFetchGuard();
  onDestroy(() => guard.invalidate());

  let detail = $state(null);
  let error = $state(null);
  // Shortcut preset for the decision composer: e/s/x choose the kind
  // and focus submit, never submit on their own. A fresh object per
  // keypress so repeats refire the panel effect.
  let preset = $state(/** @type {{kind: string, seq: number}|null} */ (null));
  let presetSeq = 0;

  // Rendered briefing: fetched completion wins, snapshot fills in
  // until (or instead of) the fetch. Reads the prop inside a
  // derived, never as state, so updates stay explicit.
  const shown = $derived(detail ?? initial);

  async function load() {
    guard.invalidate();
    const token = guard.start();
    error = null;
    try {
      const fresh = await fetchBriefing(id);
      if (guard.current(token)) {
        detail = fresh;
      }
    } catch (err) {
      if (guard.current(token) && detail === null) {
        error = err instanceof Error ? err.message : String(err);
      }
    }
  }

  // Refetch only when the briefing ID changes; completions apply
  // through the guard, so an older response never wins. A provided
  // snapshot renders as-is and never refetches.
  $effect(() => {
    if (initial !== null) {
      return;
    }
    const current = id;
    guard.invalidate();
    const token = guard.start();
    error = null;
    fetchBriefing(current).then(
      (fresh) => {
        if (guard.current(token)) {
          detail = fresh;
        }
      },
      (err) => {
        if (guard.current(token) && detail === null) {
          error = err instanceof Error ? err.message : String(err);
        }
      }
    );
  });

  const created = $derived(shown === null ? null : displayTime(shown.created_at));

  // Dossier shortcuts: e expected, s snooze, x not-expected. Silent
  // while typing; they preset the composer, the operator submits.
  function onKey(/** @type {KeyboardEvent} */ ev) {
    if (typingTarget(ev) || ev.metaKey || ev.ctrlKey || ev.altKey) {
      return;
    }
    const map = { e: 'expected', s: 'snooze', x: 'not_expected' };
    const next = map[ev.key];
    if (next !== undefined && shown !== null) {
      presetSeq += 1;
      preset = { kind: next, seq: presetSeq };
    }
  }

  onMount(() => {
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('keydown', onKey);
    };
  });
</script>

{#if error !== null && shown === null}
  <section aria-label="Briefing unavailable">
    <h2>Briefing unavailable</h2>
    <p>The bridge did not return this briefing ({error}).</p>
    <button type="button" onclick={load}>Retry</button>
  </section>
{:else if shown === null}
  <p role="status">Loading briefing…</p>
{:else}
  <article aria-label="Briefing shown">
    <p><a href="#/">Back to Home</a></p>

    <section aria-label="Summary">
      <h2>{shown.priority}: {shown.what_changed}</h2>
      <p>Reported change: {shown.kind}. Reported by {shown.host}.</p>
    </section>

    <section aria-label="Why this priority">
      <h3>Why this priority</h3>
      <ol class="stepper">
        <li>Baseline floor: <strong>{shown.baseline}</strong>{shown.baseline_rule ? ` (${shown.baseline_rule})` : ''}</li>
        <li>
          Model suggestion:
          {#if shown.model_suggestion === null || shown.model_suggestion === undefined}
            <strong>none — baseline stands</strong>
          {:else}
            <strong>{shown.model_suggestion}</strong>{shown.clamped ? ' (clamped to one step above baseline)' : ''}
          {/if}
        </li>
        <li>Final priority: <strong>{shown.priority}</strong></li>
      </ol>
      <p>Source: {sourceLabel(shown.source)}.</p>
      {#if shown.fallback_used}
        <p>Deterministic fallback text: the model path did not produce this briefing.</p>
      {/if}
      <p>{shown.priority_reason}</p>
    </section>

    <section aria-label="Referenced events">
      <h3>Referenced events</h3>
      <p>An index of the grouped scanner reports, not the reports themselves.</p>
      {#if shown.referenced_events.length === 0}
        <p>No referenced events retained.</p>
      {:else}
        <ul>
          {#each shown.referenced_events as ref (ref.event_id)}
            <li>{ref.event_id} — {ref.event_type}</li>
          {/each}
        </ul>
      {/if}
      {#if shown.referenced_truncated}
        <p>Older groups may reference more events than shown.</p>
      {/if}
    </section>

    <section aria-label="Reported evidence">
      <h3>Reported evidence</h3>
      <p>Retained scanner views for exactly the referenced events — never a profile's latest view.</p>
      {#if shown.observations.length === 0}
        <p>No retained observations for these events.</p>
      {:else}
        {#each shown.observations as obs (`${obs.event_id}:${obs.view_role}`)}
          {@const reported = displayTime(obs.reported_at)}
          <details>
            <summary>{obs.event_id} ({obs.view_role}), reported {reported.local}</summary>
            <dl>
              {#each Object.entries(obs.attributes ?? {}) as [key, value] (key)}
                <dt>{key}</dt>
                <dd>{JSON.stringify(value)}</dd>
              {/each}
            </dl>
            {#if (obs.ports ?? []).length > 0}
              <ul>
                {#each obs.ports as port (`${port.proto}/${port.port}`)}
                  <li>{port.proto}/{port.port} {port.state}{port.service ? ` (${port.service})` : ''}</li>
                {/each}
              </ul>
            {/if}
            <p>Snapshot covers {obs.ports_total} ports{obs.ports_truncated ? ' (truncated)' : ''}.</p>
          </details>
        {/each}
      {/if}
      {#if shown.observations_truncated}
        <p>More observations exist than shown; the section is byte- and count-bounded.</p>
      {/if}
    </section>

    <section aria-label="Retained reported context">
      <h3>Retained reported context</h3>
      <p>{shown.context}</p>
      {#if shown.profiles.length === 0}
        <p>No profile links persisted for this briefing.</p>
      {:else}
        <ul>
          {#each shown.profiles as link (`${link.profile_id}:${link.role}`)}
            <li>
              <a href={`#/devices/${link.profile_id}`}>{link.profile_id}</a> ({link.role})
            </li>
          {/each}
        </ul>
      {/if}
      {#if shown.profiles_truncated}
        <p>More profile links exist than shown.</p>
      {/if}
    </section>

    <section aria-label="Recommended check">
      <h3>Recommended check</h3>
      <p>{shown.recommended_check}</p>
    </section>

    <DecisionPanel
      subjectKind="briefing"
      subjectId={shown.briefing_id}
      kinds={['expected', 'not_expected', 'snooze', 'note']}
      decisions={shown.decisions ?? []}
      effective={shown.effective_review ?? null}
      {preset}
      reload={load}
    />

    <section aria-label="How I checked this">
      <h3>How I checked this</h3>
      {#if shown.likely_explanation || (shown.evidence_refs && shown.evidence_refs.length) || (shown.traces && shown.traces.length)}
        {#if shown.likely_explanation}
          <p><strong>Likely explanation.</strong> {shown.likely_explanation}</p>
        {/if}
        {#if shown.confidence}
          <p>Confidence: {shown.confidence}</p>
        {/if}
        {#if shown.input_tokens != null || shown.output_tokens != null || shown.cost_usd != null}
          <p>
            Accounting:{#if shown.input_tokens != null} {shown.input_tokens} input tokens{/if}{#if shown.output_tokens != null}, {shown.output_tokens} output tokens{/if}{#if shown.cost_usd != null}, ${shown.cost_usd}{/if}.
          </p>
        {/if}
        {#if shown.evidence_refs && shown.evidence_refs.length}
          <h4>Cited evidence</h4>
          <ul>
            {#each shown.evidence_refs as ref (ref.event_id ?? `step:${ref.tool_step}`)}
              <li>
                {#if ref.event_id}
                  Event {ref.event_id}
                {:else}
                  Tool step {ref.tool_step}
                {/if}
              </li>
            {/each}
          </ul>
        {/if}
        {#if shown.traces && shown.traces.length}
          <h4>Executed tool steps</h4>
          <ol>
            {#each shown.traces as step (step.step)}
              <li>
                Step {step.step}: {step.tool} — {step.status} ({step.duration_ms} ms)
                <pre>Args: {step.args}</pre>
                <pre>Result: {step.result}</pre>
              </li>
            {/each}
          </ol>
        {/if}
      {:else}
        <p>No agent trace is available for this briefing.</p>
      {/if}
    </section>

    <section aria-label="Provenance">
      <h3>Provenance</h3>
      <dl>
        <dt>Briefing</dt>
        <dd>{shown.briefing_id}</dd>
        <dt>Group</dt>
        <dd>{shown.group_id}</dd>
        <dt>Grouping version</dt>
        <dd>{shown.grouping_version}</dd>
        <dt>Sequence</dt>
        <dd>{shown.seq}</dd>
        <dt>Created</dt>
        <dd><time datetime={shown.created_at} title={created?.utc}>{created?.local}</time></dd>
      </dl>
    </section>
  </article>
{/if}

<style>
  .stepper {
    list-style: none;
    padding-left: 0;
  }
  .stepper li {
    padding: 0.25rem 0;
  }
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
  details {
    border: 1px solid #ccc;
    padding: 0.5rem 0.75rem;
    margin-bottom: 0.5rem;
  }
  summary {
    cursor: pointer;
  }
</style>
