// Fetch/stream freshness guard. Startup fires a fetch, stream opening
// fires another, and overview frames arrive in between; without fencing,
// a stale completion can restore an outdated verdict, while a stale
// failure can hide a valid overview behind the error screen while
// hash-change-only SSE leaves that state unchanged indefinitely.
export function createFetchGuard() {
  let latest = 0;
  let streamGen = 0;
  return {
    // Call when starting a fetch; keep the token for completion.
    start() {
      latest += 1;
      return { id: latest, stream: streamGen };
    },
    // Call when an overview stream frame is applied.
    streamed() {
      streamGen += 1;
    },
    // True when this completion is still the newest fetch and no stream
    // frame arrived after it started.
    current(token) {
      return token.id === latest && token.stream === streamGen;
    }
  };
}
