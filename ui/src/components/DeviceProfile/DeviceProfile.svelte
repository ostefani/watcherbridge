<script>
  import { displayTime, classLabel } from '@/lib/format.js';

  // Reported profile identity and provenance: upstream facts, last
  // report and retention-adjacent wording. Purge claims live on the
  // Health screen, never here.
  let { profile } = $props();
  const first = $derived(displayTime(profile.first_report_at));
  const last = $derived(displayTime(profile.last_report_at));
</script>

<h2>{profile.profile_id} — {classLabel(profile.class)}</h2>
{#if profile.class === 'legacy'}
  <p role="note">Earlier bridge-inferred association; not used for current identity.</p>
{:else if profile.class === 'address' || profile.class === 'event'}
  <p>This page describes {profile.class === 'address' ? 'address context' : 'unresolved events'}, not a device.</p>
{/if}
<dl>
  <dt>Source scope</dt>
  <dd>{profile.source_scope}</dd>
  <dt>Upstream reference</dt>
  <dd>{profile.reported_id}</dd>
  <dt>Reported by</dt>
  <dd>{profile.reported_source} (confidence: {profile.reported_confidence || 'unknown'})</dd>
  <dt>Association</dt>
  <dd>{profile.association_status}</dd>
  {#if profile.operator_label !== null && profile.operator_label !== undefined}
    <dt>Your label</dt>
    <dd>{profile.operator_label}</dd>
  {/if}
  <dt>First report</dt>
  <dd><time datetime={profile.first_report_at} title={first.utc}>{first.local}</time></dd>
  <dt>Last report</dt>
  <dd><time datetime={profile.last_report_at} title={last.utc}>{last.local}</time></dd>
</dl>
