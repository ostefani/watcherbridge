// Svelte context glue for the single event stream. The store object
// itself is framework-free (`./stream.js`); this file only publishes
// it to the component tree. Call `setConnection` once in App setup,
// `getConnection` in any screen — never a module-level singleton, so
// prerender stays connection-free.

import { setContext, getContext } from 'svelte';

const KEY = Symbol('watcherbridge.connection');

/** @param {ReturnType<import('./stream.js').createEventStream>} stream */
export function setConnection(stream) {
  setContext(KEY, stream);
}

/** @returns {ReturnType<import('./stream.js').createEventStream>} */
export function getConnection() {
  return getContext(KEY);
}
