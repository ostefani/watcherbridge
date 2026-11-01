<script>
  import VerdictHeader from './VerdictHeader.svelte';
  import AttentionList from './AttentionList.svelte';
  import DigestBar from './DigestBar.svelte';
  import SourceCoverage from './SourceCoverage.svelte';

  // Home (Designv9 §8.2): connection/source health -> review verdict with
  // receipts -> attention -> digest. Explicit states for loading, fetch
  // errors, no reports, quiet, degraded, and blind — never a green badge
  // that hides coverage.
  let { overview = null, loadError = null } = $props();

  const degraded = $derived(
    overview !== null && (overview.coverage === 'degraded' || overview.coverage === 'blind')
  );
</script>

{#if loadError !== null && overview === null}
  <section aria-label="Overview unavailable">
    <h2>Overview unavailable</h2>
    <p>The bridge did not return an overview ({loadError}). The stream below keeps retrying.</p>
  </section>
{:else if overview === null}
  <p role="status">Loading overview…</p>
{:else}
  {#if overview.receipts === 0}
    <p class="coverage-note">
      No receipts in the last {overview.window} — quiet here means no data, not a secure network.
    </p>
  {/if}
  {#if degraded}
    <p role="alert" class="coverage-note">
      Coverage is {overview.coverage}. Treat quiet results with caution until the reasons below clear.
    </p>
  {/if}
  <VerdictHeader
    verdict={overview.verdict}
    headline={overview.headline}
    coverage={overview.coverage}
    liveness={overview.liveness}
    reasons={overview.reasons ?? []}
    receipts={overview.receipts}
    window={overview.window}
    lastChange={overview.last_change}
  />
  <AttentionList items={overview.attention} />
  <DigestBar digest={overview.digest} />
  <SourceCoverage sources={overview.sources} />
{/if}

<style>
  .coverage-note {
    border: 1px solid #9a6700;
    padding: 0.5rem 0.75rem;
  }
</style>
