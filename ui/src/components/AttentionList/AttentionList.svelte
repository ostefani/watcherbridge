<script>
  import { changeLabel, reviewTitle } from '@/lib/format.js';

  // Attention list (pure): server-ordered (flagged, priority, incident,
  // newest, stable ID). Briefing and incident rows link to their detail
  // screens; other kinds stay text until their screens exist.
  // Expansion is UI-only state; member content arrives through the
  // `members` snippet (an IncidentMembersContainer in production), so
  // this component never fetches. Without the snippet, incident rows
  // render collapsed with an inert toggle.
  let {
    items = [],
    selectedId = null,
    titles = {},
    onselect = null,
    members = null
  } = $props();

  let expanded = $state(/** @type {Record<string, boolean>} */ ({}));
</script>

<section aria-label="Attention">
  <h3>Needs attention ({items.length})</h3>
  {#if items.length === 0}
    <p>Nothing flagged. New briefings appear here when the bridge reports them.</p>
  {:else}
    <ol class="attention" data-rows>
      {#each items as item (item.id)}
        <li data-status={item.status}>
          {#snippet row()}
            <span class="pill priority" data-priority={item.priority} aria-label={`Priority: ${item.priority}`}>{item.priority}</span>
            <span class="row-main"><strong>{item.kind === 'incident' ? 'Incident · ' : ''}{item.kind === 'briefing' ? (titles[item.id] ?? reviewTitle(item.change_kind)) : changeLabel(item.change_kind)}</strong><code>{item.id}</code></span>
            <span class="row-meta"><span class="pill">{item.status}</span><time datetime={item.created_at} title={item.created_at}>{new Date(item.created_at).toLocaleString()}</time></span>
          {/snippet}
          {#if item.kind === 'briefing' || item.kind === 'incident'}
            {#if onselect}
              <button class="attention-row" type="button" data-row aria-current={selectedId === item.id ? 'true' : undefined} onclick={() => onselect(item)}>{@render row()}</button>
            {:else}
              <a class="attention-row" data-row href={item.kind === 'briefing' ? `#/briefing/${item.id}` : `#/incidents/${item.id}`}>{@render row()}</a>
            {/if}
          {:else}
            <div class="attention-row">{@render row()}</div>
          {/if}
          {#if item.kind === 'incident'}
            {@const open = expanded[item.id] ?? false}
            <button
              type="button"
              aria-expanded={open}
              onclick={() => { expanded[item.id] = !open; }}
            >
              {open ? 'Hide members' : 'Show members'}
            </button>
            {@render members?.(item.id, open)}
          {/if}
        </li>
      {/each}
    </ol>
  {/if}
</section>
