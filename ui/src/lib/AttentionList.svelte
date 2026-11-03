<script>
  import { onDestroy, onMount } from 'svelte';
  import { fetchIncident } from '../api.js';

  // Attention list: server-ordered (flagged, priority, incident, newest,
  // stable ID). Briefing and incident rows link to their detail
  // screens; other kinds stay text until their screens exist.
  // Incident rows expand inline to their member briefings; the member
  // list is cached until an update/reconnect/resync invalidates it, and a
  // fetch failure surfaces on the row, never as navigation.
  let { items = [] } = $props();

  let expanded = $state(/** @type {Record<string, boolean>} */ ({}));
  let memberCache = $state(/** @type {Record<string, import('../wire.js').IncidentMember[]>} */ ({}));
  let expandError = $state(/** @type {Record<string, string>} */ ({}));
  let expanding = $state(/** @type {Record<string, boolean>} */ ({}));
  let alive = true;
  let generation = 0;
  onMount(() => {
    window.addEventListener('watcherbridge:incident', refreshMembers);
    return () => window.removeEventListener('watcherbridge:incident', refreshMembers);
  });

  function refreshMembers() {
    generation++;
    memberCache = {};
    expandError = {};
    expanding = {};
    expanded = Object.fromEntries(items.filter((item) => item.kind === 'incident' && expanded[item.id]).map((item) => [item.id, true]));
    for (const id of Object.keys(expanded)) {
      loadMembers(id);
    }
  }
  onDestroy(() => {
    alive = false;
  });

  async function toggleMembers(/** @type {string} */ id) {
    if (expanded[id]) {
      expanded[id] = false;
      return;
    }
    expanded[id] = true;
    if (memberCache[id] !== undefined || expanding[id]) {
      return;
    }
    await loadMembers(id);
  }

  async function loadMembers(id) {
    const token = generation;
    delete expandError[id];
    expanding[id] = true;
    try {
      const detail = await fetchIncident(id);
      if (alive && token === generation) {
        memberCache[id] = detail.members;
      }
    } catch (err) {
      if (alive && token === generation) {
        expandError[id] = err instanceof Error ? err.message : String(err);
      }
    } finally {
      if (alive && token === generation) {
        expanding[id] = false;
      }
    }
  }
</script>

<section aria-label="Attention">
  <h3>Needs attention ({items.length})</h3>
  {#if items.length === 0}
    <p>Nothing flagged. New briefings appear here when the bridge reports them.</p>
  {:else}
    <ol class="attention" data-rows>
      {#each items as item (item.id)}
        <li data-status={item.status}>
          {#if item.kind === 'briefing'}
            <a data-row href={`#/briefing/${item.id}`}>
              <span class="pill">Priority: {item.priority}</span>
              <span class="pill">Status: {item.status}</span>
              <span class="kind">{item.change_kind}</span>
              <code>{item.id}</code>
              <time datetime={item.created_at} title={item.created_at}
                >{new Date(item.created_at).toLocaleString()}</time
              >
            </a>
          {:else if item.kind === 'incident'}
            <a data-row href={`#/incidents/${item.id}`}>
              <span class="pill">Priority: {item.priority}</span>
              <span class="pill">Status: {item.status}</span>
              <span class="kind">{item.change_kind}</span>
              <code>{item.id}</code>
              <time datetime={item.created_at} title={item.created_at}
                >{new Date(item.created_at).toLocaleString()}</time
              >
            </a>
            <button
              type="button"
              aria-expanded={expanded[item.id] ?? false}
              disabled={expanding[item.id] ?? false}
              onclick={() => toggleMembers(item.id)}
            >
              {expanded[item.id] ? 'Hide members' : expanding[item.id] ? 'Loading members…' : 'Show members'}
            </button>
            {#if expandError[item.id] !== undefined}
              <p role="alert">{expandError[item.id]}</p>
            {/if}
            {#if (expanded[item.id] ?? false) && memberCache[item.id] !== undefined}
              <ol>
                {#each memberCache[item.id] as m (m.briefing_id)}
                  <li>
                    <a href={`#/briefing/${m.briefing_id}`}>{m.briefing_id}</a>
                    <span class="pill">{m.priority}</span>
                  </li>
                {/each}
              </ol>
            {/if}
          {:else}
            <span class="pill">Priority: {item.priority}</span>
            <span class="pill">Status: {item.status}</span>
            <span class="kind">{item.change_kind}</span>
            <code>{item.id}</code>
            <time datetime={item.created_at} title={item.created_at}
              >{new Date(item.created_at).toLocaleString()}</time
            >
          {/if}
        </li>
      {/each}
    </ol>
  {/if}
</section>

<style>
  .attention {
    list-style: decimal;
    margin: 0;
    padding-left: 1.5rem;
  }
  .attention li {
    margin-bottom: 0.5rem;
    font-size: 0.9rem;
  }
  .pill {
    border: 1px solid #999;
    padding: 0 0.4rem;
    font-size: 0.8rem;
    margin-right: 0.4rem;
  }
  .kind {
    margin-right: 0.4rem;
  }
  code {
    font-size: 0.8rem;
    margin-right: 0.4rem;
  }
</style>
