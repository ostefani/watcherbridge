/** Open saved evidence inside the current dossier without changing its hash route. */
export function revealEvidence(root, key) {
  if (!root) return false;
  const target = Array.from(root.querySelectorAll('[data-evidence]'))
    .find((node) => node.dataset.evidence === key);
  if (!target) return false;
  for (let node = target; node && node !== root; node = node.parentElement) {
    if (node.tagName === 'DETAILS') node.open = true;
  }
  target.scrollIntoView({ block: 'nearest' });
  target.focus({ preventScroll: true });
  return true;
}
