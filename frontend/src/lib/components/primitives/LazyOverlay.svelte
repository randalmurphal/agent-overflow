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
    <!-- The overlay never loaded, so nothing renders its frame. This branch
         mounts wherever the LazyOverlay sits — beside PaneHost, as a flex
         sibling of the pane strip — so it has to place itself: a fixed,
         centred panel rather than an inline div that would squeeze the
         panes sideways. -->
    <div class="pointer-events-none fixed inset-0 z-50 flex items-center justify-center p-6">
      <div
        class="pointer-events-auto max-w-md rounded-[var(--radius-card)] border border-error/40 bg-surface-1 px-4 py-3 text-xs text-error shadow-menu"
        role="alert"
        data-testid="lazy-overlay-load-error"
      >
        Failed to load: {err instanceof Error ? err.message : String(err)}
      </div>
    </div>
  {/await}
{/if}
