<script>
  import { displayTime } from '@/lib/format.js';

  // Incident scope: the upstream source, target and member window.
  // Scope describes what the bundle covers, never device identity.
  let { incident } = $props();
  const start = $derived(displayTime(incident.window_start));
  const last = $derived(displayTime(incident.last_member_at));
</script>

<section aria-label="Scope">
  <h3>Scope</h3>
  <p>Source: {incident.source_scope}</p>
  <p>Target: {incident.target}</p>
  <p>Window start: <time datetime={incident.window_start} title={start.utc}>{start.local}</time> · Last member: <time datetime={incident.last_member_at} title={last.utc}>{last.local}</time></p>
</section>
