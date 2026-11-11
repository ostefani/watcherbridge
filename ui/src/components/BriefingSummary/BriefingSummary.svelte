<script>
  import IdentityGrid from '@/components/IdentityGrid/IdentityGrid.svelte';
  import { eventKey, REPORTS_KEY } from '@/lib/format/evidence.js';

  // Briefing summary: the reported change with its priority, the
  // single-view identity grid, and the scanner-report index. Text
  // renders as text, never HTML.
  let {
    title,
    priority,
    description,
    identity = null,
    identityComplete = false,
    primaryEvent = null,
    eventCount = 0,
    eventsTruncated = false,
    range = null,
    referencedTruncated = false,
    source,
    onJump = () => {}
  } = $props();
</script>

<section class="briefing-summary dossier-card" aria-label="Summary">
  <div class="briefing-heading">
    <h2>{title}</h2>
    <span class="pill priority" data-priority={priority} aria-label={`Priority: ${priority}`}>{priority} priority</span>
  </div>
  <p class="briefing-description">{description}</p>
  <IdentityGrid {identity} {identityComplete} {primaryEvent} {eventCount} {range} {referencedTruncated} {source} />
  <div class="evidence-index">
    <span>{eventCount}{eventsTruncated ? '+' : ''} source report{eventCount === 1 ? '' : 's'}</span>
    {#if primaryEvent}
      <button type="button" class="text-link" onclick={() => onJump(eventKey(primaryEvent.event_id))}>View scanner snapshot →</button>
    {:else if eventCount > 0}
      <button type="button" class="text-link" onclick={() => onJump(REPORTS_KEY)}>View saved reports →</button>
    {/if}
  </div>
</section>
