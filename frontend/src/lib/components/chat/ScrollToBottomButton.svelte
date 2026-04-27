<script lang="ts">
  // Floating "scroll to bottom" chip. Hand-rolled <button> rather than the
  // IconButton primitive — IconButton's size/variant matrix is built for
  // the toolbar (rounded-md, fixed h-7/h-8), not the rounded-full
  // shadow-sheet floating chip we want here. SendButton.svelte is the
  // existing precedent for "custom button when chrome diverges from the
  // primitive."
  //
  // `data-scroll-anchor-ignore` opts the chip out of the controller's
  // click-anchor handler so clicking it re-sticks rather than unsticks.

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
    data-scroll-anchor-ignore
    data-testid="scroll-to-bottom"
    transition:fade={{ duration: 120 }}
    class={[
      'absolute bottom-4 right-4 z-10',
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
