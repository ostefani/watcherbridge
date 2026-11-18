// App publishes its event stream to the component tree. Standalone
// renders have no provider and never open a connection.
import { setContext, getContext } from 'svelte';

const KEY = Symbol('watcherbridge.connection');

/** @typedef {ReturnType<import('./stream.svelte.js').createEventStream>} EventStream */

/** @param {EventStream} stream */
export function setConnection(stream) {
  setContext(KEY, stream);
}

/** @returns {EventStream|null} */
export function getConnectionOrNull() {
  return getContext(KEY) ?? null;
}
