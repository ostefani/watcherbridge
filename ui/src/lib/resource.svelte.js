import { untrack } from 'svelte';
import { toErrorMessage } from '@/lib/format.js';
import { latestOnly } from '@/lib/latestOnly.js';

// Fetch state is independent of the caller's display snapshot.
export function createResource(fetcher, initial = () => null) {
  const guard = latestOnly();
  const empty = { data: null, error: null, refreshError: null, loading: false };
  let state = $state.raw(empty);
  const shown = $derived(state.data ?? initial());
  const notice = $derived(shown === null ? null : (state.refreshError ?? state.error));

  async function refresh() {
    const token = guard.start();
    const { data } = state;
    state = { ...empty, data, loading: data === null };
    try {
      const fresh = await fetcher();
      if (!guard.current(token)) {
        return 'stale';
      }
      state = { ...empty, data: fresh };
      return 'applied';
    } catch (err) {
      if (!guard.current(token)) {
        return 'stale';
      }
      const message = toErrorMessage(err);
      state = data === null ? { ...empty, error: message } : { ...empty, data, refreshError: message };
      return 'failed';
    }
  }

  function invalidate() {
    guard.invalidate();
    state = { ...state, loading: false };
  }

  function clear() {
    guard.invalidate();
    state = empty;
  }

  return {
    get data() { return state.data; },
    get error() { return state.error; },
    get loading() { return state.loading; },
    get shown() { return shown; },
    get notice() { return notice; },
    refresh, invalidate, clear
  };
}

// A new identity gets its own fetch state immediately. The effect
// owns initial loading and retires requests for the previous identity.
export function createSelectedResource(getId, fetcher, initial) {
  const resource = $derived.by(() => {
    const id = getId();
    return createResource(() => fetcher(id), initial);
  });

  $effect(() => {
    const selected = resource;
    if (initial() === null) {
      untrack(selected.refresh);
    }
    return selected.invalidate;
  });

  return () => resource;
}
