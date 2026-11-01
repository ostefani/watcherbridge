// Overview API client. Window validation lives server-side: an invalid
// or oversized window is a 400 the caller surfaces as an error state,
// never clamped or widened silently.

/**
 * @returns {Promise<import('./wire.js').Overview>}
 */
export async function fetchOverview(window = '24h') {
  const res = await fetch(`/api/overview?window=${encodeURIComponent(window)}`);
  if (!res.ok) {
    throw new Error(`overview ${res.status}`);
  }
  return res.json();
}
