<script>
  import { displayTime } from '@/lib/format.js';

  // Retention and purge: per-table totals, the policy line (never a
  // per-profile boundary) and the latest fully successful run on its
  // own record — never inferred from an error-bearing attempt.
  let { purge, retention = null } = $props();

  /** @param {number} seconds */
  function days(seconds) {
    return `${Math.floor(seconds / 86400)} days`;
  }
</script>

<details>
  <summary>Purge</summary>
  <dl>
    {#each Object.entries(purge.totals ?? {}) as [table, count] (table)}
      <dt>{table}</dt>
      <dd>{count}</dd>
    {/each}
  </dl>
  {#if retention}
    <p>
      Configured retention: {days(retention.reports_seconds)} for reports{#if retention.subjects_seconds !== undefined}, {days(retention.subjects_seconds)} for subjects{/if};
      policy only — it never proves a per-profile boundary. Ask the API for per-profile history.
    </p>
  {/if}
  {#if purge.last_success}
    {@const ok = displayTime(purge.last_success.finished_at ?? purge.last_success.started_at)}
    <p>Latest fully successful purge: {ok.local}.</p>
  {:else}
    <p>No fully successful purge recorded yet.</p>
  {/if}
</details>
