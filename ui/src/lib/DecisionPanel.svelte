<script>
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
  // Older pages past the embedded trail, fetched on demand. A parent
  // reload replaces the embedded head, so extra pages reset then.
  let extra = $state([]);
  let paging = $state(false);
  let exhausted = $state(false);
  const trail = $derived([...decisions, ...extra]);

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
      exhausted = false;
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
      exhausted = false;
      await reload();
    } catch (err) {
      listError = err instanceof Error ? err.message : String(err);
    } finally {
      undoPending = null;
    }
  }

  async function older() {
    if (paging || exhausted || trail.length === 0) {
      return;
    }
    paging = true;
    listError = null;
    try {
      const last = trail[trail.length - 1];
      const page = await fetchDecisions(subjectKind, subjectId, { before: last.cursor });
      if (page.length === 0) {
        exhausted = true;
      } else {
        extra = [...extra, ...page];
      }
    } catch (err) {
      listError = err instanceof Error ? err.message : String(err);
    } finally {
      paging = false;
    }
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
    {#if !exhausted}
      <button type="button" onclick={older} disabled={paging}>
        {paging ? 'Loading…' : 'Show older decisions'}
      </button>
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
