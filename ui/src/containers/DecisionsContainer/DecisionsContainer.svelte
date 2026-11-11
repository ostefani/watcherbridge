<script>
  import { onDestroy } from 'svelte';
  import { fetchDecisions, fetchDecisionMembers, postDecision, postUndo } from '@/api.js';
  import { memberUndoReview, continueMemberUndo } from '@/lib/memberUndo.js';
  import DecisionStatus from '@/components/DecisionStatus/DecisionStatus.svelte';
  import DecisionComposer from '@/components/DecisionComposer/DecisionComposer.svelte';
  import DecisionTrail from '@/components/DecisionTrail/DecisionTrail.svelte';
  import MemberUndoReview from '@/components/MemberUndoReview/MemberUndoReview.svelte';
  import styles from './DecisionsContainer.module.css';

  // Decisions container (Designv9 §5.1): explicit immutable verdicts
  // with superseding undo. The server owns the subject matrix, text
  // rules and idempotency; this container only offers the kinds the
  // parent allows and surfaces 4xx/409 answers as text.
  //
  // `variant="card"` selects the dossier design (no preselected kind,
  // strict submit gate); the default keeps the first offered kind
  // preselected with the pending-only gate. `preselect` names an
  // explicit initial kind for either variant. `preset` (dossier
  // shortcuts) stays until the Briefing step replaces it with an
  // explicit callback. `ondecided` replaces the old reload callback:
  // parents own refetching, the container only notifies.
  //
  // requestId is stable until a submission succeeds, so a retry after
  // a network failure replays safely instead of duplicating; success
  // rotates it for the next decision. Undo keys are stable per row
  // until that undo succeeds. History navigation is a bounded window,
  // never an unbounded append.
  let {
    subjectKind,
    subjectId,
    kinds = [],
    decisions = [],
    effective = null,
    memberScope = false,
    memberCount = 0,
    kindHelp = {},
    preselect = null,
    variant = 'plain',
    ondecided = null
  } = $props();

  const card = $derived(variant === 'card');
  const defaultKind = $derived(preselect ?? (card ? null : (kinds[0] ?? 'note')));

  let formError = $state(null);
  let listError = $state(null);
  let pending = $state(false);
  let undoPending = $state(/** @type {string|null} */ (null));
  let requestId = $state(crypto.randomUUID());
  let outcomeNote = $state(/** @type {string|null} */ (null));
  let memberReview = $state(null);
  let memberPaging = $state(false);
  let memberError = $state(null);
  let undoKeys = $state(/** @type {Record<string, string>} */ ({}));
  const PAGE_LIMIT = 50;
  const BACK_PAGES = 4;
  let alive = true;
  onDestroy(() => {
    alive = false;
  });
  // pageGen invalidates in-flight paging on every mutation and reset:
  // a response selected against a previous window can commit
  // neither rows nor exhaustion nor errors once the generation moved.
  // Head identity additionally fences parent-history replacement.
  let pageGen = 0;
  const headKey = $derived(
    decisions.length === 0 ? 'empty' : `${decisions[0].decision_id}:${decisions.length}`
  );
  let extra = $state([]);
  let extraKey = $state('');
  let backStack = $state(/** @type {{rows: any[], key: string}[]} */ ([]));
  let paging = $state(false);
  // Paging keys are reactive state alongside their values: derived
  // UI (exhaustion, notes) depends on them, and a plain variable
  // assignment would not invalidate it when the value itself is
  // unchanged (for example, recording exhaustion twice in a row).
  let atEnd = $state(false);
  let atEndKey = $state('');
  let windowNote = $state(/** @type {string|null} */ (null));
  let windowNoteKey = $state('');
  // Every paging flag is keyed to the head that produced it: a parent
  // refresh with a new head automatically unhides continuation and
  // drops stale navigation, with no reset timing to get right.
  const trail = $derived(extraKey === headKey && extra.length > 0 ? [...decisions, ...extra] : decisions);
  const exhausted = $derived(atEnd && atEndKey === headKey);
  const note = $derived(windowNoteKey === headKey ? windowNote : null);

  let composer = $state(null);

  // Explicit preset callback for dossier shortcuts: the briefing
  // container calls this instead of driving the composer through a
  // preset prop and an effect.
  export function applyPreset(next) {
    composer?.applyPreset(next);
  }

  async function submit({ kind, untilText, text, includeMembers }) {
    pending = true;
    formError = null;
    try {
      const d = { subjectKind, subjectId, kind, requestId };
      if (kind === 'snooze' && untilText !== '') {
        const at = new Date(untilText);
        if (Number.isNaN(at.getTime())) {
          throw new Error('snooze time is not a date');
        }
        d.until = at.toISOString();
      }
      if (kind === 'note' || kind === 'label') {
        d.text = text;
      }
      if (memberScope) {
        d.includeMembers = includeMembers;
      }
      const out = await postDecision(d);
      requestId = crypto.randomUUID();
      outcomeNote = null;
      memberReview = null;
      memberError = null;
      extra = [];
      extraKey = '';
      backStack = [];
      atEnd = false;
      windowNote = null;
      pageGen += 1;
      if (out.applied === false) {
        formError = `Already recorded (${out.reason ?? 'duplicate'}).`;
      } else if (Array.isArray(out.members) && out.members.length > 0) {
        outcomeNote = `Recorded on the incident and ${out.members.length} member briefing${out.members.length === 1 ? '' : 's'}.`;
      }
      await ondecided?.();
    } catch (err) {
      formError = err instanceof Error ? err.message : String(err);
      throw err;
    } finally {
      pending = false;
    }
  }

  async function undo(/** @type {string} */ id) {
    if (undoPending !== null) {
      return;
    }
    undoPending = id;
    listError = null;
    outcomeNote = null;
    memberReview = null;
    memberError = null;
    try {
      undoKeys[id] ??= crypto.randomUUID();
      const out = await postUndo(id, undoKeys[id]);
      if ((out.generated_decision_ids?.length ?? 0) > 0 || out.generated_next) {
        memberReview = memberUndoReview(id, out);
      }
      const rest = { ...undoKeys };
      delete rest[id];
      undoKeys = rest;
      extra = [];
      extraKey = '';
      backStack = [];
      atEnd = false;
      windowNote = null;
      pageGen += 1;
      await ondecided?.();
    } catch (err) {
      listError = err instanceof Error ? err.message : String(err);
    } finally {
      undoPending = null;
    }
  }

  async function moreMembers() {
    if (memberPaging || !memberReview?.next) {
      return;
    }
    const selected = memberReview;
    memberPaging = true;
    memberError = null;
    try {
      const next = await continueMemberUndo(selected, fetchDecisionMembers);
      if (alive && memberReview === selected) {
        memberReview = next;
      }
    } catch (err) {
      if (alive && memberReview === selected) {
        memberError = err instanceof Error ? err.message : String(err);
      }
    } finally {
      if (alive) {
        memberPaging = false;
      }
    }
  }

  async function older() {
    if (paging || exhausted || trail.length === 0) {
      return;
    }
    // Capture the generation and window identity up front: only a
    // response selected against this same mutation generation and
    // head may commit rows, exhaustion, or errors, and only while
    // mounted. An empty page marks exhaustion but preserves the
    // current window, so the last reachable page stays navigable.
    const gen = pageGen;
    const key = headKey;
    const before = trail[trail.length - 1].cursor;
    paging = true;
    listError = null;
    try {
      const page = await fetchDecisions(subjectKind, subjectId, { limit: PAGE_LIMIT, before });
      if (!alive || gen !== pageGen || key !== headKey) {
        return;
      }
      if (page.length === 0) {
        atEnd = true;
        atEndKey = key;
        return;
      }
      if (extraKey === key && extra.length > 0) {
        backStack.push({ rows: extra, key: extraKey });
        if (backStack.length > BACK_PAGES) {
          backStack.shift();
          windowNote = 'Earlier pages dropped from this view; the full audit remains available through the API.';
          windowNoteKey = key;
        }
      }
      extra = page;
      extraKey = key;
      atEnd = page.length < PAGE_LIMIT;
      atEndKey = key;
    } catch (err) {
      if (!alive || gen !== pageGen || key !== headKey) {
        return;
      }
      listError = err instanceof Error ? err.message : String(err);
    } finally {
      if (alive) {
        paging = false;
      }
    }
  }

  function newer() {
    while (backStack.length > 0 && backStack[backStack.length - 1].key !== headKey) {
      backStack.pop();
    }
    const prev = backStack.pop();
    if (prev === undefined) {
      extra = [];
      extraKey = '';
    } else {
      extra = prev.rows;
      extraKey = prev.key;
    }
    atEnd = false;
    atEndKey = headKey;
  }
</script>

<section aria-label="Operator decisions" class={card ? styles.card : undefined}>
  <DecisionStatus title={card ? 'Your decision' : 'Operator decisions'} {effective} {card} />
  <DecisionComposer
    bind:this={composer}
    {kinds}
    {subjectId}
    {kindHelp}
    {memberScope}
    {memberCount}
    {variant}
    defaultKind={defaultKind}
    {pending}
    {formError}
    {outcomeNote}
    onSubmit={submit}
  />
  {#if memberReview !== null}
    <MemberUndoReview review={memberReview} paging={memberPaging} error={memberError} onMore={moreMembers} />
  {/if}
  <DecisionTrail
    {trail}
    {exhausted}
    showNewer={extraKey === headKey && extra.length > 0}
    {note}
    {listError}
    {paging}
    {undoPending}
    {card}
    onUndo={undo}
    onOlder={older}
    onNewer={newer}
  />
</section>
