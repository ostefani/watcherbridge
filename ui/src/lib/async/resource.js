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
 * @template T
 * @param {() => Promise<T>} fetcher
 * @param {{initial?: T|null}} [options] snapshot rendered before any fetch
 */
export function createResource(fetcher, { initial = null } = {}) {
  const data = writable(initial);
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
