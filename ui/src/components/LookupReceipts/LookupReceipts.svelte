<script>
  import { stepKey } from '@/lib/evidence.js';

  // Lookup receipts: what the investigator could establish, grouped by
  // interpreted result with links back to the executed tool steps.
  let { receipts = [], onJump = () => {} } = $props();
</script>

{#if receipts?.length}
  <h4>What the investigator could establish</h4>
  <ul class="lookup-receipts">
    {#each receipts as receipt, i (i)}
      <li><span class="receipt-mark" aria-hidden="true">•</span><div><p>{receipt.summary || 'No interpreted result is available. Open the tool step to inspect the saved response.'}</p><div class="receipt-steps">{#each receipt.steps as step (step)}<button type="button" class="text-link" onclick={() => onJump(stepKey(step))}>View step {step}</button>{/each}</div></div></li>
    {/each}
  </ul>
{/if}
