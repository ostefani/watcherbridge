// Roving focus for row lists: j/k move between rows wherever the
// controls exist, Enter activates natively. Rows are anchors, so Tab
// and screen readers already work; this only adds the shortcuts the
// help lists. Ignored inside inputs and editable content.

/**
 * Move focus one row in the given direction.
 * @param {1 | -1} dir
 * @returns {boolean} true when focus moved.
 */
export function moveRowFocus(dir) {
  const active = document.activeElement;
  const container = active?.closest?.('[data-rows]');
  if (!container) {
    return false;
  }
  const rows = [...container.querySelectorAll('[data-row]')];
  if (rows.length === 0) {
    return false;
  }
  let i = rows.indexOf(active);
  if (i === -1) {
    i = dir === 1 ? -1 : 0;
  }
  const next = rows[i + dir] ?? (dir === 1 ? rows[0] : rows[rows.length - 1]);
  next.focus();
  return true;
}

/**
 * True when a key event targets typing content, where single-letter
 * shortcuts must stay silent.
 * @param {KeyboardEvent} ev
 */
export function typingTarget(ev) {
  const el = /** @type {HTMLElement|null} */ (ev.target);
  if (el === null) {
    return true;
  }
  if (el.isContentEditable) {
    return true;
  }
  const tag = el.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT';
}
