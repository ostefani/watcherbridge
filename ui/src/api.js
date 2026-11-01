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
