<script>
  import TimeStamp from '@/components/TimeStamp/TimeStamp.svelte';

  // Pipeline internals over process lifetime: ingest, provider,
  // redaction and writer saturation counters plus queue depth.
  let { process } = $props();
</script>

<details>
  <summary>Pipeline (process lifetime)</summary>
  <dl>
    <dt>Started</dt>
    <dd><TimeStamp iso={process.started_at} /></dd>
    <dt>Ingest accepted</dt>
    <dd>{process.ingest_accepted}</dd>
    <dt>Ingest duplicates</dt>
    <dd>{process.ingest_duplicates}</dd>
    <dt>Ingest rejected</dt>
    <dd>{process.ingest_rejected}</dd>
    <dt>Ingest saturated</dt>
    <dd>{process.ingest_saturated}</dd>
    <dt>Provider calls</dt>
    <dd>{process.provider_calls}</dd>
    <dt>Provider fallbacks</dt>
    <dd>{process.provider_fallbacks}</dd>
    <dt>Redaction blocked</dt>
    <dd>{process.redaction_blocked}</dd>
    <dt>Writer saturated</dt>
    <dd>{process.writer_saturated}</dd>
  </dl>
</details>
