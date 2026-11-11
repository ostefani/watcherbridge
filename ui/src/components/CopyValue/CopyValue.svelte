<script>
  import { onDestroy } from 'svelte';
  import styles from './CopyValue.module.css';
  // Copy button with honest outcome reporting: the label only reads
  // "Copied" after a write actually succeeded, and a rejection
  // announces the failure instead of masquerading as success.
  let { value, name = 'value' } = $props();

  let state = $state('idle');
  let timer = $state(null);
  let alive = true;

  onDestroy(() => { alive = false; if (timer !== null) clearTimeout(timer); });

  async function copy() {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
    state = 'idle';
    try {
      if (navigator.clipboard?.writeText !== undefined) {
        await navigator.clipboard.writeText(value);
      } else {
        const area = document.createElement('textarea');
        area.value = value;
        area.className = 'wb-clipboard-buffer';
        document.body.appendChild(area);
        const previous = document.activeElement;
        let ok = false;
        try {
          area.select();
          ok = document.execCommand('copy');
        } finally {
          area.remove();
          if (previous instanceof HTMLElement) previous.focus({ preventScroll: true });
        }
        if (!ok) {
          throw new Error('copy rejected');
        }
      }
      if (!alive) return;
      state = 'copied';
      timer = setTimeout(() => {
        state = 'idle';
        timer = null;
      }, 1500);
    } catch {
      if (alive) state = 'failed';
    }
  }
</script>

<button type="button" class={styles.copyBtn} onclick={copy} aria-label={`Copy ${name}`} aria-live="polite">
  {state === 'copied' ? 'Copied' : state === 'failed' ? 'Copy failed' : 'Copy'}
</button>
{#if state === 'failed'}
  <span role="status" class="section-note">Could not copy the {name}; select and copy it manually.</span>
{/if}

