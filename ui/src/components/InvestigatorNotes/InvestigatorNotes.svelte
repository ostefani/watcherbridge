<script>
  import Pill from '@/components/Pill/Pill.svelte';
  import LookupReceipts from '@/components/LookupReceipts/LookupReceipts.svelte';
  import { eventKey, stepKey } from '@/lib/evidence.js';

  // Investigator notes: the model or fallback explanation with its
  // confidence, limits, retained context, cited evidence and linked
  // profiles. Unknown, blocked and failed results stay explicit
  // limitations — never proof of absent history.
  let {
    source,
    likelyExplanation = null,
    confidence = null,
    limited = false,
    context,
    receipts = [],
    evidenceRefs = [],
    profiles = [],
    profilesTruncated = false,
    onJump = () => {}
  } = $props();
</script>

<section class="dossier-card investigator-notes" aria-label="Investigator notes">
  <h3>{source === 'agent' ? 'Investigator notes' : 'Analysis and retained context'}</h3>
  {#if likelyExplanation}<p class="hypothesis"><strong>Possible explanation:</strong> {likelyExplanation}</p>{/if}
  {#if confidence}<p><Pill>Explanation confidence (model self-assessment): {confidence}</Pill></p>{/if}
  {#if limited}<p class="context-limit"><strong>Investigation has limits.</strong> Some lookups could not establish retained context. Missing lookup results do not prove this host has no history.</p>{/if}
  <p class="context-text">{context}</p>
  <LookupReceipts {receipts} {onJump} />
  {#if evidenceRefs?.length}
    <div class="citation-row"><span class="section-note">Cited evidence</span>
      {#each evidenceRefs as ref, i (`${ref.event_id ?? ref.tool_step}:${i}`)}
        <button type="button" class="citation" onclick={() => onJump(ref.event_id ? eventKey(ref.event_id) : stepKey(ref.tool_step))}>{ref.event_id ? `Event ${ref.event_id}` : `Tool step ${ref.tool_step}`}</button>
      {/each}
    </div>
  {/if}
  {#if profiles.length > 0}
    <details class="context-links"><summary>Linked reported context</summary><ul>{#each profiles as link (`${link.profile_id}:${link.role}`)}<li><a href={`#/devices/${encodeURIComponent(link.profile_id)}`}>Open reported context</a> <span class="section-note">{link.profile_id} ({link.role})</span></li>{/each}</ul></details>
  {:else}<p class="section-note">No profile links persisted for this briefing.</p>{/if}
  {#if profilesTruncated}<p class="section-note">More profile links exist than shown.</p>{/if}
</section>
