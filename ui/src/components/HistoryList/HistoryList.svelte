<script>
  import Notice from '@/components/Notice/Notice.svelte';
  import TimeStamp from '@/components/TimeStamp/TimeStamp.svelte';

  // Independently paged observation history over retained events. The
  // history cutoff comes from retention policy on the Health screen —
  // this list never claims a purge from an empty page. Accumulated rows
  // are capped so an unbounded history cannot grow the browser heap.
  let {
    observations = [],
    historyError = null,
    exhausted = false,
    capped = false,
    cap = 200,
    onOlder = () => {},
    onRetry = () => {}
  } = $props();
</script>

<section aria-label="History">
  <h3>History</h3>
  {#if observations.length === 0}
    <p>No retained reports in this window.</p>
  {:else}
    <ol>
      {#each observations as obs (obs.observation_id)}
        <li>
          {obs.event_id} ({obs.view_role}) —
          <TimeStamp iso={obs.reported_at} />,
          {obs.ports_total} ports{obs.ports_truncated ? ' (truncated)' : ''}
        </li>
      {/each}
    </ol>
  {/if}
  {#if historyError !== null}
    <Notice kind="alert" actionLabel="Retry" onAction={onRetry}>Older page failed ({historyError}).</Notice>
  {/if}
  {#if capped}
    <p>History capped at {cap} rows; use the API for the full set.</p>
  {:else if !exhausted}
    <button type="button" onclick={onOlder}>Load more reports</button>
  {:else}
    <p>End of retained history. Retention policy lives on the <a href="#/health">Health</a> screen.</p>
  {/if}
</section>
