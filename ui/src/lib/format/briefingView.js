// Pure briefing dossier derivations (Designv9 §8.2). No markup, no
// fetch: BriefingContainer calls these, and scripts/characterize.mjs
// covers them in plain node.

/**
 * The priority comparison opens when the reader needs it: equal
 * baseline, suggestion and final stay collapsed; a difference or a
 * clamp opens the detail.
 * @param {{clamped?: boolean, baseline?: string, priority?: string, model_suggestion?: string|null}|null} shown
 */
export function priorityDiffers(shown) {
  return shown !== null &&
    (shown.clamped || shown.baseline !== shown.priority ||
      (shown.model_suggestion ?? shown.baseline) !== shown.priority);
}

/**
 * Lookup steps that could not establish retained context: unknown
 * subjects, blocked or failed lookups stay explicit limitations.
 * @param {Array<{outcome?: string}>} traces
 */
export function lookupLimits(traces) {
  return (traces ?? []).filter((step) =>
    ['unknown_subject', 'blocked', 'error', 'unavailable', 'truncated'].includes(step.outcome));
}

/**
 * Group tool steps into lookup receipts by their summary, preserving
 * first-seen order.
 * @param {Array<{summary?: string, step?: number}>} traces
 */
export function lookupReceipts(traces) {
  const groups = [];
  for (const step of traces ?? []) {
    const key = step.summary || `step:${step.step}`;
    let group = groups.find((receipt) => receipt.key === key);
    if (!group) {
      group = { key, summary: step.summary, steps: [] };
      groups.push(group);
    }
    group.steps.push(step.step);
  }
  return groups;
}

/**
 * The single-view identity grid renders only when every observation
 * and event is retained; a truncated dossier falls back to per-report
 * views with an explicit notice.
 * @param {{observations_truncated?: boolean, referenced_truncated?: boolean}|null} shown
 */
export function identityComplete(shown) {
  return shown !== null && !shown.observations_truncated && !shown.referenced_truncated;
}
