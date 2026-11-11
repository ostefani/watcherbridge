<script>
  import { displayTime } from '@/lib/format.js';
  import TraceStep from '@/components/TraceStep/TraceStep.svelte';

  // Technical details: executed tool steps, model accounting and
  // provenance. Opaque grouping anchors and IDs belong here, never in
  // the reported-host label.
  let {
    traces = [],
    inputTokens = null,
    outputTokens = null,
    costUsd = null,
    briefingId,
    groupId,
    groupingVersion,
    seq,
    createdAt,
    host
  } = $props();
  const created = $derived(displayTime(createdAt));
</script>

<details class="tech-block dossier-card" aria-label="Technical details">
  <summary>Technical details <span class="section-note">Tool calls, accounting and provenance</span></summary>
  <div class="tech-body"><h4>Executed tool steps</h4>
    {#if traces?.length}<ol class="trace-list">{#each traces as step (step.step)}<TraceStep {step} />{/each}</ol>{:else}<p>No agent trace is available for this briefing.</p>{/if}
    {#if inputTokens != null || outputTokens != null || costUsd != null}
      <h4>Model accounting</h4><p>{inputTokens ?? 'Unknown'} input tokens · {outputTokens ?? 'Unknown'} output tokens · {costUsd == null ? 'Cost unknown' : `$${costUsd}`}</p>
    {/if}
    <h4>Provenance</h4><dl>
      <dt>Briefing</dt><dd>{briefingId}</dd><dt>Group</dt><dd>{groupId}</dd>
      <dt>Grouping version</dt><dd>{groupingVersion}</dd><dt>Sequence</dt><dd>{seq}</dd>
      <dt>Created</dt><dd><time datetime={createdAt} title={created?.utc}>{created?.local}</time></dd>
      <dt>Grouping anchor</dt><dd>{host}</dd>
    </dl>
  </div>
</details>
