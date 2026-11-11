<script>
  import TimeStamp from '@/components/TimeStamp/TimeStamp.svelte';

  // Incident scope: the upstream source, target and member window.
  // Scope describes what the bundle covers, never device identity.
  let { incident } = $props();
</script>

<section aria-label="Scope">
  <h3>Scope</h3>
  <p>Source: {incident.source_scope}</p>
  <p>Target: {incident.target}</p>
  <p>Window start: <TimeStamp iso={incident.window_start} /> · Last member: <TimeStamp iso={incident.last_member_at} /></p>
</section>
