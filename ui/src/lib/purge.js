// Purge run notice logic: a "completed" ledger row can still carry
// error text (skipped token expiry records completed-with-note), so
// status alone never proves clean work. Pure, so the smoke script
// asserts it directly in node.

/**
 * True when the latest purge attempt needs operator attention: any
 * non-completed status, or a completed run carrying error text.
 * The "latest clean purge" claim belongs to last_success alone.
 * @param {{status?: string, error?: string}|null|undefined} last
 */
export function latestRunNeedsAttention(last) {
  if (last === null || last === undefined) {
    return false;
  }
  return last.status !== 'completed' || (last.error ?? '') !== '';
}
