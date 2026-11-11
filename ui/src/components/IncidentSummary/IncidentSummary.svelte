<script>
  // Optional bounded summary over the sampled member reports. Coverage
  // wording stays explicit: the sample may not represent every member.
  let { summary } = $props();
</script>

<section aria-label="Incident summary">
  <h3>Summary</h3>
  <p><span class="pill">Summary status: {summary.status}</span></p>
  {#if summary.summary}<p>{summary.summary}</p>{/if}
  {#if summary.hypothesis}<p><strong>Hypothesis:</strong> {summary.hypothesis}</p>{/if}
  {#if summary.confidence}<p>Display confidence: {summary.confidence}</p>{/if}
  <p>Priority floor: {summary.floor}. Resolved priority: {summary.final_priority}{summary.suggestion ? ` (suggested ${summary.suggestion}${summary.clamped ? ', clamped' : ''})` : ' (no model raise)'}</p>
  {#if summary.reason}<p>Priority explanation: {summary.reason}</p>{/if}
  <p>Evidence coverage: {summary.sampled} sampled member reports; {summary.omitted} omitted. The sample may not represent every member.</p>
  {#if summary.citations.length}
    <p>Cited reports:</p>
    <ul>{#each summary.citations as citation (citation.member)}<li>{citation.member}: <a href={`#/briefing/${citation.briefing_id}`}>{citation.briefing_id}</a></li>{/each}</ul>
  {/if}
  {#if summary.error}<p>{summary.error}</p>{/if}
</section>
