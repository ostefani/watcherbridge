// Overview API client. Window validation lives server-side: an invalid
// or oversized window is a 400 the caller surfaces as an error state,
// never clamped or widened silently.

/**
 * Window is intentionally not a client parameter: the request omits it
 * so the server default applies, matching the window of the `overview`
 * stream frames. A server-side window change never desyncs the two.
 * @returns {Promise<import('./wire.js').Overview>}
 */
export async function fetchOverview() {
  const res = await fetch('/api/overview');
  if (!res.ok) {
    throw new Error(`overview ${res.status}`);
  }
  return res.json();
}

async function fetchJSON(path, label) {
  const res = await fetch(path);
  if (!res.ok) {
    throw new Error(`${label} ${res.status}`);
  }
  return res.json();
}

/**
 * @param {string} id briefing or group ID; the server validates it
 * @returns {Promise<import('./wire.js').BriefingDetail>}
 */
export function fetchBriefing(id) {
  return fetchJSON(`/api/briefing?id=${encodeURIComponent(id)}`, 'briefing');
}

/**
 * Append an operator decision. The server is the matrix authority:
 * unknown kinds, text where none belongs, and past snooze horizons
 * come back as 4xx the caller surfaces. requestId makes a retry after
 * a network failure replay-safe; rotate it after a success.
 * @param {{subjectKind: string, subjectId: string, kind: string, until?: string, text?: string, requestId: string}} d
 * @returns {Promise<{decision: import('./wire.js').Decision, applied: boolean, reason?: string}>}
 */
export async function postDecision(d) {
  const body = {
    subject_kind: d.subjectKind,
    subject_id: d.subjectId,
    kind: d.kind,
    request_id: d.requestId
  };
  if (d.until !== undefined) {
    body.until = d.until;
  }
  if (d.text !== undefined) {
    body.text = d.text;
  }
  const res = await fetch('/api/decisions', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  });
  if (!res.ok) {
    throw new Error(`decision ${res.status}: ${await res.text()}`);
  }
  return res.json();
}

/**
 * Undo one decision by ID. Undoing a stale label head is a 409, never
 * a silent overwrite; the caller surfaces the status.
 * @param {string} id decision ID
 * @param {string} requestId retry key, rotated after success
 */
export async function postUndo(id, requestId) {
  const res = await fetch(`/api/decisions/${encodeURIComponent(id)}/undo`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ request_id: requestId })
  });
  if (!res.ok) {
    throw new Error(`undo ${res.status}: ${await res.text()}`);
  }
  return res.json();
}

/**
 * @param {string} after tuple cursor, "" starts at the oldest
 * @returns {Promise<import('./wire.js').DevicesPage>}
 */
export function fetchDevices(after = '') {
  const q = after === '' ? '' : `?after=${encodeURIComponent(after)}`;
  return fetchJSON(`/api/devices${q}`, 'devices');
}

/**
 * @param {string} id profile ID; the server validates it
 * @param {string} historyAfter tuple cursor, "" starts at the oldest
 * @returns {Promise<import('./wire.js').DeviceProfile>}
 */
export function fetchDevice(id, historyAfter = '') {
  const q = historyAfter === '' ? '' : `?history_after=${encodeURIComponent(historyAfter)}`;
  return fetchJSON(`/api/devices/${encodeURIComponent(id)}${q}`, 'device');
}

/**
 * @returns {Promise<import('./wire.js').Health>}
 */
export function fetchHealth() {
  return fetchJSON('/api/health', 'health');
}
