// Reconnect-edge decision: refresh when the stream comes (back) live
// and there is rendered content — fetched data or a mounted snapshot.
// A snapshot is renderable state, not a reason to stay stale. Pure so
// it is unit-tested; the container owns the wasLive tracking.

/**
 * @param {{live: boolean, wasLive: boolean, data: unknown, initial: unknown}} state
 */
export function shouldRefreshOnReconnect({ live, wasLive, data, initial }) {
  return live && !wasLive && (data ?? initial) !== null;
}
