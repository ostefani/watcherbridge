// Shared display helpers: local time with a UTC tooltip, and plain
// class wording. Scanner/model/operator text is bound as text, never
// HTML, at every call site.

/**
 * Format an RFC 3339 instant for local display.
 * @param {string} [iso]
 * @returns {{ local: string, utc: string }} "unknown" when unparseable.
 */
export function displayTime(iso) {
  const d = new Date(iso ?? '');
  if (Number.isNaN(d.getTime())) {
    return { local: 'unknown', utc: 'unknown' };
  }
  return { local: d.toLocaleString(), utc: d.toISOString() };
}

/**
 * One-line source badge wording for a briefing source value.
 * @param {string} source
 */
export function sourceLabel(source) {
  switch (source) {
    case 'rules':
      return 'Deterministic rules';
    case 'model':
      return 'Model suggestion';
    case 'agent':
      return 'Investigator agent';
    case 'fallback':
      return 'Deterministic fallback';
    case 'legacy':
      return 'Earlier version (source unknown)';
    default:
      return 'Unknown source';
  }
}

/**
 * Operator-facing wording for a reported-subject class.
 * @param {string} cls
 */
export function classLabel(cls) {
  switch (cls) {
    case 'reported_device':
      return 'Reported device';
    case 'address':
      return 'Address context — not a device identity';
    case 'event':
      return 'Unresolved events — not a device identity';
    case 'legacy':
      return 'Legacy profile';
    default:
      return 'Unknown class';
  }
}
