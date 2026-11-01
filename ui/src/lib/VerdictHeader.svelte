<script>
  // Verdict header: the server owns the verdict; this only presents it.
  // Coverage sits beside the verdict as text, never hidden behind a
  // green badge. Quiet always carries its context (receipts, window,
  // last change) and never claims security or presence.
  let { verdict, headline, coverage, liveness, reasons = [], receipts, window, lastChange } = $props();
</script>

<section aria-label="Review verdict" class="verdict" data-verdict={verdict}>
  <h2>{headline}</h2>
  <ul class="pills">
    <li class="pill">Verdict: {verdict}</li>
    <li class="pill">Coverage: {coverage}</li>
    <li class="pill">Liveness: {liveness}</li>
  </ul>
  {#if reasons.length > 0}
    <ul class="reasons">
      {#each reasons as reason (reason)}
        <li>{reason}</li>
      {/each}
    </ul>
  {/if}
  <p class="context">
    {receipts} {receipts === 1 ? 'receipt' : 'receipts'} in the last {window}{#if lastChange}, last
      change <time datetime={lastChange} title={lastChange}
        >{new Date(lastChange).toLocaleString()}</time
      >{/if}
  </p>
</section>

<style>
  .verdict {
    border: 1px solid #ccc;
    padding: 1rem;
  }
  .verdict[data-verdict='act'] {
    border-color: #9a6700;
  }
  h2 {
    margin: 0 0 0.5rem;
    font-size: 1.25rem;
  }
  .pills {
    list-style: none;
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
    margin: 0 0 0.5rem;
    padding: 0;
  }
  .pill {
    border: 1px solid #999;
    padding: 0.15rem 0.5rem;
    font-size: 0.85rem;
  }
  .reasons {
    margin: 0 0 0.5rem;
    padding-left: 1.25rem;
    font-size: 0.9rem;
  }
  .context {
    margin: 0;
    font-size: 0.9rem;
  }
</style>
