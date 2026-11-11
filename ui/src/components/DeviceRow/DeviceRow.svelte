<script>
  import { displayTime, classLabel } from '@/lib/format.js';

  // One reported-host row. All values bind as text, so hostile content
  // stays inert.
  let { dev } = $props();
  const last = $derived(displayTime(dev.last_report_at));
</script>

<li>
  <a data-row href={`#/devices/${dev.profile_id}`}>
    <span class="host-icon" aria-hidden="true">▣</span>
    <span class="row-main"><strong>{dev.operator_label || dev.reported_id || 'Unlabelled subject'}</strong><code>{dev.profile_id}</code><span>{classLabel(dev.class)}</span></span>
    <span class="row-meta">Last report<time datetime={dev.last_report_at} title={last.utc}>{last.local}</time></span>
    <span aria-hidden="true">→</span>
  </a>
</li>
