import { nextPageRows } from '@/lib/paging.js';
import { toErrorMessage } from '@/lib/format.js';
import { latestOnly } from '@/lib/latestOnly.js';

// A successful reset retires appends against the old cursor. A failed
// reset leaves those appends eligible to commit against the retained rows.
/**
 * @template T
 * @param {{
 *   fetchPage: (cursor: string) => Promise<{rows: T[], nextAfter: string}>,
 *   keyOf: (row: T) => string | number,
 *   cap: number,
 *   initial?: {rows: T[], nextAfter: string} | (() => ({rows: T[], nextAfter: string} | null)) | null
 * }} options
 */
export function createPager({ fetchPage, keyOf, cap, initial = null }) {
  const snap = typeof initial === 'function' ? initial() : initial;
  let state = $state.raw({
    rows: snap ? [...snap.rows] : [],
    cursor: snap?.nextAfter ?? '',
    started: snap != null,
    failure: null
  });
  const resets = latestOnly();
  const pages = latestOnly();

  async function loadMore(reset = false) {
    const guard = reset ? resets : pages;
    const token = guard.start();
    state = { ...state, failure: null };
    try {
      const page = await fetchPage(reset ? '' : state.cursor);
      if (!guard.current(token)) {
        return 'stale';
      }
      const base = reset ? [] : state.rows;
      const fresh = nextPageRows(base, page.rows, keyOf, cap);
      if (reset) {
        pages.invalidate();
      }
      state = {
        rows: [...base, ...fresh],
        cursor: page.nextAfter ?? '',
        started: true,
        failure: reset ? null : state.failure
      };
      return 'applied';
    } catch (err) {
      if (!guard.current(token)) {
        return 'stale';
      }
      state = { ...state, failure: { message: toErrorMessage(err), reset } };
      return 'failed';
    }
  }

  function invalidate() {
    resets.invalidate();
    pages.invalidate();
  }

  function replace(rows, cursor = '') {
    invalidate();
    state = { rows: [...rows], cursor: cursor ?? '', started: true, failure: null };
  }

  return {
    get rows() { return state.rows; },
    get started() { return state.started; },
    get exhausted() { return state.started && state.cursor === ''; },
    get capped() { return state.cursor !== '' && state.rows.length >= cap; },
    get failure() { return state.failure; },
    get error() { return state.failure?.message ?? null; },
    loadMore, reset: () => loadMore(true), replace, invalidate
  };
}
