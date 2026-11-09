// Cursor pager with merge-by-key, a row cap and per-operation fencing.
// Reset (reload) and append (load-more) run on independent tokens, so a
// reload landing after a page start still applies — the BUG-1 case.
// Failures preserve existing rows; only the error slot changes.

import { writable, derived, get } from 'svelte/store';
import { nextPageRows } from '@/lib/paging.js';

/**
 * @template T
 * @param {{fetchPage: (cursor: string) => Promise<{rows: T[], nextAfter: string}>, keyOf: (row: T) => string|number, cap: number}} options
 */
export function createPager({ fetchPage, keyOf, cap }) {
  const rows = writable([]);
  const cursor = writable('');
  const started = writable(false);
  const error = writable(null);
  const loadingMore = writable(false);
  const exhausted = derived([cursor, started], ([$cursor, $started]) => $started && $cursor === '');
  const capped = derived([rows, cursor], ([$rows, $cursor]) => $cursor !== '' && $rows.length >= cap);
  let resetGen = 0;
  let pageGen = 0;

  async function loadMore(reset = false) {
    const my = reset ? (resetGen += 1) : (pageGen += 1);
    const gen = () => (reset ? resetGen : pageGen);
    error.set(null);
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
      return 'applied';
    } catch (err) {
      if (my !== gen()) {
        return 'stale';
      }
      error.set(err instanceof Error ? err.message : String(err));
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

  return { rows, cursor, exhausted, capped, error, loadingMore, loadMore, reset };
}
