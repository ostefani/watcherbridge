<script>
  import TimeStamp from '@/components/TimeStamp/TimeStamp.svelte';
  import { classLabel } from '@/lib/format.js';

  // One reported-host row. All values bind as text, so hostile content
  // stays inert.
  let { dev } = $props();
</script>

<li>
  <a data-row href={`#/devices/${dev.profile_id}`}>
    <span class="host-icon" aria-hidden="true">▣</span>
    <span class="row-main"><strong>{dev.operator_label || dev.reported_id || 'Unlabelled subject'}</strong><code>{dev.profile_id}</code><span>{classLabel(dev.class)}</span></span>
    <span class="row-meta">Last report<TimeStamp iso={dev.last_report_at} /></span>
    <span aria-hidden="true">→</span>
  </a>
</li>
