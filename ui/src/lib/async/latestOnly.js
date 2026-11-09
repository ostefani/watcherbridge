// Latest-wins guard for one async operation. Each operation (reload,
// load-more, decision page, …) owns an instance, so intents never
// cancel each other — the flaw BUG-1 pins in screens that share a
// single guard. Stream-frame fencing lives in lib/events, not here.

/**
 * @returns {{start: () => number, current: (token: number) => boolean, invalidate: () => void}}
 */
export function latestOnly() {
  let gen = 0;
  return {
    // Call when starting work; keep the token for completion.
    start() {
      gen += 1;
      return gen;
    },
    // True only when no newer work started (or invalidated) since.
    current(token) {
      return token === gen;
    },
    // Void every in-flight token: unmounts, route changes, resets.
    invalidate() {
      gen += 1;
    }
  };
}
