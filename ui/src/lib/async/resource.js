// Single-item async resource with snapshot support. Replaces the
// initial/retired plumbing in screens: a snapshot fills `data` until
// (or instead of) a fetch, a failed refresh keeps data with a notice,
// and only a data-less failure becomes an error. One guard per
// resource; concurrent refreshes are latest-wins.
//
// Plain JS over svelte/store (no runes, no .svelte.js): the stores run
// uncompiled, so scripts/characterize.mjs covers this in plain node
// with no test runner. Components subscribe with `$data` as usual.

import { writable, get } from 'svelte/store';

/**
 * Fetch-owned data with a failed-refresh notice. The mount-time
 * snapshot (if any) stays a container concern: containers render
 * `data ?? snapshot`, so the resource never captures props at init.
 * @template T
 * @param {() => Promise<T>} fetcher
 */
export function createResource(fetcher) {
  const data = writable(null);
  const error = writable(null);
  const refreshError = writable(null);
  const loading = writable(false);
  const guard = { gen: 0 };

  async function refresh() {
    const my = (guard.gen += 1);
    refreshError.set(null);
    if (get(data) === null) {
      error.set(null);
      loading.set(true);
    }
    try {
      const fresh = await fetcher();
      if (my !== guard.gen) {
        return 'stale';
      }
      data.set(fresh);
      error.set(null);
      refreshError.set(null);
      return 'applied';
    } catch (err) {
      if (my !== guard.gen) {
        return 'stale';
      }
      const message = err instanceof Error ? err.message : String(err);
      if (get(data) === null) {
        error.set(message);
      } else {
        refreshError.set(message);
      }
      return 'failed';
    } finally {
      if (my === guard.gen) {
        loading.set(false);
      }
    }
  }

  function invalidate() {
    guard.gen += 1;
    loading.set(false);
  }

  return { data, error, refreshError, loading, refresh, invalidate };
}

/**
 * Which failure (if any) is visible next to rendered content. A
 * data-less failure renders as the error state, never a notice; a
 * snapshot counts as rendered content, so its failures must surface
 * too — otherwise a failed refresh on a snapshot screen is silent.
 * @param {{data: unknown, initial: unknown, error: string|null, refreshError: string|null}} state
 * @returns {string|null} notice text, or null when nothing may show
 */
export function failureNotice({ data = null, initial = null, error = null, refreshError = null } = {}) {
  if ((data ?? initial) === null) {
    return null;
  }
  return refreshError ?? (data === null ? error : null);
}
