<script lang="ts">
  // Send/interrupt button extracted from the old Composer. Owns nothing
  // except the trigger chrome — the parent Composer still owns the send
  // flow, the draft store, and the turn-active signal.
  //
  // The two modes are rendered as the same `<button>` element so focus
  // stays on the control when a turn starts mid-click and so screen
  // readers hear "Interrupt turn" immediately rather than an implicit
  // "the send button just vanished and a stop button appeared".

  import ArrowUp from 'lucide-svelte/icons/arrow-up';
  import Square from 'lucide-svelte/icons/square';
  import Icon from '../../primitives/Icon.svelte';

  interface Props {
    canSend: boolean;
    isTurnActive: boolean;
    onSend: () => void;
    onInterrupt: () => void;
  }

  let { canSend, isTurnActive, onSend, onInterrupt }: Props = $props();

  function handleClick(): void {
    if (isTurnActive) {
      onInterrupt();
      return;
    }
    if (canSend) onSend();
  }

  // The button is only disabled when we genuinely can't do anything:
  // the textarea is empty AND no turn is running. When a turn is active
  // the button becomes an interrupt trigger and stays enabled.
  let disabled = $derived(!isTurnActive && !canSend);
</script>

<button
  type="button"
  onclick={handleClick}
  {disabled}
  data-testid={isTurnActive ? 'composer-interrupt' : 'composer-send'}
  aria-label={isTurnActive ? 'Interrupt current turn' : 'Send message'}
  title={isTurnActive ? 'Interrupt current turn' : 'Send message'}
  class={[
    'inline-flex h-8 w-8 items-center justify-center rounded-full shrink-0',
    'transition-[background-color,transform,opacity] duration-150 cursor-pointer',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-0 focus-visible:ring-accent/50',
    'hover:scale-105 active:scale-95',
    isTurnActive
      ? 'bg-error text-surface-0 hover:bg-error/85'
      : 'bg-accent text-surface-0 hover:bg-accent/85',
    'disabled:opacity-30 disabled:cursor-not-allowed disabled:hover:scale-100',
  ].join(' ')}
>
  {#if isTurnActive}
    <Icon icon={Square} size={12} strokeWidth={2.5} class="opacity-100" />
  {:else}
    <Icon icon={ArrowUp} size={16} strokeWidth={2.5} class="opacity-100" />
  {/if}
</button>
