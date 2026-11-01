<script>
  // Attention list: server-ordered (flagged, priority, incident, newest,
  // stable ID). Briefing rows link to their detail screen; other kinds
  // stay text until their screens exist.
  let { items = [] } = $props();
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
