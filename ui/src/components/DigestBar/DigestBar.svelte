<script>
  import { changeLabel } from '@/lib/format.js';
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
  <p class="section-note">Counts in this review window. Expected means explicitly marked by you.</p>
  <dl class="digest">
    {#each KINDS as kind (kind)}
      <div>
        <dt>{changeLabel(kind)}</dt>
        <dd>{digest[kind] ?? 0} <span class="expected">expected {explained[kind] ?? 0}</span></dd>
      </div>
    {/each}
  </dl>
</section>
