<script>
  import Notice from '@/components/Notice/Notice.svelte';
  import { memberUndoNote } from '@/lib/memberUndo.js';

  // Member undo review: the fan-out of an undo onto member briefings,
  // with paging while more generated decisions remain.
  let { review, paging = false, error = null, onMore = async () => {} } = $props();
</script>

<Notice>{memberUndoNote(review)}</Notice>
{#if review.members.length > 0}
  <ul>
    {#each review.members as m (m.decision_id)}
      <li><a href={`#/briefing/${encodeURIComponent(m.subject_id)}`}>{m.subject_id}</a> — {m.kind.replace('_', ' ')}</li>
    {/each}
  </ul>
{/if}
{#if review.next}
  <button type="button" onclick={onMore} disabled={paging}>
    {paging ? 'Loading…' : error ? 'Retry remaining member decisions' : 'Show remaining member decisions'}
  </button>
{/if}
{#if error !== null}<Notice kind="alert">{error}</Notice>{/if}
