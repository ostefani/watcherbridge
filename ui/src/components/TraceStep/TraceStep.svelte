<script>
  import styles from './TraceStep.module.css';
  // One executed investigator tool step. Outcome and summary are the
  // server's bounded display classification (Go owns it); the raw
  // status, args and result ride untouched beside them. A completed
  // transport is never presented as successful evidence on its own:
  // unknown subjects, blocks, errors and partial results each read
  // distinctly.
  let { step } = $props();

  const anchor = $derived(`step-${step.step}`);
  const headline = $derived(
    step.summary ?? (step.outcome === 'unknown_subject'
      ? 'The investigator could not resolve this lookup to a permitted reported subject; history remains unknown.'
      : step.status === 'blocked'
        ? 'Redaction blocked this result; the lookup did not complete.'
        : step.status === 'error'
          ? 'Tool execution failed; result unavailable.'
          : 'No interpreted result is available.')
  );
</script>

<li class={styles.step} id={anchor} data-evidence={`step:${step.step}`} tabindex="-1">
  <span class={`mono ${styles.toolName}`}>{step.tool}</span>
  {#if step.outcome}<span class="pill">{step.outcome.replaceAll('_', ' ')}</span>{/if}
  <span class="section-note">step {step.step} · {step.status} · {step.duration_ms} ms</span>
  <p>{headline}</p>
  <details>
    <summary>Arguments and tokenized result</summary>
    <pre>Args: {step.args}</pre>
    <pre>Result: {step.result}</pre>
  </details>
</li>

