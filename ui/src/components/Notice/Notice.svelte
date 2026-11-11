<script>
  // Notice: status/alert/note banners with an optional action button.
  // Semantic wrapper only — banner text styling stays where it is, so
  // existing copy renders unchanged. `class` passes extra hooks (e.g.
  // the coverage note) straight to the paragraph.
  let { kind = 'status', actionLabel = null, onAction = null, class: className = '', children } = $props();
</script>

<p role={kind} class={className || undefined}>
  {@render children()}
  {#if actionLabel !== null}
    <button type="button" onclick={onAction}>{actionLabel}</button>
  {/if}
</p>
