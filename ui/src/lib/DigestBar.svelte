<script>
  // Kind digest: reported-change counts for the window. All six kinds
  // always render (zero included) so the shape stays stable; counts come
  // from the server, never derived client-side.
  const KINDS = [
    'weak_appearance',
    'weak_disappearance',
    'port_closed',
    'hostname_change',
    'os_refinement',
    'other'
  ];

  // explained counts briefings the operator explicitly marked
  // expected, per kind; the rest still need review. Both maps are
  // server-computed, zero-filled across the same six kinds.
  let { digest = {}, explained = {} } = $props();
</script>

<section aria-label="Change digest">
  <h3>Reported changes</h3>
  <dl class="digest">
    {#each KINDS as kind (kind)}
      <div>
        <dt>{kind}</dt>
        <dd>{digest[kind] ?? 0} <span class="expected">expected {explained[kind] ?? 0}</span></dd>
      </div>
    {/each}
  </dl>
</section>

<style>
  .digest {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(10rem, 1fr));
    gap: 0.5rem;
    margin: 0;
  }
  .digest div {
    border: 1px solid #ccc;
    padding: 0.4rem 0.6rem;
  }
  dt {
    font-size: 0.8rem;
  }
  dd {
    margin: 0;
    font-size: 1.1rem;
  }
  .expected {
    display: block;
    font-size: 0.8rem;
  }
</style>
