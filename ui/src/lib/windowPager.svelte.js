import { latestOnly } from '@/lib/latestOnly.js';
import { toErrorMessage } from '@/lib/format.js';

const PAGE_LIMIT = 50;
const BACK_PAGES = 4;
const DROPPED_NOTE = 'Earlier pages dropped from this view; the full audit remains available through the API.';
const EMPTY = { rows: [], back: [], exhausted: false, note: null, paging: false, error: null };

export function createWindowPager({ getHead, getSubject, fetchPage }) {
  const guard = latestOnly();
  const head = $derived(getHead());
  // Equivalent refreshes keep the window. A different subject, leading
  // decision or continuation boundary retires it and its pending page.
  const key = $derived(JSON.stringify([getSubject(), head[0]?.decision_id, head.length, head.at(-1)?.cursor]));
  let selected = $state.raw(null);
  const view = $derived(selected?.key === key ? selected : EMPTY);
  const trail = $derived(view.rows.length > 0 ? [...head, ...view.rows] : head);

  async function older() {
    if (view.paging || view.exhausted || trail.length === 0) {
      return;
    }
    const token = guard.start();
    const requestedKey = key;
    const before = trail.at(-1).cursor;
    const snapshot = view;
    selected = { ...snapshot, key: requestedKey, paging: true, error: null };
    try {
      const rows = await fetchPage({ limit: PAGE_LIMIT, before });
      if (!guard.current(token) || key !== requestedKey) {
        return;
      }
      if (rows.length === 0) {
        selected = { ...selected, exhausted: true, paging: false };
        return;
      }
      const back = snapshot.rows.length === 0 ? snapshot.back : [...snapshot.back, snapshot.rows];
      selected = {
        ...selected,
        rows,
        back: back.slice(-BACK_PAGES),
        note: back.length > BACK_PAGES ? DROPPED_NOTE : snapshot.note,
        exhausted: rows.length < PAGE_LIMIT,
        paging: false
      };
    } catch (err) {
      if (guard.current(token) && key === requestedKey) {
        selected = { ...selected, error: toErrorMessage(err), paging: false };
      }
    } finally {
      if (guard.current(token) && key !== requestedKey) {
        // Retire the old pending window even if its head later returns.
        reset();
      }
    }
  }

  function newer() {
    if (view.paging || view.rows.length === 0) {
      return;
    }
    selected = { ...view, rows: view.back.at(-1) ?? [], back: view.back.slice(0, -1), exhausted: false };
  }

  function reset() {
    guard.invalidate();
    selected = null;
  }

  function clearError() {
    if (view.error !== null) {
      selected = { ...view, error: null };
    }
  }

  return {
    get trail() { return trail; },
    get showNewer() { return view.rows.length > 0; },
    get exhausted() { return view.exhausted; },
    get note() { return view.note; },
    get paging() { return view.paging; },
    get error() { return view.error; },
    older, newer, reset, clearError
  };
}
