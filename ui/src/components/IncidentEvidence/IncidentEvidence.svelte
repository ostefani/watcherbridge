<script>
  import TimeStamp from '@/components/TimeStamp/TimeStamp.svelte';
  import Pill from '@/components/Pill/Pill.svelte';

  // Member evidence: accept-time snapshots of member priorities and
  // qualified references. The incident never rewrites member briefings;
  // text renders as text, so hostile scanner strings stay inert.
  let { incident, members = [], truncated = false } = $props();

  function countLine(/** @type {{ member_count: number, qualified_refs: number }} */ inc) {
    const reports = `${inc.member_count} member report${inc.member_count === 1 ? '' : 's'}`;
    const refs = `${inc.qualified_refs} qualified upstream reference${inc.qualified_refs === 1 ? '' : 's'}`;
    return `${reports} · ${refs}`;
  }
</script>

<section aria-label="Evidence">
  <h3>Evidence</h3>
  <p>{countLine(incident)}</p>
  {#if incident.qualified_refs < incident.member_count}
    <p>Some members share identity evidence; only distinct qualified references count toward the burst threshold.</p>
  {/if}
  {#if truncated}
    <p>More members exist than shown.</p>
  {/if}
  {#if members.length === 0}
    <p>No member evidence retained.</p>
  {:else}
    <ol>
      {#each members as m (m.briefing_id)}
        <li>
          <a href={`#/briefing/${m.briefing_id}`}>{m.briefing_id}</a>
          <Pill>{m.priority}</Pill>
          <TimeStamp iso={m.created_at} />
          <span>{m.refs.length} qualified reference{m.refs.length === 1 ? '' : 's'}</span>
        </li>
      {/each}
    </ol>
  {/if}
</section>
