<script>
  import Pill from '@/components/Pill/Pill.svelte';
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
          <Pill>Status: {source.status}</Pill>
          <Pill>Liveness: {source.liveness}</Pill>
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
