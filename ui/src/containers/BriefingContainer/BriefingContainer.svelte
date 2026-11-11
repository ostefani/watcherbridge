<script>
  import { onMount, untrack } from 'svelte';
  import { fetchBriefing } from '@/api.js';
  import { createResource } from '@/lib/async/resource.js';
  import { reviewTitle, reportRange, singleViewIdentity } from '@/lib/format.js';
  import { priorityDiffers, lookupLimits, lookupReceipts, identityComplete } from '@/lib/format/briefingView.js';
  import { revealEvidence } from '@/lib/evidenceNavigation.js';
  import { createShortcutRegistry } from '@/lib/shortcuts/registry.js';
  import BriefingSummary from '@/components/BriefingSummary/BriefingSummary.svelte';
  import SnapshotNote from '@/components/SnapshotNote/SnapshotNote.svelte';
  import RecommendedCheck from '@/components/RecommendedCheck/RecommendedCheck.svelte';
  import PriorityExplanation from '@/components/PriorityExplanation/PriorityExplanation.svelte';
  import ScannerReports from '@/components/ScannerReports/ScannerReports.svelte';
  import InvestigatorNotes from '@/components/InvestigatorNotes/InvestigatorNotes.svelte';
  import TechnicalDetails from '@/components/TechnicalDetails/TechnicalDetails.svelte';
  // Shared review widget (plan step 33): screens embed DecisionsContainer
  // with subject props; page containers stay App-composed (see CONVENTIONS).
  // eslint-disable-next-line no-restricted-imports -- shared-widget exception above
  import DecisionsContainer from '@/containers/DecisionsContainer/DecisionsContainer.svelte';

  // Briefing container (Planv4 Step 24 order): Summary -> snapshot
  // notice -> Recommended check -> decision -> Why this priority ->
  // Saved scanner reports -> Analysis and retained context ->
  // Technical details. Associations come only from persisted
  // briefing_subjects rows; the host string is display text, never a
  // link source.
  // `initial` carries a server snapshot for prerender/tests: when set
  // the screen renders it without fetching, exactly as a completion
  // would. Production mounts omit it and fetch.
  // `layout` replaces the old `.embedded` CSS fork: 'page' is the full
  // route, 'embedded' the workspace review pane.
  let { id, initial = null, layout = 'page', ontitle = null, shortcuts = null } = $props();

  // Shortcut registry: App shares its global registry so the help
  // panel lists the working briefing keys; standalone renders fall
  // back to a local one.
  const registry = $derived(shortcuts ?? createShortcutRegistry());

  let articleElement = $state(null);
  let decisions = $state(null);
  let resource = $state(null);
  const shown = $derived($resource?.data ?? initial);
  const notice = $derived($resource?.error ?? $resource?.refreshError ?? null);

  // Reset the resource when the briefing changes: the previous
  // briefing must never render while the next one fetches (was BUG-2).
  // A provided snapshot renders as-is and never refetches.
  $effect(() => {
    const current = id;
    const next = createResource(() => fetchBriefing(current));
    resource = next;
    if (initial === null) {
      next.refresh();
    }
    return () => next.invalidate();
  });

  function load() {
    resource?.refresh();
  }

  const title = $derived(shown === null ? '' : reviewTitle(shown.kind, shown.observations, shown.referenced_events));
  $effect(() => {
    if (shown !== null && ontitle) {
      const briefingId = shown.briefing_id;
      const heading = title;
      // Reporting the title must not subscribe this effect to parent state.
      untrack(() => ontitle(briefingId, heading));
    }
  });

  const range = $derived(shown === null ? null : reportRange(shown.referenced_events));
  const identity = $derived(shown === null ? null : singleViewIdentity(shown.observations));
  const complete = $derived(identityComplete(shown));
  const differs = $derived(priorityDiffers(shown));
  const limits = $derived(lookupLimits(shown?.traces));
  const receipts = $derived(lookupReceipts(shown?.traces));

  const primaryEvent = $derived(shown?.referenced_events.length === 1 ? shown.referenced_events[0] : null);

  function jumpTo(key) {
    revealEvidence(articleElement, key);
  }

  /** Observations retained for exactly one event, in view order. */
  function viewsFor(/** @type {string} */ eventId) {
    return (shown?.observations ?? []).filter((o) => o.event_id === eventId);
  }

  // Dossier shortcuts: e expected, s snooze, x not-expected. The
  // registry stays silent while typing and never claims modifiers;
  // handlers preset the composer, the operator submits.
  onMount(() => {
    registry.register('briefing', 'e', 'preset expected', () => decisions?.applyPreset('expected'));
    registry.register('briefing', 's', 'preset snooze', () => decisions?.applyPreset('snooze'));
    registry.register('briefing', 'x', 'preset not-expected', () => decisions?.applyPreset('not_expected'));
    const onKey = (/** @type {KeyboardEvent} */ ev) => {
      registry.dispatch('briefing', ev);
    };
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('keydown', onKey);
    };
  });
</script>

{#if notice !== null && shown === null}
  <section aria-label="Briefing unavailable">
    <h2>Briefing unavailable</h2>
    <p>The bridge did not return this briefing ({notice}).</p>
    <button type="button" onclick={load}>Retry</button>
  </section>
{:else if shown === null}
  <p role="status">Loading briefing…</p>
{:else}
  <article class="briefing dossier" data-layout={layout} aria-label="Briefing shown" bind:this={articleElement}>
    {#if layout === 'page'}<p class="dossier-back"><a href="#/">← Back to Home</a></p>{/if}
    {#if notice !== null}
      <p role="alert">The saved view could not be refreshed. Your last loaded report is still shown. <button type="button" onclick={load}>Retry refresh</button></p>
    {/if}

    <div class="dossier-layout">
      <BriefingSummary
        {title}
        priority={shown.priority}
        description={shown.what_changed}
        {identity}
        identityComplete={complete}
        {primaryEvent}
        eventCount={shown.referenced_events.length}
        eventsTruncated={shown.referenced_truncated}
        {range}
        referencedTruncated={shown.referenced_truncated}
        source={shown.source}
        onJump={jumpTo}
      />

      <SnapshotNote />

      <RecommendedCheck
        check={shown.recommended_check}
        {identity}
        identityComplete={complete}
      />

      <aside class="dossier-side" aria-label="Operator decision">
        <DecisionsContainer subjectKind="briefing" subjectId={shown.briefing_id}
          kinds={['expected', 'not_expected', 'snooze', 'note']}
          decisions={shown.decisions ?? []} effective={shown.effective_review ?? null}
          ondecided={load} variant="card" bind:this={decisions} />
      </aside>

      <PriorityExplanation
        priority={shown.priority}
        priorityReason={shown.priority_reason}
        baseline={shown.baseline}
        baselineRule={shown.baseline_rule}
        modelSuggestion={shown.model_suggestion}
        clamped={shown.clamped}
        fallbackUsed={shown.fallback_used}
        differs={differs}
      />

      <ScannerReports
        events={shown.referenced_events}
        {viewsFor}
        profiles={shown.profiles}
        referencedTruncated={shown.referenced_truncated}
        observationsTruncated={shown.observations_truncated}
        onJump={jumpTo}
      />

      <InvestigatorNotes
        source={shown.source}
        likelyExplanation={shown.likely_explanation}
        confidence={shown.confidence}
        limited={limits.length > 0}
        context={shown.context}
        receipts={receipts}
        evidenceRefs={shown.evidence_refs}
        profiles={shown.profiles}
        profilesTruncated={shown.profiles_truncated}
        onJump={jumpTo}
      />

      <TechnicalDetails
        traces={shown.traces}
        inputTokens={shown.input_tokens}
        outputTokens={shown.output_tokens}
        costUsd={shown.cost_usd}
        briefingId={shown.briefing_id}
        groupId={shown.group_id}
        groupingVersion={shown.grouping_version}
        seq={shown.seq}
        createdAt={shown.created_at}
        host={shown.host}
      />
    </div>
  </article>
{/if}
