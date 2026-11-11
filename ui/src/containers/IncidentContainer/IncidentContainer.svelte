<script>
  import Notice from '@/components/Notice/Notice.svelte';
  import { onDestroy, onMount } from 'svelte';
  import { fetchIncident } from '@/api.js';
  import { createResource } from '@/lib/resource.js';
  import { getConnectionOrNull } from '@/lib/context.js';
  import IncidentHeader from '@/components/IncidentHeader/IncidentHeader.svelte';
  import IncidentScope from '@/components/IncidentScope/IncidentScope.svelte';
  import IncidentEvidence from '@/components/IncidentEvidence/IncidentEvidence.svelte';
  import IncidentSummary from '@/components/IncidentSummary/IncidentSummary.svelte';
  // Shared review widget (plan step 33): screens embed DecisionsContainer
  // with subject props; page containers stay App-composed (see CONVENTIONS).
  // eslint-disable-next-line no-restricted-imports -- shared-widget exception above
  import DecisionsContainer from '@/containers/DecisionsContainer/DecisionsContainer.svelte';

  // Incident container: owns the incident fetch and the live-update
  // subscription; rendering belongs to the components below.
  // Operator review lives in DecisionsContainer, which notifies this
  // container through ondecided so it owns its own refetching.
  // `initial` carries a server snapshot: when set the screen renders it
  // without fetching, exactly as a completion would.
  // Live updates arrive through the shared connection's incident
  // revision; standalone renders without a provider keep the legacy
  // window subscription as a fallback.
  let { id, initial = null } = $props();

  const connection = getConnectionOrNull();
  const incidentRevision = connection?.incidentRevision ?? null;
  let seenRevision = -1;
  $effect(() => {
    if (!incidentRevision) {
      return;
    }
    const rev = $incidentRevision;
    if (seenRevision === -1) {
      seenRevision = rev;
      return;
    }
    if (rev !== seenRevision) {
      seenRevision = rev;
      reload();
    }
  });

  const resource = createResource(() => fetchIncident(id));
  const { data, error, refreshError } = resource;
  const shown = $derived($data ?? initial);
  // A single notice slot: the only failure mode the old screen had.
  // With no snapshot the loading state renders beneath it.
  const notice = $derived($error ?? $refreshError);

  const INCIDENT_KIND_HELP = {
    expected: 'Mark reviewed: this incident needs no further action. Members stay untouched unless included below.',
    not_expected: 'Flag for follow-up: this incident still needs a check.',
    snooze: 'Hide until a time. Empty keeps the server default (24h).',
    note: 'Free-text note, kept with the audit (max 2000 characters).'
  };

  function reload() {
    resource.refresh();
  }

  // App bumps the incident revision on live frames, reconnect and
  // resync. These events have no sequence ID and cannot use briefing
  // replay.
  onMount(() => {
    if (initial === null) {
      reload();
    }
    if (incidentRevision) {
      return;
    }
    window.addEventListener('watcherbridge:incident', reload);
    return () => window.removeEventListener('watcherbridge:incident', reload);
  });

  onDestroy(() => {
    resource.invalidate();
  });
</script>

<p><a href="#/">Back to overview</a></p>

{#if notice !== null}
  <Notice kind="alert" actionLabel="Retry" onAction={reload}>Refresh failed: {notice}</Notice>
{/if}
{#if shown === null}
  <p>Loading incident…</p>
{:else}
  {@const inc = shown.incident}
  <IncidentHeader incident={inc} />
  <IncidentScope incident={inc} />
  <IncidentEvidence incident={inc} members={shown.members} truncated={shown.truncated} />
  {#if shown.summary}
    <IncidentSummary summary={shown.summary} />
  {/if}

  <DecisionsContainer
    subjectKind="incident"
    subjectId={inc.incident_id}
    kinds={['expected', 'not_expected', 'snooze', 'note']}
    decisions={shown.decisions ?? []}
    effective={shown.effective_review ?? null}
    memberScope
    memberCount={inc.member_count}
    kindHelp={INCIDENT_KIND_HELP}
    ondecided={reload}
  />
{/if}
