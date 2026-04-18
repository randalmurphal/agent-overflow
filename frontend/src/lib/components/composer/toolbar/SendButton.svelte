<script lang="ts">
  // Send/interrupt button extracted from the old Composer. Owns nothing
  // except the trigger chrome — the parent Composer still owns the send
  // flow, the draft store, and the turn-active signal.
  //
  // The two modes are rendered as the same `<button>` element so focus
  // stays on the control when a turn starts mid-click and so screen
  // readers hear "Interrupt turn" immediately rather than an implicit
  // "the send button just vanished and a stop button appeared".

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
    'ml-auto inline-flex h-8 w-8 items-center justify-center rounded-full shrink-0',
    'transition-colors cursor-pointer',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50',
    isTurnActive
      ? 'bg-error text-surface-0 hover:bg-error/85'
      : 'bg-accent text-surface-0 hover:bg-accent/85',
    'disabled:opacity-40 disabled:cursor-not-allowed',
  ].join(' ')}
>
  {#if isTurnActive}
    <svg
      viewBox="0 0 24 24"
      class="h-3.5 w-3.5"
      fill="currentColor"
      aria-hidden="true"
    >
      <rect x="6" y="6" width="12" height="12" rx="1" />
    </svg>
  {:else}
    <svg
      viewBox="0 0 24 24"
      class="h-4 w-4"
      fill="none"
      stroke="currentColor"
      stroke-width="2.5"
      stroke-linecap="round"
      stroke-linejoin="round"
      aria-hidden="true"
    >
      <path d="M12 19V5" />
      <path d="M5 12l7-7 7 7" />
    </svg>
  {/if}
</button>
