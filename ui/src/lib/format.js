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

/** Human-readable reported-change names; unknown values remain inert text. */
export function changeLabel(kind) {
  return ({ weak_appearance: 'Weak sightings', weak_disappearance: 'Weak disappearances',
    port_closed: 'Ports closed', hostname_change: 'Hostname changes',
    os_refinement: 'OS refinements', other: 'Other changes' })[kind] ?? kind;
}

// Describe a reported confidence decrease only from paired event views.
// Missing evidence or unknown confidence never implies a decrease.
export function reviewTitle(kind, observations = [], references = []) {
  if (kind !== 'other') {
    return changeLabel(kind);
  }
  const ranks = new Map([['low', 0], ['medium', 1], ['high', 2]]);
  const previous = new Map();
  for (const obs of observations) {
    if (obs.view_role === 'previous' && obs.event_id) {
      previous.set(obs.event_id, ranks.get(obs.attributes?.identity_confidence));
    }
  }
  for (const obs of observations) {
    if (obs.view_role !== 'current') {
      continue;
    }
    const before = previous.get(obs.event_id);
    const after = ranks.get(obs.attributes?.identity_confidence);
    if (before !== undefined && after !== undefined && after < before) {
      return 'Reported identity confidence decreased';
    }
  }
  const types = new Set(references.map((event) => event.event_type));
  if (types.size === 1) {
    if (types.has('host.appeared')) {
      return 'New host reported';
    }
    if (types.has('host.disappeared')) {
      return 'Host disappearance reported';
    }
    if (types.has('host.changed')) {
      return 'Reported host details changed';
    }
  }
  return types.size > 1 ? 'Reported network changes' : 'Reported details changed';
}

/** Format the server's duration without changing the selected window. */
export function windowLabel(value) {
  const match = /^(\d+)h0m0s$/.exec(value);
  return match ? `${match[1]} hours` : value;
}
