// SSE connection lifecycle for GET /events. Transport only: connection
// state, overview frames and an incident revision counter. Parsing
// failures degrade to the next frame; refresh ownership stays with the
// screens, which subscribe to `incidentRevision` for refetch/markStale.
// No module singletons: App creates one instance and shares it through
// `./context.js`, so prerender never connects.
//
// `open`/`incident`/`resync` all mean "incident state may have
// changed". Screens subscribe to `incidentRevision` from context, so
// no window-event bus remains.

import { writable } from 'svelte/store';

export const INCIDENT_KINDS = ['open', 'incident', 'resync'];

/**
 * @param {object} [options]
 * @param {string} [options.url] stream URL
 * @param {number} [options.retryMs] reconnect delay after an error
 * @param {(url: string) => EventSource} [options.createSource] transport factory (fake in tests)
 * @param {(fn: () => void, ms: number) => unknown} [options.schedule] retry scheduler (manual in tests)
 * @param {(t: unknown) => void} [options.cancelSchedule]
 * @param {(overview: unknown) => void} [options.onOverview] valid overview frames only
 * @param {(kind: string) => void} [options.onIncident] every open/incident/resync frame
 */
export function createEventStream({
  url = '/events',
  retryMs = 2000,
  createSource = (u) => new EventSource(u),
  schedule = (fn, ms) => setTimeout(fn, ms),
  cancelSchedule = (t) => clearTimeout(t),
  onOverview = () => {},
  onIncident = () => {}
} = {}) {
  const connection = writable('connecting');
  const incidentRevision = writable(0);
  let source = null;
  let timer = null;
  // Generation fences late callbacks after disconnect/reconnect, the
  // same stale-continuation rule screens apply to fetches.
  let gen = 0;

  function cleanup() {
    gen += 1;
    if (timer !== null) {
      cancelSchedule(timer);
      timer = null;
    }
    if (source !== null) {
      source.onopen = null;
      source.onmessage = null;
      source.onerror = null;
      for (const kind of [...INCIDENT_KINDS, 'overview']) {
        source.removeEventListener?.(kind, listeners[kind]);
      }
      source.close();
      source = null;
    }
  }

  // Named listeners so cleanup removes exactly what connect added.
  const listeners = {};
  for (const kind of INCIDENT_KINDS) {
    listeners[kind] = () => {
      incidentRevision.update((n) => n + 1);
      onIncident(kind);
    };
  }
  listeners.overview = (/** @type {MessageEvent} */ ev) => {
    try {
      onOverview(JSON.parse(ev.data));
    } catch {
      // Malformed frame degrades to the next refresh, never a crash.
    }
  };

  function connect() {
    cleanup();
    const my = gen;
    connection.set('connecting');
    const next = createSource(url);
    source = next;
    next.onopen = () => {
      if (my === gen) {
        connection.set('live');
      }
    };
    for (const kind of [...INCIDENT_KINDS, 'overview']) {
      next.addEventListener(kind, listeners[kind]);
    }
    next.onerror = () => {
      if (my !== gen) {
        return;
      }
      cleanup();
      connection.set('retrying');
      const mine = gen;
      timer = schedule(() => {
        if (mine === gen) {
          connect();
        }
      }, retryMs);
    };
  }

  function disconnect() {
    cleanup();
  }

  return { connection, incidentRevision, connect, disconnect };
}
