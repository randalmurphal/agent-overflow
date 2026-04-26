<script lang="ts">
  // Send/interrupt button extracted from the old Composer. Owns nothing
  // except the trigger chrome — the parent Composer still owns the send
  // flow, the draft store, and the turn-active signal.
  //
  // The normal send/interrupt path stays on one `<button>` so focus
  // remains stable when a turn starts mid-click. Plan follow-up actions
  // can opt into a split menu for secondary actions like "new thread" or
  // "send without comments".

  import ArrowUp from 'lucide-svelte/icons/arrow-up';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Play from 'lucide-svelte/icons/play';
  import Square from 'lucide-svelte/icons/square';
  import Icon from '../../primitives/Icon.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import type { SendButtonAction } from './sendButtonTypes';

  interface Props {
    canSend: boolean;
    isTurnActive: boolean;
    action?: SendButtonAction;
    label?: string;
    planCommentCount?: number;
    onSend: () => void;
    onSendWithoutPlanComments?: () => void;
    onSendInNewThread?: () => void;
    onInterrupt: () => void;
  }

  let {
    canSend,
    isTurnActive,
    action = 'send',
    label,
    planCommentCount = 0,
    onSend,
    onSendWithoutPlanComments,
    onSendInNewThread,
    onInterrupt,
  }: Props = $props();
  let menuTriggerEl: HTMLButtonElement | undefined = $state(undefined);
  let menuOpen = $state(false);

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
  let idleLabel = $derived(action === 'implement'
    ? 'Implement proposed plan'
    : action === 'refine'
      ? 'Refine proposed plan'
      : action === 'send-comments'
        ? 'Send plan comments'
        : 'Send message');
  let showImplementMenu = $derived(action === 'implement' && !isTurnActive && Boolean(onSendInNewThread));
  let showCommentsMenu = $derived(action === 'send-comments' && !isTurnActive && Boolean(onSendWithoutPlanComments));
  let showCommentCount = $derived(action === 'send-comments' && planCommentCount > 0);

  function closeMenu(): void {
    menuOpen = false;
  }

  function handleNewThread(): void {
    closeMenu();
    onSendInNewThread?.();
  }

  function handleWithoutComments(): void {
    closeMenu();
    onSendWithoutPlanComments?.();
  }
</script>

{#if showImplementMenu || showCommentsMenu}
  <div class="flex">
    <button
      type="button"
      onclick={handleClick}
      {disabled}
      data-testid="composer-send"
      aria-label={idleLabel}
      title={idleLabel}
      class={[
        'inline-flex h-8 items-center justify-center gap-1.5 rounded-l-full shrink-0 px-3 text-xs font-medium',
        'transition-[background-color,transform,opacity] duration-150 cursor-pointer',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-0 focus-visible:ring-accent/50',
        'hover:scale-105 active:scale-95',
        'bg-accent text-surface-0 hover:bg-accent/85',
        'disabled:opacity-30 disabled:cursor-not-allowed disabled:hover:scale-100',
      ].join(' ')}
    >
      {#if showImplementMenu}
        <Icon icon={Play} size={13} strokeWidth={2.5} class="opacity-100" />
      {:else}
        <Icon icon={ArrowUp} size={13} strokeWidth={2.5} class="opacity-100" />
      {/if}
      <span>{label}</span>
      {#if showCommentCount}
        <span class="inline-flex min-w-4 items-center justify-center rounded-full bg-surface-0/20 px-1 text-[10px] font-semibold leading-4 text-surface-0">
          {planCommentCount}
        </span>
      {/if}
    </button>
    <button
      bind:this={menuTriggerEl}
      type="button"
      onclick={() => (menuOpen = !menuOpen)}
      disabled={disabled}
      aria-label={showImplementMenu ? 'More implementation options' : 'More send options'}
      aria-haspopup="menu"
      aria-expanded={menuOpen}
      class={[
        'inline-flex h-8 w-7 items-center justify-center rounded-r-full border-l border-surface-0/20',
        'bg-accent text-surface-0 hover:bg-accent/85 transition-colors cursor-pointer',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-0 focus-visible:ring-accent/50',
        'disabled:opacity-30 disabled:cursor-not-allowed',
      ].join(' ')}
      data-testid="composer-send-menu"
    >
      <Icon icon={ChevronDown} size={13} strokeWidth={2.5} />
    </button>
  </div>

  <Popover
    anchor={menuTriggerEl}
    open={menuOpen}
    onClose={closeMenu}
    placement="top-end"
    role="none"
  >
    {#snippet children()}
      <Menu ariaLabel={showImplementMenu ? 'Implementation options' : 'Send options'} onClose={closeMenu}>
        {#if showImplementMenu}
          <MenuItem label="Implement in new thread" onSelect={handleNewThread} />
        {:else}
          <MenuItem label="Send without comments" onSelect={handleWithoutComments} />
        {/if}
      </Menu>
    {/snippet}
  </Popover>
{:else}
  <button
    type="button"
    onclick={handleClick}
    {disabled}
    data-testid={isTurnActive ? 'composer-interrupt' : 'composer-send'}
    aria-label={isTurnActive ? 'Interrupt current turn' : idleLabel}
    title={isTurnActive ? 'Interrupt current turn' : idleLabel}
    class={[
      'inline-flex h-8 items-center justify-center rounded-full shrink-0',
      label && !isTurnActive ? 'gap-1.5 px-3 text-xs font-medium' : 'w-8',
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
    {:else if action === 'implement'}
      <Icon icon={Play} size={13} strokeWidth={2.5} class="opacity-100" />
      <span>{label}</span>
    {:else if label}
      <span>{label}</span>
      {#if showCommentCount}
        <span class="inline-flex min-w-4 items-center justify-center rounded-full bg-surface-0/20 px-1 text-[10px] font-semibold leading-4 text-surface-0">
          {planCommentCount}
        </span>
      {/if}
      <Icon icon={ArrowUp} size={13} strokeWidth={2.5} class="opacity-100" />
    {:else}
      <Icon icon={ArrowUp} size={16} strokeWidth={2.5} class="opacity-100" />
    {/if}
  </button>
{/if}
