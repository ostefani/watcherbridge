<script>
  import ReportView from '@/components/ReportView/ReportView.svelte';

  // Saved scanner reports: the referenced events with their linked
  // context, in report order. Truncation stays explicit.
  let {
    events = [],
    viewsFor = () => [],
    profiles = [],
    referencedTruncated = false,
    observationsTruncated = false
  } = $props();
</script>

<section class="dossier-card" aria-label="Saved scanner reports" data-evidence="reports" tabindex="-1">
  <div class="section-heading spread"><h3>Scanner snapshot{events.length === 1 ? '' : 's'}</h3><span class="pill">Saved at report time</span></div>
  <p class="section-note">Open a report to see what the scanner supplied, with its linked context.</p>
  {#if events.length === 0}<p>No referenced events retained.</p>
  {:else}{#each events as ref (ref.event_id)}<ReportView {ref} views={viewsFor(ref.event_id)} profileLinks={profiles} />{/each}{/if}
  {#if referencedTruncated}<p class="section-note">More referenced events exist than shown.</p>{/if}
  {#if observationsTruncated}<p class="section-note">More observations exist than shown; some saved views are omitted.</p>{/if}
</section>
