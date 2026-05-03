<script lang="ts">
  // Floating "scroll to bottom" chip. Hand-rolled <button> rather than the
  // IconButton primitive — IconButton's size/variant matrix is built for
  // the toolbar (rounded-md, fixed h-7/h-8), not the rounded-full
  // shadow-sheet floating chip we want here. SendButton.svelte is the
  // existing precedent for "custom button when chrome diverges from the
  // primitive."
  //
  // Positioning: the chip floats just above the composer overlay. The
  // composer is absolutely positioned at `bottom-0` of the timeline's
  // relative parent and grows upward with content (attachment tray,
  // multi-line input, approval panel). Without lifting the chip by
  // `--composer-height`, the chip would sit *behind* the composer
  // (composer is z-20 with `pointer-events-auto` children) and clicks
  // would land on the composer instead. Putting the chip at z-30 +
  // bottom = composer-height + 1rem keeps it visible and clickable
  // regardless of composer growth.
  //
  // ChatView writes `--composer-height` from a ResizeObserver on the
  // overlay. Discussion (ChannelView) intentionally does NOT — its
  // textarea is a sibling flex section below the chip's relative
  // parent, not an overlay, so the fallback `0px + 1rem` lands the
  // chip 1rem above the scroll container's bottom edge, which is the
  // visually-correct anchor on that surface. If a future refactor adds
  // a floating element INSIDE Discussion's relative parent (between
  // the scroll container and the chip), set `--composer-height` to its
  // height the same way ChatView does.

  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import { fade } from 'svelte/transition';
  import Icon from '../primitives/Icon.svelte';

  interface Props {
    visible: boolean;
    onClick: () => void;
  }

  let { visible, onClick }: Props = $props();
</script>

{#if visible}
  <button
    type="button"
    onclick={onClick}
    aria-label="Scroll to latest"
    title="Scroll to latest"
    data-testid="scroll-to-bottom"
    transition:fade={{ duration: 120 }}
    style="bottom: calc(var(--composer-height, 0px) + 1rem);"
    class={[
      'absolute right-4 z-30',
      'inline-flex h-9 w-9 items-center justify-center',
      'rounded-full border border-border-subtle bg-card text-text-secondary',
      'shadow-sheet transition-[background-color,transform,color]',
      'hover:bg-surface-2/80 hover:text-text-primary hover:scale-105 active:scale-95',
      'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50',
      'cursor-pointer',
    ].join(' ')}
  >
    <Icon icon={ChevronDown} size={16} strokeWidth={2.5} />
  </button>
{/if}
