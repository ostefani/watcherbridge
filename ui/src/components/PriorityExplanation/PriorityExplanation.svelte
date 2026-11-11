<script>
  // Priority explanation: baseline floor, model suggestion and final
  // priority with the one-step raise limit. The comparison opens when
  // the reader needs it: equal values stay collapsed.
  let {
    priority,
    priorityReason,
    baseline,
    baselineRule = null,
    modelSuggestion = null,
    clamped = false,
    fallbackUsed = false,
    differs = false
  } = $props();
</script>

<section class="dossier-card" aria-label="Why this priority">
  <h3>Why this priority</h3>
  <p class="priority-reason">{priorityReason}</p>
  <div class="priority-rule"><span class="pill priority" data-priority={priority}>Final priority: {priority}</span>{#if baselineRule}<code>{baselineRule}</code>{/if}</div>
  <details class="priority-comparison" open={differs}>
    <summary>How the priority was computed</summary>
    <ol class="stepper">
      <li>Baseline floor <strong>{baseline}</strong></li>
      <li>Model suggestion <strong>{modelSuggestion ?? 'No model suggestion — baseline stands'}</strong></li>
      <li>Final priority <strong>{priority}</strong></li>
    </ol>
    {#if clamped}<p class="section-note">Suggestion adjusted to respect the baseline floor and one-step raise limit.</p>{/if}
  </details>
  {#if fallbackUsed}<p class="section-note">Deterministic fallback text: the model path did not produce this briefing.</p>{/if}
</section>
