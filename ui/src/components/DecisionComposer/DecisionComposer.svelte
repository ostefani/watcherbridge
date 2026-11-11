<script>
  import { tick } from 'svelte';
  import styles from './DecisionComposer.module.css';

  // Decision composer: the operator's explicit choice as form draft
  // state (kind, texts, member inclusion) plus submit. Nothing submits
  // on its own: the card variant leaves no kind preselected and gates
  // on an explicit in-set choice, and the operator always presses
  // submit. Drafts clear only after the container reports success, so
  // a network failure keeps the form for a safe idempotent retry.
  let {
    kinds = [],
    subjectId,
    kindHelp = {},
    memberScope = false,
    memberCount = 0,
    variant = 'plain',
    defaultKind = null,
    pending = false,
    formError = null,
    outcomeNote = null,
    onSubmit = async () => {}
  } = $props();

  const card = $derived(variant === 'card');

  const KIND_HELP = {
    expected: 'No further action needed.',
    not_expected: 'Flag for follow-up: this briefing still needs a check.',
    snooze: 'Hide from attention until a time. Empty keeps the server default (24h).',
    note: 'Free-text note, kept with the audit (max 2000 characters).',
    label: 'Your label for this profile, never identity (max 256 characters).'
  };

  // chosen is the operator's explicit choice. Without a default the
  // kind remains null until the operator chooses. The derived reads
  // the prop each time so lint sees no stale capture.
  let chosen = $state(/** @type {string|null} */ (null));
  const kind = $derived(chosen ?? defaultKind);
  let untilText = $state('');
  let text = $state('');
  // Member-inclusion choice incident subjects require: unchecked
  // (incident-only) is the default, so acknowledgement never silently
  // takes members.
  let includeMembers = $state(false);
  let submitButton = $state(null);

  const needsText = $derived(kind === 'note' || kind === 'label');
  const textCap = $derived(kind === 'label' ? 256 : 2000);
  // Submittable only with an explicit kind inside the offered set; a
  // note/label additionally needs nonempty text. Only the card design
  // gates on it; other flows keep the pending-only gate.
  const canSubmit = $derived(
    kind !== null && kinds.includes(kind) && (!needsText || text.trim() !== '')
  );

  // Explicit preset callback for dossier shortcuts: adopt the kind
  // and hand focus to submit. Nothing submits without the operator.
  // The container exposes this through DecisionsContainer so the
  // shortcut registry calls the composer directly instead of driving
  // it through a preset prop and an effect. Focus waits a tick: on a
  // fresh briefing the submit button is disabled until the choice
  // lands, and focusing a disabled button is a silent no-op.
  // Makes no network calls itself — presetting posts nothing.
  export async function applyPreset(next) {
    if (!kinds.includes(next)) {
      return;
    }
    chosen = next;
    await tick();
    submitButton?.focus();
  }

  async function submit(/** @type {SubmitEvent} */ ev) {
    ev.preventDefault();
    if (pending) {
      return;
    }
    // Programmatic guard mirrors the disabled button: no implicit or
    // out-of-set kind ever submits from the card design.
    if (card && !canSubmit) {
      return;
    }
    if (kind === null) {
      return;
    }
    // The container reports 'ok'/'error' instead of throwing, so this
    // await never produces an unhandled rejection; drafts clear only
    // on 'ok', and a failed submit keeps the form for retry.
    const posted = await onSubmit({ kind, untilText, text, includeMembers });
    if (posted !== 'ok') {
      return;
    }
    chosen = null;
    untilText = '';
    text = '';
    includeMembers = false;
  }
</script>

<form class={card ? styles.composer : undefined} onsubmit={submit} aria-label="Record a decision">
  <fieldset>
    <legend class:sr-only={card}>Choose a decision</legend>
    {#each kinds as k (k)}
      <label class={kind === k ? styles.selected : undefined}>
        <input type="radio" name={`decision-kind-${subjectId}`} value={k} checked={kind === k} onchange={() => (chosen = k)} />
        <span class={styles.decisionLabel}>{k === 'not_expected' ? 'Not expected' : k.charAt(0).toUpperCase() + k.slice(1)}</span>
        {#if card}<span class={styles.decisionHint}>{({ expected: 'no further action', not_expected: 'investigate', snooze: 'hide until later', note: 'annotation only' })[k]}</span>{/if}
      </label>
    {/each}
  </fieldset>
  <p class="help">{kind === null ? 'Choose a verdict to record it.' : (kindHelp[kind] ?? KIND_HELP[kind] ?? '')}</p>
  {#if memberScope}
    <label>
      <input type="checkbox" bind:checked={includeMembers} />
      Also apply to the {memberCount} member briefing{memberCount === 1 ? '' : 's'} (snapshot now; later joiners stay untouched)
    </label>
  {/if}
  {#if outcomeNote !== null}
    <p role="status">{outcomeNote}</p>
  {/if}
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
  <button type="submit" bind:this={submitButton} disabled={card ? (pending || !canSubmit) : pending}>
    {pending ? 'Recording…' : 'Record decision'}
  </button>
</form>
