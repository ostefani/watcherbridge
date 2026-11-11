<script>
  import CopyValue from '@/components/CopyValue/CopyValue.svelte';

  // Recommended check: the stored next step. Copy chips carry
  // addresses for the operator's own investigation — never action
  // buttons that scan or enforce.
  let { check, identity = null, identityComplete = false } = $props();
</script>

<section class="dossier-card recommended-card" aria-label="Recommended check">
  <div class="section-heading"><span class="section-icon" aria-hidden="true">↗</span><div><h3>Recommended check</h3><p class="section-note">Use this check to decide whether further investigation is needed.</p></div></div>
  <p class="recommended-action">{check}</p>
  {#if identity !== null && identityComplete && (identity.ip !== null || identity.mac !== null)}
    <div class="copy-row"><span class="section-note">Copy for your investigation</span>
      {#if identity.ip !== null}<span class="copy-chip">{identity.ip} <CopyValue value={identity.ip} name="IP address" /></span>{/if}
      {#if identity.mac !== null}<span class="copy-chip">{identity.mac} <CopyValue value={identity.mac} name="MAC address" /></span>{/if}
    </div>
  {/if}
</section>
