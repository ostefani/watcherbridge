<script>
  import { createWindowPager } from '@/lib/windowPager.svelte.js';
  import { toErrorMessage } from '@/lib/format.js';
  import { onDestroy } from 'svelte';
  import { fetchDecisions, fetchDecisionMembers, postDecision, postUndo } from '@/api.js';
  import { memberUndoReview, continueMemberUndo } from '@/lib/memberUndo.js';
  import { buildDecisionPayload } from '@/lib/payload.js';
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
  const audit = createWindowPager({
    getHead: () => decisions,
    getSubject: () => [subjectKind, subjectId],
    fetchPage: (query) => fetchDecisions(subjectKind, subjectId, query)
  });
  let alive = true;
  onDestroy(() => {
    alive = false;
    audit.reset();
  });

  let composer = $state(null);

  // Explicit preset callback for dossier shortcuts: the briefing
  // container calls this instead of driving the composer through a
  // preset prop and an effect.
  export function applyPreset(next) {
    composer?.applyPreset(next);
  }

  // Returns 'ok' when the draft posted (including an idempotent
  // duplicate) and 'error' otherwise. Never throws: the composer must
  // await this without producing an unhandled rejection, and drafts
  // clear only on 'ok' so a failed submit keeps the form for retry.
  async function submit({ kind, untilText, text, includeMembers }) {
    pending = true;
    formError = null;
    try {
      const built = buildDecisionPayload(
        { kind, untilText, text, includeMembers },
        { subjectKind, subjectId, requestId, memberScope }
      );
      if (built.error) {
        formError = built.error;
        return 'error';
      }
      const out = await postDecision(built.payload);
      requestId = crypto.randomUUID();
      outcomeNote = null;
      memberReview = null;
      memberError = null;
      audit.reset();
      if (out.applied === false) {
        formError = `Already recorded (${out.reason ?? 'duplicate'}).`;
      } else if (Array.isArray(out.members) && out.members.length > 0) {
        outcomeNote = `Recorded on the incident and ${out.members.length} member briefing${out.members.length === 1 ? '' : 's'}.`;
      }
      await ondecided?.();
      return 'ok';
    } catch (err) {
      formError = toErrorMessage(err);
      return 'error';
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
    audit.clearError();
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
      audit.reset();
      await ondecided?.();
    } catch (err) {
      listError = toErrorMessage(err);
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
        memberError = toErrorMessage(err);
      }
    } finally {
      if (alive) {
        memberPaging = false;
      }
    }
  }

  function older() {
    listError = null;
    return audit.older();
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
    trail={audit.trail}
    exhausted={audit.exhausted}
    showNewer={audit.showNewer}
    note={audit.note}
    listError={listError ?? audit.error}
    paging={audit.paging}
    {undoPending}
    {card}
    onUndo={undo}
    onOlder={older}
    onNewer={audit.newer}
  />
</section>
