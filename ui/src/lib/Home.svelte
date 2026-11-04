<script>
  import Detail from './Detail.svelte';
  import Incident from './Incident.svelte';
  import VerdictHeader from './VerdictHeader.svelte';
  import AttentionList from './AttentionList.svelte';
  import DigestBar from './DigestBar.svelte';
  import SourceCoverage from './SourceCoverage.svelte';

  // Home (Designv9 §8.2): connection/source health -> review verdict with
  // receipts -> attention -> digest. Explicit states for loading, fetch
  // errors, no reports, quiet, degraded, and blind — never a green badge
  // that hides coverage.
  let { overview = null, loadError = null } = $props();

  let selectedId = $state(null);
  const selected = $derived((overview?.attention ?? []).find((item) => item.id === selectedId && (item.kind === 'briefing' || item.kind === 'incident')) ?? null);

  const degraded = $derived(
    overview !== null && (overview.coverage === 'degraded' || overview.coverage === 'blind')
  );
</script>

{#if loadError !== null && overview === null}
  <section aria-label="Overview unavailable">
    <h2>Overview unavailable</h2>
    <p>The bridge did not return an overview ({loadError}). The connection retries automatically.</p>
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
  <div class="review-workspace" data-empty={overview.attention.length === 0}>
    <div class="review-list">
      <AttentionList items={overview.attention} {selectedId} onselect={(item) => (selectedId = item.id)} />
      {#if selected}<p><a href="#selected-review" onclick={(event) => { event.preventDefault(); document.getElementById('selected-review')?.focus(); }}>Jump to selected review ↓</a></p>{/if}
      <DigestBar digest={overview.digest} explained={overview.explained ?? {}} />
      <SourceCoverage sources={overview.sources} />
      <p class="section-note"><kbd>j</kbd> <kbd>k</kbd> move between rows · <kbd>Enter</kbd> select · <kbd>?</kbd> shortcuts</p>
    </div>
    {#if overview.attention.length > 0}
    <section class="detail-pane" aria-label="Selected review" id="selected-review" tabindex="-1">
      {#if selected}
        <div class="pane-heading"><span class="eyebrow">Selected review</span><a href={selected.kind === 'briefing' ? `#/briefing/${selected.id}` : `#/incidents/${selected.id}`}>Open full page →</a></div>
        {#key selected.id}
          {#if selected.kind === 'briefing'}<Detail id={selected.id} embedded />{:else}<Incident id={selected.id} />{/if}
        {/key}
      {:else}
        <div class="empty-detail"><span class="empty-mark" aria-hidden="true">↖</span><h2>Select a review</h2><p>Choose an item in Needs attention to read its evidence and recommended check, and record your decision.</p><p>New reports update the list automatically.</p></div>
      {/if}
    </section>
    {/if}
  </div>
{/if}
