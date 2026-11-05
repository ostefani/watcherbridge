<script>
  import { onDestroy, onMount, untrack } from 'svelte';
  import { fetchBriefing } from '../api.js';
  import { createFetchGuard } from './freshness.js';
  import { typingTarget } from './roving.js';
  import { displayTime, sourceLabel, reviewTitle, reportRange, singleViewIdentity } from './format.js';
  import DecisionPanel from './DecisionPanel.svelte';
  import CopyValue from './CopyValue.svelte';
  import ReportView from './ReportView.svelte';
  import TraceStep from './TraceStep.svelte';
  import { revealEvidence } from './evidenceNavigation.js';

  // Briefing dossier (Planv4 Step 24 order): Summary -> snapshot
  // notice -> Recommended check -> decision -> Why this priority ->
  // Saved scanner reports -> Analysis and retained context ->
  // Technical details (traces, accounting, provenance). Associations
  // come only from persisted briefing_subjects rows; the host string
  // is display text, never a link source.
  // initial carries a server snapshot for prerender/tests: when set
  // the screen renders it without fetching, exactly as a completion
  // would. Production mounts omit it and fetch.
  let { id, initial = null, embedded = false, ontitle = null } = $props();

  const guard = createFetchGuard();
  onDestroy(() => guard.invalidate());

  let articleElement = $state(null);
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
  const title = $derived(shown === null ? '' : reviewTitle(shown.kind, shown.observations, shown.referenced_events));
  $effect(() => {
    if (shown !== null && ontitle) {
      const briefingId = shown.briefing_id;
      const heading = title;
      // Reporting the title must not subscribe this effect to parent state.
      untrack(() => ontitle(briefingId, heading));
    }
  });

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
      if (guard.current(token)) {
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
        if (guard.current(token)) {
          error = err instanceof Error ? err.message : String(err);
        }
      }
    );
  });

  const created = $derived(shown === null ? null : displayTime(shown.created_at));
  const range = $derived(shown === null ? null : reportRange(shown.referenced_events));
  const identity = $derived(shown === null ? null : singleViewIdentity(shown.observations));
  // The priority comparison opens when the reader needs it: equal
  // baseline, suggestion and final stay collapsed; a difference or a
  // clamp opens the detail.
  const priorityDiffers = $derived(
    shown !== null &&
      (shown.clamped || shown.baseline !== shown.priority || (shown.model_suggestion ?? shown.baseline) !== shown.priority)
  );

  const primaryEvent = $derived(shown?.referenced_events.length === 1 ? shown.referenced_events[0] : null);
  const identityComplete = $derived(shown !== null && !shown.observations_truncated && !shown.referenced_truncated);
  const lookupLimits = $derived((shown?.traces ?? []).filter((step) =>
    ['unknown_subject', 'blocked', 'error', 'unavailable', 'truncated'].includes(step.outcome)));

  const lookupReceipts = $derived.by(() => {
    const groups = [];
    for (const step of shown?.traces ?? []) {
      const key = step.summary || `step:${step.step}`;
      let group = groups.find((receipt) => receipt.key === key);
      if (!group) { group = { key, summary: step.summary, steps: [] }; groups.push(group); }
      group.steps.push(step.step);
    }
    return groups;
  });

  function jumpTo(key) {
    revealEvidence(articleElement, key);
  }

  /** Observations retained for exactly one event, in view order. */
  function viewsFor(/** @type {string} */ eventId) {
    return (shown?.observations ?? []).filter((o) => o.event_id === eventId);
  }

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
  <article class="briefing dossier" class:embedded aria-label="Briefing shown" bind:this={articleElement}>
    {#if !embedded}<p class="dossier-back"><a href="#/">← Back to Home</a></p>{/if}
    {#if error !== null}
      <p role="alert">The saved view could not be refreshed. Your last loaded report is still shown. <button type="button" onclick={load}>Retry refresh</button></p>
    {/if}

    <div class="dossier-layout">
      <section class="briefing-summary dossier-card" aria-label="Summary">
        <div class="briefing-heading">
          <h2>{title}</h2>
          <span class="pill priority" data-priority={shown.priority} aria-label={`Priority: ${shown.priority}`}>{shown.priority} priority</span>
        </div>
        <p class="briefing-description">{shown.what_changed}</p>
        <dl class="identity-grid">
          {#if identity !== null && identityComplete}
            <div><dt>Reported host</dt><dd>{identity.hostname ?? 'Not reported'}</dd></div>
            <div><dt>IP address</dt><dd class="mono">{identity.ip ?? 'Not reported'}{#if identity.ip !== null}<CopyValue value={identity.ip} name="IP address" />{/if}</dd></div>
            <div><dt>MAC address</dt><dd class="mono">{identity.mac ?? 'Not reported'}{#if identity.mac !== null}<CopyValue value={identity.mac} name="MAC address" />{/if}</dd></div>
          {:else}
            <div class="identity-group"><dt>Reported identities</dt><dd>See the individual report views below<p class="section-note">{identityComplete ? 'Grouped changes can describe different host views.' : 'Only part of the retained evidence is shown.'}</p></dd></div>
          {/if}
          <div><dt>Reported scan target</dt><dd class="mono">{primaryEvent?.target || (shown.referenced_events.length > 1 ? 'Shown per report below' : 'Not reported')}{#if primaryEvent?.target_truncated}<span class="section-note"> (truncated)</span>{/if}</dd></div>
          <div><dt>{range?.partial || shown.referenced_truncated ? 'Available report times' : shown.referenced_events.length > 1 ? 'Report time range' : 'Report time'}</dt><dd>{#if range !== null}<span title={range.utc}>{range.local}</span>{:else}Not reported{/if}</dd></div>
          <div><dt>Analysis source</dt><dd>{sourceLabel(shown.source)}</dd></div>
        </dl>
        <div class="evidence-index">
          <span>{shown.referenced_events.length}{shown.referenced_truncated ? '+' : ''} source report{shown.referenced_events.length === 1 ? '' : 's'}</span>
          {#if primaryEvent}
            <button type="button" class="text-link" onclick={() => jumpTo(`event:${primaryEvent.event_id}`)}>View scanner snapshot →</button>
          {:else if shown.referenced_events.length > 0}
            <button type="button" class="text-link" onclick={() => jumpTo('reports')}>View saved reports →</button>
          {/if}
        </div>
      </section>

      <div class="snapshot-note"><span class="info-mark" aria-hidden="true">i</span><p><strong>Reported at the time shown.</strong> Network conditions may have changed since this report.</p></div>

      <section class="dossier-card recommended-card" aria-label="Recommended check">
        <div class="section-heading"><span class="section-icon" aria-hidden="true">↗</span><div><h3>Recommended check</h3><p class="section-note">Use this check to decide whether further investigation is needed.</p></div></div>
        <p class="recommended-action">{shown.recommended_check}</p>
        {#if identity !== null && identityComplete && (identity.ip !== null || identity.mac !== null)}
          <div class="copy-row"><span class="section-note">Copy for your investigation</span>
            {#if identity.ip !== null}<span class="copy-chip">{identity.ip} <CopyValue value={identity.ip} name="IP address" /></span>{/if}
            {#if identity.mac !== null}<span class="copy-chip">{identity.mac} <CopyValue value={identity.mac} name="MAC address" /></span>{/if}
          </div>
        {/if}
      </section>

      <aside class="dossier-side" aria-label="Operator decision">
        <DecisionPanel subjectKind="briefing" subjectId={shown.briefing_id}
          kinds={['expected', 'not_expected', 'snooze', 'note']}
          decisions={shown.decisions ?? []} effective={shown.effective_review ?? null}
          {preset} reload={load} requireChoice />
      </aside>

      <section class="dossier-card" aria-label="Why this priority">
        <h3>Why this priority</h3>
        <p class="priority-reason">{shown.priority_reason}</p>
        <div class="priority-rule"><span class="pill priority" data-priority={shown.priority}>Final priority: {shown.priority}</span>{#if shown.baseline_rule}<code>{shown.baseline_rule}</code>{/if}</div>
        <details class="priority-comparison" open={priorityDiffers}>
          <summary>How the priority was computed</summary>
          <ol class="stepper">
            <li>Baseline floor <strong>{shown.baseline}</strong></li>
            <li>Model suggestion <strong>{shown.model_suggestion ?? 'No model suggestion — baseline stands'}</strong></li>
            <li>Final priority <strong>{shown.priority}</strong></li>
          </ol>
          {#if shown.clamped}<p class="section-note">Suggestion adjusted to respect the baseline floor and one-step raise limit.</p>{/if}
        </details>
        {#if shown.fallback_used}<p class="section-note">Deterministic fallback text: the model path did not produce this briefing.</p>{/if}
      </section>

      <section class="dossier-card" aria-label="Saved scanner reports" data-evidence="reports" tabindex="-1">
        <div class="section-heading spread"><h3>Scanner snapshot{shown.referenced_events.length === 1 ? '' : 's'}</h3><span class="pill">Saved at report time</span></div>
        <p class="section-note">Open a report to see what the scanner supplied, with its linked context.</p>
        {#if shown.referenced_events.length === 0}<p>No referenced events retained.</p>
        {:else}{#each shown.referenced_events as ref (ref.event_id)}<ReportView {ref} views={viewsFor(ref.event_id)} profileLinks={shown.profiles} />{/each}{/if}
        {#if shown.referenced_truncated}<p class="section-note">More referenced events exist than shown.</p>{/if}
        {#if shown.observations_truncated}<p class="section-note">More observations exist than shown; some saved views are omitted.</p>{/if}
      </section>

      <section class="dossier-card investigator-notes" aria-label="Investigator notes">
        <h3>{shown.source === 'agent' ? 'Investigator notes' : 'Analysis and retained context'}</h3>
        {#if shown.likely_explanation}<p class="hypothesis"><strong>Possible explanation:</strong> {shown.likely_explanation}</p>{/if}
        {#if shown.confidence}<p><span class="pill">Explanation confidence (model self-assessment): {shown.confidence}</span></p>{/if}
        {#if lookupLimits.length > 0}<p class="context-limit"><strong>Investigation has limits.</strong> Some lookups could not establish retained context. Missing lookup results do not prove this host has no history.</p>{/if}
        <p class="context-text">{shown.context}</p>
        {#if shown.traces?.length}
          <h4>What the investigator could establish</h4>
          <ul class="lookup-receipts">
            {#each lookupReceipts as receipt, i (i)}
              <li><span class="receipt-mark" aria-hidden="true">•</span><div><p>{receipt.summary || 'No interpreted result is available. Open the tool step to inspect the saved response.'}</p><div class="receipt-steps">{#each receipt.steps as step (step)}<button type="button" class="text-link" onclick={() => jumpTo(`step:${step}`)}>View step {step}</button>{/each}</div></div></li>
            {/each}
          </ul>
        {/if}
        {#if shown.evidence_refs?.length}
          <div class="citation-row"><span class="section-note">Cited evidence</span>
            {#each shown.evidence_refs as ref, i (`${ref.event_id ?? ref.tool_step}:${i}`)}
              <button type="button" class="citation" onclick={() => jumpTo(ref.event_id ? `event:${ref.event_id}` : `step:${ref.tool_step}`)}>{ref.event_id ? `Event ${ref.event_id}` : `Tool step ${ref.tool_step}`}</button>
            {/each}
          </div>
        {/if}
        {#if shown.profiles.length > 0}
          <details class="context-links"><summary>Linked reported context</summary><ul>{#each shown.profiles as link (`${link.profile_id}:${link.role}`)}<li><a href={`#/devices/${encodeURIComponent(link.profile_id)}`}>Open reported context</a> <span class="section-note">{link.profile_id} ({link.role})</span></li>{/each}</ul></details>
        {:else}<p class="section-note">No profile links persisted for this briefing.</p>{/if}
        {#if shown.profiles_truncated}<p class="section-note">More profile links exist than shown.</p>{/if}
      </section>

      <details class="tech-block dossier-card" aria-label="Technical details">
        <summary>Technical details <span class="section-note">Tool calls, accounting and provenance</span></summary>
        <div class="tech-body"><h4>Executed tool steps</h4>
          {#if shown.traces?.length}<ol class="trace-list">{#each shown.traces as step (step.step)}<TraceStep {step} />{/each}</ol>{:else}<p>No agent trace is available for this briefing.</p>{/if}
          {#if shown.input_tokens != null || shown.output_tokens != null || shown.cost_usd != null}
            <h4>Model accounting</h4><p>{shown.input_tokens ?? 'Unknown'} input tokens · {shown.output_tokens ?? 'Unknown'} output tokens · {shown.cost_usd == null ? 'Cost unknown' : `$${shown.cost_usd}`}</p>
          {/if}
          <h4>Provenance</h4><dl>
            <dt>Briefing</dt><dd>{shown.briefing_id}</dd><dt>Group</dt><dd>{shown.group_id}</dd>
            <dt>Grouping version</dt><dd>{shown.grouping_version}</dd><dt>Sequence</dt><dd>{shown.seq}</dd>
            <dt>Created</dt><dd><time datetime={shown.created_at} title={created?.utc}>{created?.local}</time></dd>
            <dt>Grouping anchor</dt><dd>{shown.host}</dd>
          </dl>
        </div>
      </details>
    </div>
  </article>
{/if}

<style>
  .dossier { max-width: 1400px; margin: 0 auto; container-type: inline-size; }
  .dossier-back { margin: 0 0 20px; }
  .dossier-back a { text-decoration: none; }
  .dossier-layout { display: grid; grid-template-columns: minmax(0, 1fr) minmax(300px, .48fr); gap: 20px 24px; align-items: start; }
  .dossier-layout > :not(.dossier-side) { grid-column: 1; min-width: 0; }
  .dossier-card { padding: 26px; margin: 0; border: 1px solid var(--border); border-radius: 16px; background: var(--surface); overflow-wrap: anywhere; }
  .dossier-side { grid-column: 2; grid-row: 1 / span 7; position: sticky; top: 20px; max-height: calc(100dvh - 40px); overflow: auto; border-radius: 16px; }
  .dossier .briefing-heading { align-items: flex-start; flex-wrap: wrap; margin-bottom: 16px; }
  .dossier .briefing-heading h2 { font-size: clamp(23px, 2.1vw, 30px); line-height: 1.22; letter-spacing: -.7px; flex: 1 1 260px; }
  .dossier .briefing-heading .priority { text-transform: uppercase; letter-spacing: .06em; font-weight: 700; padding: 5px 12px; }
  .dossier .briefing-description { font-size: 16px; line-height: 1.65; margin-bottom: 24px; }
  .dossier .identity-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 22px 28px; border-top: 1px solid var(--border); padding-top: 22px; margin: 0; }
  .identity-grid > div { min-width: 0; }
  .identity-grid dt { font-size: 11px; text-transform: uppercase; letter-spacing: .08em; margin-bottom: 7px; }
  .identity-grid dd { display: flex; flex-wrap: wrap; align-items: center; gap: 6px 10px; font-size: 15px; font-weight: 600; margin: 0; }
  .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  .identity-group { grid-column: 1 / -1; }
  .identity-group p { margin: 5px 0 0; }
  .evidence-index { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 4px 12px; margin-top: 24px; padding-top: 14px; border-top: 1px solid var(--border); color: var(--muted); font-size: 13px; }
  .snapshot-note { display: flex; gap: 12px; padding: 15px 18px; border: 1px solid var(--accent); border-radius: 12px; color: var(--accent); background: var(--accent-soft); }
  .snapshot-note p { margin: 0; }
  .info-mark { display: inline-grid; place-items: center; border: 1.5px solid currentColor; border-radius: 50%; width: 19px; height: 19px; flex: none; margin-top: 2px; font-size: 12px; font-weight: 700; }
  .dossier h3 { font-size: 18px; margin-bottom: 12px; letter-spacing: -.3px; }
  .section-heading { display: flex; align-items: flex-start; gap: 12px; }
  .section-heading h3 { margin-bottom: 5px; }
  .section-heading .section-note { margin: 0; }
  .spread { justify-content: space-between; flex-wrap: wrap; margin-bottom: 12px; }
  .section-icon { display: grid; place-items: center; width: 34px; height: 34px; background: var(--accent-soft); color: var(--accent); border-radius: 9px; font-size: 23px; flex: none; }
  .recommended-action { margin: 18px 0; border: 1px solid var(--border); border-left: 3px solid var(--accent); border-radius: 9px; padding: 16px; font-size: 15px; line-height: 1.7; }
  .copy-row { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; }
  .copy-chip { display: inline-flex; flex-wrap: wrap; align-items: center; gap: 8px; padding: 5px 8px 5px 10px; border: 1px solid var(--border); border-radius: 8px; background: var(--subtle); font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; overflow-wrap: anywhere; min-width: 0; }
  .priority-rule { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; margin-bottom: 10px; }
  .priority-rule code { padding: 4px 8px; border-radius: 6px; background: var(--subtle); border: 1px solid var(--border); }
  .priority-comparison, .context-links { border: 0; border-top: 1px solid var(--border); border-radius: 0; padding: 0; margin: 16px 0 0; }
  .priority-comparison summary, .context-links summary { color: var(--accent); font-size: 13px; }
  .hypothesis, .context-text { line-height: 1.7; }
  .context-limit { padding: 12px 14px; border: 1px solid var(--warning-border); background: var(--warning-bg); color: var(--warning); border-radius: 9px; font-size: 13px; }
  .lookup-receipts { list-style: none; margin: 0; padding: 0; }
  .lookup-receipts li { display: flex; align-items: flex-start; gap: 12px; padding: 12px 0; border-top: 1px solid var(--border); }
  .receipt-steps { display: flex; flex-wrap: wrap; gap: 4px 16px; }
  .lookup-receipts p { margin: 0; font-size: 13px; }
  .receipt-mark { color: var(--muted); font-weight: 700; }
  .text-link { border: 0; background: transparent; color: var(--accent); font-size: 13px; padding: 6px 0; min-height: 36px; text-align: left; }
  .text-link:hover { text-decoration: underline; background: transparent; }
  .citation-row { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; margin-top: 16px; }
  .citation { font-size: 12px; color: var(--accent); background: var(--accent-soft); padding: 4px 9px; min-height: 32px; }
  .tech-block { padding: 0 24px; }
  .tech-block > summary { padding: 20px 0; font-size: 15px; }
  .tech-block > summary span { margin-left: 10px; font-weight: 400; }
  .tech-body { border-top: 1px solid var(--border); padding: 18px 0 8px; }
  .trace-list { padding: 0; margin: 0 0 20px; }
  .embedded .dossier-layout { display: flex; flex-direction: column; gap: 16px; }
  .embedded .dossier-layout > * { width: 100%; }
  .embedded .dossier-side { position: static; max-height: none; overflow: visible; order: 1; }
  .embedded .recommended-card { order: 0; }
  .embedded .dossier-card:not(.briefing-summary,.recommended-card), .embedded .tech-block { order: 2; }
  .embedded .dossier-card { padding: 20px; }
  @container (max-width: 840px) {
    .dossier-layout { display: flex; flex-direction: column; }
    .dossier-layout > * { width: 100%; }
    .dossier-side { position: static; max-height: none; overflow: visible; order: 1; }
    .dossier-card:not(.briefing-summary,.recommended-card), .tech-block { order: 2; }
  }
  @container (max-width: 500px) {
    .dossier .identity-grid { grid-template-columns: minmax(0,1fr); gap: 18px; }
    .dossier-card { padding: 20px; }
    .tech-block { padding: 0 20px; }
  }
</style>
