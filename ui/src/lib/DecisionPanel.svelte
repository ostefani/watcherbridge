<script>
  import { onDestroy } from 'svelte';
  import { fetchDecisions, postDecision, postUndo } from '../api.js';

  // Operator decisions (Designv9 §5.1): explicit immutable verdicts
  // with superseding undo. The server owns the subject matrix, text
  // rules and idempotency; this panel only offers the kinds the
  // parent allows and surfaces 4xx/409 answers as text. Text renders
  // as text, never HTML, so hostile scanner strings stay inert.
  //
  // requestId is stable until a submission succeeds, so a retry after
  // a network failure replays safely instead of duplicating; success
  // rotates it for the next decision.
  let {
    subjectKind,
    subjectId,
    kinds = [],
    decisions = [],
    effective = null,
    preset = null,
    reload = async () => {}
  } = $props();

  const KIND_HELP = {
    expected: 'Mark reviewed: this briefing needs no further action.',
    not_expected: 'Flag for follow-up: this briefing still needs a check.',
    snooze: 'Hide until a time. Empty keeps the server default (24h).',
    note: 'Free-text note, kept with the audit (max 2000 characters).',
    label: 'Your label for this profile, never identity (max 256 characters).'
  };

  // chosen overrides the parent-supplied first kind; the derived
  // reads the prop each time so lint sees no stale capture.
  let chosen = $state(/** @type {string|null} */ (null));
  const kind = $derived(chosen ?? kinds[0] ?? 'note');
  let untilText = $state('');
  let text = $state('');
  let pending = $state(false);
  let undoPending = $state(/** @type {string|null} */ (null));
  let formError = $state(null);
  let listError = $state(null);
  let requestId = $state(crypto.randomUUID());
  let submitButton = $state(null);
  // Undo keys are stable per row until that undo succeeds: a retry
  // after a network failure replays against the original undo row
  // instead of minting a second one.
  let undoKeys = $state(/** @type {Record<string, string>} */ ({}));
  // History navigation is a bounded window, never an unbounded
  // append. headKey identifies the embedded head; an older page
  // (extra) and the back stack apply only while their key matches
  // it, so a late page response can never merge rows selected
  // against a previous window, and destroying the panel drops
  // in-flight work silently.
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
  let atEnd = $state(false);
  let windowNote = $state(/** @type {string|null} */ (null));
  const trail = $derived(extraKey === headKey && extra.length > 0 ? [...decisions, ...extra] : decisions);

  const needsText = $derived(kind === 'note' || kind === 'label');
  const textCap = $derived(kind === 'label' ? 256 : 2000);

  // Shortcut presets from the dossier screen: adopt the kind and
  // hand focus to submit. Nothing submits without the operator.
  // The preset object is fresh per keypress, so repeats refire.
  $effect(() => {
    if (preset !== null && kinds.includes(preset.kind)) {
      chosen = preset.kind;
      submitButton?.focus();
    }
  });

  function statusLine() {
    if (effective === null || effective === undefined) {
      return 'No operator review';
    }
    if (effective.kind === 'snooze') {
      return `Snoozed${effective.until ? ` until ${effective.until}` : ''}`;
    }
    if (effective.kind === 'expected') {
      return 'Reviewed: expected';
    }
    return `Flagged: ${effective.kind.replace('_', ' ')}`;
  }

  async function submit(/** @type {SubmitEvent} */ ev) {
    ev.preventDefault();
    if (pending) {
      return;
    }
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
      if (needsText) {
        d.text = text;
      }
      const out = await postDecision(d);
      requestId = crypto.randomUUID();
      chosen = null;
      untilText = '';
      text = '';
      extra = [];
      extraKey = '';
      backStack = [];
      atEnd = false;
      windowNote = null;
      pageGen += 1;
      if (out.applied === false) {
        formError = `Already recorded (${out.reason ?? 'duplicate'}).`;
      }
      await reload();
    } catch (err) {
      formError = err instanceof Error ? err.message : String(err);
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
    try {
      undoKeys[id] ??= crypto.randomUUID();
      await postUndo(id, undoKeys[id]);
      const rest = { ...undoKeys };
      delete rest[id];
      undoKeys = rest;
      extra = [];
      extraKey = '';
      backStack = [];
      atEnd = false;
      windowNote = null;
      pageGen += 1;
      await reload();
    } catch (err) {
      listError = err instanceof Error ? err.message : String(err);
    } finally {
      undoPending = null;
    }
  }

  async function older() {
    if (paging || atEnd || trail.length === 0) {
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
        return;
      }
      if (extraKey === key && extra.length > 0) {
        backStack.push({ rows: extra, key: extraKey });
        if (backStack.length > BACK_PAGES) {
          backStack.shift();
          windowNote = 'Earlier pages dropped from this view; the full audit remains available through the API.';
        }
      }
      extra = page;
      extraKey = key;
      atEnd = page.length < PAGE_LIMIT;
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
  }

</script>

<section aria-label="Operator decisions">
  <h3>Operator decisions</h3>
  <p><span class="pill">Status: {statusLine()}</span></p>

  <form onsubmit={submit} aria-label="Record a decision">
    <fieldset>
      <legend>Kind</legend>
      {#each kinds as k (k)}
        <label>
          <input type="radio" name="decision-kind" value={k} checked={kind === k} onchange={() => (chosen = k)} />
          {k.replace('_', ' ')}
        </label>
      {/each}
    </fieldset>
    <p class="help">{KIND_HELP[kind] ?? ''}</p>
    {#if kind === 'snooze'}
      <label>
        Until (empty keeps the server default)
        <input type="datetime-local" bind:value={untilText} />
      </label>
    {/if}
    {#if needsText}
      <label>
        {kind === 'label' ? 'Label' : 'Note'}
        <textarea bind:value={text} maxlength={textCap} rows="2" required></textarea>
      </label>
    {/if}
    {#if formError !== null}
      <p role="alert">{formError}</p>
    {/if}
    <button type="submit" bind:this={submitButton} disabled={pending}>
      {pending ? 'Recording…' : 'Record decision'}
    </button>
  </form>

  {#if trail.length === 0}
    <p>No decisions recorded.</p>
  {:else}
    <ol class="trail">
      {#each trail as d (d.decision_id)}
        <li>
          <span class="pill">{d.kind.replace('_', ' ')}</span>
          {#if d.undone}<span class="pill">undone</span>{/if}
          {#if d.text}<span class="text">{d.text}</span>{/if}
          {#if d.until}<span class="until">until {d.until}</span>{/if}
          <time datetime={d.created_at}>{new Date(d.created_at).toLocaleString()}</time>
          {#if !d.undone && d.kind !== 'undo'}
            <button
              type="button"
              onclick={() => undo(d.decision_id)}
              disabled={undoPending !== null}
            >
              {undoPending === d.decision_id ? 'Undoing…' : 'Undo'}
            </button>
          {/if}
        </li>
      {/each}
    </ol>
    {#if !atEnd}
      <button type="button" onclick={older} disabled={paging}>
        {paging ? 'Loading…' : 'Show older decisions'}
      </button>
    {/if}
    {#if extraKey === headKey && extra.length > 0}
      <button type="button" onclick={newer} disabled={paging}>Show newer decisions</button>
    {/if}
    {#if windowNote !== null}
      <p role="note">{windowNote}</p>
    {/if}
  {/if}
  {#if listError !== null}
    <p role="alert">{listError}</p>
  {/if}
</section>

<style>
  .pill {
    border: 1px solid #999;
    padding: 0 0.4rem;
    font-size: 0.8rem;
    margin-right: 0.4rem;
  }
  .trail {
    list-style: none;
    padding-left: 0;
  }
  .trail li {
    margin-bottom: 0.5rem;
    font-size: 0.9rem;
  }
  .text {
    overflow-wrap: anywhere;
  }
  .until,
  time {
    font-size: 0.8rem;
    margin-left: 0.4rem;
  }
  .help {
    font-size: 0.85rem;
  }
  fieldset {
    border: 1px solid #ccc;
    margin-bottom: 0.5rem;
  }
  label {
    display: block;
    margin: 0.4rem 0;
  }
  textarea {
    display: block;
    width: 100%;
    font: inherit;
  }
</style>
