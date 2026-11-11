<script>
  import Notice from '@/components/Notice/Notice.svelte';
  import Pill from '@/components/Pill/Pill.svelte';
  import styles from './DecisionTrail.module.css';

  // Decision trail: the bounded audit window with per-row undo, older
  // and newer navigation, and exhaustion notes. Window identity is
  // owned by the container: a parent refresh with a new head
  // automatically unhides continuation and drops stale navigation.
  let {
    trail = [],
    exhausted = false,
    showNewer = false,
    note = null,
    listError = null,
    paging = false,
    undoPending = null,
    card = false,
    onUndo = async () => {},
    onOlder = async () => {},
    onNewer = () => {}
  } = $props();
</script>

{#if card}<h4 class={styles.historyHeading}>Recorded decisions</h4>{/if}
{#if trail.length === 0}
  <p class={styles.emptyHistory}>No decisions recorded.</p>
{:else}
  <ol class={styles.trail}>
    {#each trail as d (d.decision_id)}
      <li>
        <Pill>{d.kind.replace('_', ' ')}</Pill>
        {#if d.undone}<Pill>undone</Pill>{/if}
        {#if d.text}<span class={styles.text}>{d.text}</span>{/if}
        {#if d.until}<span class={styles.until}>until {d.until}</span>{/if}
        <time datetime={d.created_at}>{new Date(d.created_at).toLocaleString()}</time>
        {#if !d.undone && d.kind !== 'undo'}
          <button
            type="button"
            onclick={() => onUndo(d.decision_id)}
            disabled={undoPending !== null}
          >
            {undoPending === d.decision_id ? 'Undoing…' : 'Undo'}
          </button>
        {/if}
      </li>
    {/each}
  </ol>
  {#if !exhausted}
    <button type="button" onclick={onOlder} disabled={paging}>
      {paging ? 'Loading…' : 'Show older decisions'}
    </button>
  {/if}
  {#if showNewer}
    <button type="button" onclick={onNewer} disabled={paging}>Show newer decisions</button>
  {/if}
  {#if note !== null}
    <Notice kind="note">{note}</Notice>
  {/if}
{/if}
{#if listError !== null}
  <Notice kind="alert">{listError}</Notice>
{/if}
