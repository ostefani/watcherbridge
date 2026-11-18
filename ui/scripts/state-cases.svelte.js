import { flushSync } from 'svelte';
import { createSelectedResource } from '@/lib/resource.svelte.js';

export async function checkSelectedResource(check) {
  const pending = Object.create(null);
  let id = $state('first');
  let snapshot = $state(null);
  let resource;
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
  } finally {
    dispose();
  }
}
