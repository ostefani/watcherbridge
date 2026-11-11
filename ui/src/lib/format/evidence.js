// Evidence navigation keys: the contract between dossier sections
// and revealEvidence (../evidenceNavigation.js). Sections tag targets
// with data-evidence keys; buttons jump by key without changing the
// hash route. Keys are `event:<event_id>`, `step:<step>` or the
// literal reports key for the scanner-reports section.

export const REPORTS_KEY = 'reports';

/** @param {string} eventId */
export function eventKey(eventId) {
  return `event:${eventId}`;
}

/** @param {number|string} step */
export function stepKey(step) {
  return `step:${step}`;
}
