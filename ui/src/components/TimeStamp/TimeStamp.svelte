<script>
  import { displayTime } from '@/lib/format.js';

  // TimeStamp: the single local-time renderer. Null-safe like
  // displayTime: unparseable instants read "unknown", never throw.
  let { iso } = $props();
  const at = $derived(displayTime(iso));
</script>

<time datetime={iso} title={at.utc}>{at.local}</time>
