// Scoped keyboard shortcut registry. Screens register their keys with
// a human label; the help panel is generated from the same table, so
// documented and working shortcuts cannot drift apart. Dispatch stays
// silent while typing and never claims modifier combos. Multi-key
// sequences (`g h`, `g d`) stay in the App shell — they are navigation,
// not screen actions.

import { typingTarget } from '@/lib/roving.js';

export function createShortcutRegistry() {
  // scope -> key -> { label, handler }
  const bindings = new Map();

  /**
   * @param {string} scope e.g. 'global', 'briefing'
   * @param {string} key single-key value, e.g. 'j', '?', 'e'
   * @param {string} label help-panel wording
   * @param {(ev: KeyboardEvent) => void} handler
   */
  function register(scope, key, label, handler) {
    if (!bindings.has(scope)) {
      bindings.set(scope, new Map());
    }
    bindings.get(scope).set(key, { label, handler });
  }

  /**
   * @param {string} scope
   * @param {KeyboardEvent} ev
   * @returns {boolean} true when a binding handled the event
   */
  function dispatch(scope, ev) {
    if (typingTarget(ev) || ev.metaKey || ev.ctrlKey || ev.altKey) {
      return false;
    }
    const binding = bindings.get(scope)?.get(ev.key);
    if (!binding) {
      return false;
    }
    binding.handler(ev);
    return true;
  }

  /** @param {string} scope */
  function describe(scope) {
    return [...(bindings.get(scope)?.entries() ?? [])].map(([key, binding]) => ({ key, label: binding.label }));
  }

  return { register, dispatch, describe };
}
