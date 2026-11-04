<script>
  // Connection state line: the stream is the shell's proof of life.
  // Text always accompanies the dot — never color alone.
  let { state } = $props();

  const LABEL = {
    connecting: 'Connecting…',
    live: 'Live briefing stream',
    retrying: 'Connection lost — retrying automatically…'
  };
</script>

<p role="status" class="conn" data-state={state}>
  <span class="dot" aria-hidden="true"></span>
  {LABEL[state] ?? state}
</p>
