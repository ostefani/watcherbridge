// Cursor pager with merge-by-key, a row cap and per-operation fencing.
// resetGen orders reloads; pageGen invalidates appends. A reload
// landing after a page start still applies (the BUG-1 case), but a
// committed reset retires every older append, so a late page cannot
// reopen an exhausted list. Only a successful commit clears the error
// slot and voids pending appends: a failed reset changes nothing, so
// a pending append against the still-valid cursor lands normally.
//
// Unlike createResource, the pager seeds from the mount-time snapshot:
// cursor and merge live here, so the container passes `initial` once and
// never rewrites it. A reset retires the snapshot by replacing rows.
// `initial` also accepts a thunk returning the snapshot: .svelte callers
// must read the prop inside a closure, or the compiler's init-capture
// rule treats it as a stale local capture.

import { writable, derived, get } from 'svelte/store';
import { nextPageRows } from '@/lib/paging.js';

/**
 * @template T
 * @param {{fetchPage: (cursor: string) => Promise<{rows: T[], nextAfter: string}>, keyOf: (row: T) => string|number, cap: number, initial?: {rows: T[], nextAfter: string}|(() => ({rows: T[], nextAfter: string}|null))|null}} options
 */
export function createPager({ fetchPage, keyOf, cap, initial = null }) {
  const snap = typeof initial === 'function' ? initial() : initial;
  const rows = writable(snap ? [...snap.rows] : []);
  const cursor = writable(snap ? (snap.nextAfter ?? '') : '');
  const started = writable(snap !== null);
  const failure = writable(/** @type {{message: string, reset: boolean}|null} */ (null));
  const error = derived(failure, ($failure) => $failure?.message ?? null);
  const loadingMore = writable(false);
  const exhausted = derived([cursor, started], ([$cursor, $started]) => $started && $cursor === '');
  const capped = derived([rows, cursor], ([$rows, $cursor]) => $cursor !== '' && $rows.length >= cap);
  let resetGen = 0;
  let pageGen = 0;

  async function loadMore(reset = false) {
    const my = reset ? (resetGen += 1) : (pageGen += 1);
    const gen = () => (reset ? resetGen : pageGen);
    failure.set(null);
    loadingMore.set(true);
    try {
      const page = await fetchPage(reset ? '' : get(cursor));
      if (my !== gen()) {
        return 'stale';
      }
      const base = reset ? [] : get(rows);
      const fresh = nextPageRows(base, page.rows, keyOf, cap);
      rows.set(reset ? fresh : [...base, ...fresh]);
      cursor.set(page.nextAfter ?? '');
      started.set(true);
      if (reset) {
        // A committed reset owns the list now: clear the error slot
        // (a previous page failure refers to the old state) and retire
        // every append requested against an older cursor, so a late
        // page cannot reopen an exhausted list. Appends requested after
        // this commit use the new cursor and are unaffected.
        failure.set(null);
        pageGen += 1;
      }
      return 'applied';
    } catch (err) {
      if (my !== gen()) {
        return 'stale';
      }
      failure.set({ message: err instanceof Error ? err.message : String(err), reset });
      return 'failed';
    } finally {
      if (my === gen()) {
        loadingMore.set(false);
      }
    }
  }

  function reset() {
    return loadMore(true);
  }

  // Replace every row outright (profile reload with embedded history):
  // voids in-flight pages, clears the error and restarts from the new
  // cursor. Failures never reach here — only applied data replaces.
  function replace(nextRows, nextAfter = '') {
    resetGen += 1;
    pageGen += 1;
    failure.set(null);
    loadingMore.set(false);
    rows.set([...nextRows]);
    cursor.set(nextAfter ?? '');
    started.set(true);
  }

  return { rows, cursor, started, exhausted, capped, failure, error, loadingMore, loadMore, reset, replace };
}
