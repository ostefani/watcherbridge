// Paging merge helpers: cursor pages over mutable orderings (the
// backend sorts by last_report_at) can repeat rows across pages, so
// appends merge by key and keep first-seen positions. Pure, so the
// smoke script asserts them directly in node.

/**
 * Append page rows to the current list, dropping keys already shown
 * and stopping at the cap. First-seen positions never move.
 * @template T
 * @param {T[]} current rows already displayed
 * @param {T[]} page rows from the latest page
 * @param {(row: T) => string|number} keyOf stable row identity
 * @param {number} cap maximum total rows
 * @returns {T[]} rows to append (possibly empty)
 */
export function nextPageRows(current, page, keyOf, cap) {
  const seen = new Set(current.map(keyOf));
  const out = [];
  for (const row of page) {
    if (current.length + out.length >= cap) {
      break;
    }
    if (!seen.has(keyOf(row))) {
      seen.add(keyOf(row));
      out.push(row);
    }
  }
  return out;
}
