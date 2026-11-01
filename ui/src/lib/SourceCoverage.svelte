<script>
  // Per-source coverage and liveness: every source renders independently
  // with its own reasons, so one healthy source never masks a silent one.
  // Unknown liveness is the honest pre-H1 state, not a gap.
  let { sources = [] } = $props();
</script>

<section aria-label="Source coverage">
  <h3>Sources ({sources.length})</h3>
  {#if sources.length === 0}
    <p>No sources registered yet.</p>
  {:else}
    <ul class="sources">
      {#each sources as source (source.scope)}
        <li>
          <strong>{source.scope}</strong>
          <span class="pill">Status: {source.status}</span>
          <span class="pill">Liveness: {source.liveness}</span>
          {#if source.last_report_at}
            <span>
              Last report
              <time datetime={source.last_report_at} title={source.last_report_at}
                >{new Date(source.last_report_at).toLocaleString()}</time
              >
            </span>
          {/if}
          {#if source.reasons.length > 0}
            <ul>
              {#each source.reasons as reason (reason)}
                <li>{reason}</li>
              {/each}
            </ul>
          {/if}
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .sources {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .sources > li {
    border: 1px solid #ccc;
    padding: 0.5rem 0.75rem;
    margin-bottom: 0.5rem;
    font-size: 0.9rem;
  }
  .pill {
    border: 1px solid #999;
    padding: 0 0.4rem;
    font-size: 0.8rem;
    margin-left: 0.4rem;
  }
</style>
