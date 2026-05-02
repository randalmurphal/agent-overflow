<script lang="ts">
  // "Send Now" — peer affordance to Stop, surfaced only when a turn is
  // active AND the per-thread queue (Zone 1) has items. Clicking
  // interrupts the running turn; the backend's MarkUserInterrupt flow
  // then re-arms on the next round and the queued items dispatch
  // through the standard flush path. Same wire result as clicking
  // Stop, but labeled for the intent: "ship what's queued without
  // waiting for the round to finish."
  //
  // Visibility gates here (rather than the parent) so a future
  // composer surface can drop it in without re-deriving the rule, and
  // so an accidental render of a disabled button never appears — the
  // affordance only exists when it makes sense.

  import FastForward from 'lucide-svelte/icons/fast-forward';
  import Icon from '../../primitives/Icon.svelte';

  interface Props {
    /** True while a wire round is in flight. */
    isTurnActive: boolean;
    /** True when Zone 1 has at least one queued item. */
    hasQueuedItems: boolean;
    /** Triggers InterruptTurn on the active thread. */
    onInterrupt: () => void;
  }

  let { isTurnActive, hasQueuedItems, onInterrupt }: Props = $props();

  let visible = $derived(isTurnActive && hasQueuedItems);
</script>

{#if visible}
  <button
    type="button"
    onclick={onInterrupt}
    data-testid="composer-send-now"
    aria-label="Send queued messages now"
    title="Send queued messages now"
    class={[
      'inline-flex h-8 items-center justify-center gap-1.5 rounded-full shrink-0 px-3 text-xs font-medium',
      'transition-[background-color,transform,opacity] duration-150 cursor-pointer',
      'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-0 focus-visible:ring-accent/50',
      'hover:scale-105 active:scale-95',
      'bg-accent text-surface-0 hover:bg-accent/85',
    ].join(' ')}
  >
    <Icon icon={FastForward} size={13} strokeWidth={2.5} class="opacity-100" />
    <span>Send Now</span>
  </button>
{/if}
