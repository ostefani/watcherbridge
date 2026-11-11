<script>
  // Optional annotation-publisher counters. Absent publisher means
  // unpublished — never a gap in the retained evidence.
  let { publisher } = $props();
</script>

<details>
  <summary>Publisher</summary>
  <dl>
    <dt>Posted</dt>
    <dd>{publisher.posted}</dd>
    <dt>Dropped</dt>
    <dd>{publisher.dropped}</dd>
    <dt>Failed</dt>
    <dd>{publisher.failed}</dd>
  </dl>
</details>
