<script>
  import Notice from '@/components/Notice/Notice.svelte';
  import TimeStamp from '@/components/TimeStamp/TimeStamp.svelte';
  import { classLabel } from '@/lib/format.js';

  // Reported profile identity and provenance: upstream facts, last
  // report and retention-adjacent wording. Purge claims live on the
  // Health screen, never here.
  let { profile } = $props();
</script>

<h2>{profile.profile_id} — {classLabel(profile.class)}</h2>
{#if profile.class === 'legacy'}
  <Notice kind="note">Earlier bridge-inferred association; not used for current identity.</Notice>
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
  <dd><TimeStamp iso={profile.first_report_at} /></dd>
  <dt>Last report</dt>
  <dd><TimeStamp iso={profile.last_report_at} /></dd>
</dl>
