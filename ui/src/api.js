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
