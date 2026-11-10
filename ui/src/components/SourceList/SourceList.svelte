<script>
  import SourceCard from '@/components/SourceCard/SourceCard.svelte';

  // Source roster: one card per registered source, oldest API order.
  // Keeps the container to orchestration; empty state reads as an
  // explicit unknown, not a failed load.
  let { sources = [] } = $props();
</script>

<details open>
  <summary>Sources ({sources.length})</summary>
  {#if sources.length === 0}
    <p>No sources registered.</p>
  {:else}
    {#each sources as source (source.scope)}
      <SourceCard {source} />
    {/each}
  {/if}
</details>
