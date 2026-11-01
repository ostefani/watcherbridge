<script>
  // Connection state line: the stream is the shell's proof of life.
  // Text always accompanies the dot — never color alone.
  let { state } = $props();

  const LABEL = {
    connecting: 'Connecting to the bridge event stream…',
    live: 'Connected: live briefings stream.',
    retrying: 'Connection lost — retrying automatically…'
  };
</script>

<p role="status" class="conn" data-state={state}>
  <span class="dot" aria-hidden="true"></span>
  {LABEL[state] ?? state}
</p>

<style>
  .conn {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: 0.9rem;
  }
  .dot {
    width: 0.6rem;
    height: 0.6rem;
    border-radius: 50%;
    background: #888;
  }
  .conn[data-state='live'] .dot {
    background: #1a7f37;
  }
  .conn[data-state='retrying'] .dot {
    background: #9a6700;
  }
</style>
