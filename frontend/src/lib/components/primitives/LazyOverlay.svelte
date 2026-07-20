<script lang="ts" generics="TProps extends Record<string, unknown>">
  /*
   * Lazy mount gate for overlay components (modals/drawers) whose chunk
   * should stay out of the eager startup graph. Overlays can't simply be
   * wrapped in `{#if open}` at the call site: they gate visibility
   * internally so Modal's exit transitions can play on close, and an
   * external unmount would cut those off. Instead the first `active`
   * latches the load; the component then stays mounted with the live
   * props, exactly as a static mount would — every open after the first
   * is warm and synchronous.
   */
  import type { Component } from 'svelte';

  interface Props {
    /** Module loader; called once, on the first `active`. */
    load: () => Promise<{ default: Component<TProps> }>;
    /** True whenever the overlay wants to be visible (its `open` state). */
    active: boolean;
    /** Props forwarded to the loaded component (keep `open` in here too). */
    props: TProps;
  }

  let { load, active, props }: Props = $props();

  // Latch, not a derived: once loading starts it must never restart or
  // remount the overlay, regardless of later `active`/`load` changes.
  let promise = $state<Promise<{ default: Component<TProps> }> | null>(null);
  $effect(() => {
    if (active && !promise) promise = load();
  });
</script>

{#if promise}
  {#await promise then { default: Overlay }}
    <Overlay {...props} />
  {:catch err}
    <div class="px-3 py-2 text-xs text-error" data-testid="lazy-overlay-load-error">
      Failed to load: {err instanceof Error ? err.message : String(err)}
    </div>
  {/await}
{/if}
