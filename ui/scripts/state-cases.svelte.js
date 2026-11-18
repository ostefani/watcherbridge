import { flushSync } from 'svelte';
import { createSelectedResource } from '@/lib/resource.svelte.js';
import { watchIncidents, watchLive } from '@/lib/stream.svelte.js';
import { createWindowPager } from '@/lib/windowPager.svelte.js';

export async function checkSelectedResource(check) {
  const pending = Object.create(null);
  let id = $state('first');
  let snapshot = $state(null);
  let resource;
  let unmounted;
  const dispose = $effect.root(() => {
    resource = createSelectedResource(() => id, (key) => new Promise((resolve) => {
      pending[key] = resolve;
    }), () => snapshot);
  });
  try {
    flushSync();
    check('briefing selection starts its client fetch', Boolean(pending.first) && resource().loading);
    const first = resource();
    id = 'second';
    check('briefing selection immediately switches resources', resource() !== first && resource().shown === null);
    flushSync();
    pending.first('late briefing');
    pending.second('selected briefing');
    await Promise.resolve();
    check('old briefing completion is fenced', first.data === null && resource().shown === 'selected briefing');

    flushSync(() => {
      id = 'snapshot';
      snapshot = 'saved briefing';
    });
    check('briefing snapshot renders without an automatic fetch',
      resource().shown === 'saved briefing' && !pending.snapshot);

    flushSync(() => {
      id = 'unmounted';
      snapshot = null;
    });
    unmounted = resource();
  } finally {
    dispose();
  }
  pending.unmounted('completion after unmount');
  await Promise.resolve();
  check('unmounted briefing cannot apply a pending fetch', unmounted.data === null && !unmounted.loading);
}

export function checkStreamWatchers(check) {
  const stream = $state({ connection: 'live', incidentRevision: 10 });
  let live = 0;
  let incidents = 0;
  let incidental = $state(0);
  let unprovided = 0;
  const dispose = $effect.root(() => {
    watchLive(stream, () => { live += incidental + 1; });
    watchIncidents(stream, () => { incidents += 1; });
    watchLive(null, () => { unprovided += 1; });
    watchIncidents(null, () => { unprovided += 1; });
  });
  flushSync();
  check('already-live mount observes one live edge and no old incident', live === 1 && incidents === 0);
  flushSync(() => { incidental = 1; });
  check('watcher callbacks do not capture unrelated state', live === 1);
  flushSync(() => { stream.connection = 'retrying'; });
  flushSync(() => {
    stream.connection = 'live';
    stream.incidentRevision += 1;
  });
  check('reconnect and new incident revision notify their watchers', live === 3 && incidents === 1);
  dispose();
  flushSync(() => { stream.incidentRevision += 1; });
  check('disposed stream watchers stay quiet', incidents === 1);
  check('standalone screens receive no stream events', unprovided === 0);
}

export async function checkWindowPager(check) {
  const row = (cursor) => ({ cursor, decision_id: String(cursor) });
  let head = $state.raw([row(1000)]);
  let complete;
  let defer = false;
  let empty = false;
  let failed = false;
  const audit = createWindowPager({
    getHead: () => head,
    getSubject: () => ['briefing', 'example'],
    fetchPage: async ({ before, limit }) => {
      if (defer) {
        return new Promise((resolve) => { complete = resolve; });
      }
      if (failed) {
        throw new Error('offline');
      }
      return empty ? [] : Array.from({ length: limit }, (_, i) => row(before - i - 1));
    }
  });
  for (let page = 0; page < 6; page += 1) {
    await audit.older();
  }
  check('decision window keeps one 50-row page and reports dropped back pages',
    audit.trail.length === 51 && audit.trail[1].cursor === 749 && audit.note !== null);
  const last = audit.trail.at(-1);
  failed = true;
  await audit.older();
  check('failed decision page retains the window and allows retry',
    audit.error === 'offline' && audit.trail.at(-1) === last && !audit.paging && !audit.exhausted);
  failed = false;
  empty = true;
  await audit.older();
  check('empty continuation exhausts the window and keeps its last page',
    audit.exhausted && audit.trail.at(-1) === last);
  for (let page = 0; page < 4; page += 1) {
    audit.newer();
  }
  check('only four decision back pages are reachable', audit.trail[1].cursor === 949 && !audit.exhausted);
  audit.newer();
  check('newer can return to the head', audit.trail.length === 1 && !audit.showNewer);

  defer = true;
  const pending = audit.older();
  head = [{ ...head[0], cursor: 2000 }];
  complete([row(999)]);
  await pending;
  check('new continuation boundary rejects an old window response',
    audit.trail.length === 1 && audit.trail[0].cursor === 2000 && !audit.paging);
  const superseded = audit.older();
  audit.reset();
  complete([row(1999)]);
  await superseded;
  check('decision mutation invalidates an in-flight audit page',
    audit.trail.length === 1 && !audit.paging);
}
