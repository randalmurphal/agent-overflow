<script lang="ts">
  // Send/interrupt button extracted from the old Composer. Owns nothing
  // except the trigger chrome — the parent Composer still owns the send
  // flow, the draft store, and the turn-active signal.
  //
  // The normal send/interrupt path stays on one `<button>` so focus
  // remains stable when a turn starts mid-click. Plan follow-up actions
  // can opt into a split menu for secondary actions like "new thread" or
  // "send without comments". An assistant's current thread gets a session
  // menu beside the button instead: Interrupt, which keeps the session, and
  // End Session, which ends the provider process with its background work
  // (docs/specs/standing-agents.md, "Interrupt and end session").

  import ArrowUp from '@lucide/svelte/icons/arrow-up';
  import type { ScopeGrant } from '../../../transport/scopes';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Play from '@lucide/svelte/icons/play';
  import Square from '@lucide/svelte/icons/square';
  import Icon from '../../primitives/Icon.svelte';
  import SteppedSpinner from '../../primitives/SteppedSpinner.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import type { SendButtonAction } from './sendButtonTypes';

  interface Props {
    canSend: boolean;
    isTurnActive: boolean;
    /**
     * Stop-button gate. True between the moment the user clicks Send and
     * the moment SendMessage resolves (i.e. before `provider:turn_started`
     * arrives). Treated as equivalent to `isTurnActive` for rendering the
     * stop variant.
     */
    sendInFlight?: boolean;
    /**
     * The message was dispatched and the backend has shown no evidence of
     * it yet. It is nowhere on screen, so the button says it is on its
     * way and offers no action until the row or the queue entry arrives.
     * The spinner is drawn over the button's own content, which stays in
     * flow unseen, so the button keeps its width.
     */
    sendPending?: boolean;
    action?: SendButtonAction;
    label?: string;
    /** Hover explanation when a thread operation temporarily blocks Send. */
    disabledReason?: string;
    planCommentCount?: number;
    onSend: () => void;
    onSendWithoutPlanComments?: () => void;
    onSendInNewThread?: () => void;
    onInterrupt: () => void;
    /**
     * aria-label/title for the stop variant. Hosts whose stop kills
     * something other than the turn (the agent pane's read-only shell
     * stops a background task) override it.
     */
    interruptLabel?: string;
    /** Set on an assistant's current thread: the session menu's End Session. */
    onEndSession?: () => void;
    /**
     * Whether the thread has a provider session; End Session is disabled
     * without one. Undefined when the computer does not say.
     */
    sessionLive?: boolean;
    /**
     * This device's `threads:operate` there. Both items are inert unless
     * `granted`, and say why only when the grant is known to lack it.
     */
    sessionGrant?: ScopeGrant;
  }

  let {
    canSend,
    isTurnActive,
    sendInFlight = false,
    sendPending = false,
    action = 'send',
    label,
    disabledReason,
    planCommentCount = 0,
    onSend,
    onSendWithoutPlanComments,
    onSendInNewThread,
    onInterrupt,
    interruptLabel = 'Interrupt current turn',
    onEndSession,
    sessionLive,
    sessionGrant = 'granted',
  }: Props = $props();
  let menuTriggerEl: HTMLButtonElement | undefined = $state(undefined);
  let menuOpen = $state(false);

  const PENDING_LABEL = 'Sending message';

  // Treat the dispatch window the same as a wire-active turn
  // for the Stop affordance, but only when there's nothing meaningful
  // to send. With a non-empty draft mid-round, the button stays in Send
  // mode so the user can queue the message — `Composer.send()` picks
  // the enqueue branch internally when `isTurnActive` is true.
  // Interrupt is still reachable through the working-indicator's stop
  // button, which is dedicated to interrupt regardless of draft state.
  let operationBlocked = $derived(Boolean(disabledReason));
  let showPending = $derived(sendPending && !canSend);
  let showStop = $derived(!operationBlocked && !showPending && (isTurnActive || sendInFlight) && !canSend);

  // A touch tap must not blur the textarea. The blur closes the keyboard,
  // the layout viewport grows (`interactive-widget=resizes-content`) and
  // the bottom-anchored composer drops before the synthesized click lands,
  // which reads as "the keyboard closed and nothing was sent". Keeping
  // focus also keeps the keyboard up for the next message, as phone chat
  // apps do. A mouse click still moves focus to the button, as before.
  function keepInputFocus(event: PointerEvent): void {
    if (event.pointerType !== 'mouse') event.preventDefault();
  }

  function handleClick(): void {
    if (showStop) {
      onInterrupt();
      return;
    }
    if (canSend) onSend();
  }

  // The button is disabled when there is no available action, or while a
  // thread operation must settle before another Send can reuse its turn.
  let disabled = $derived(operationBlocked || showPending || (!showStop && !canSend));
  // A pending button is disabled but not dimmed: it reports progress, not
  // an unavailable action.
  let disabledClass = $derived(showPending
    ? 'disabled:cursor-default disabled:hover:scale-100'
    : 'disabled:opacity-30 disabled:cursor-not-allowed disabled:hover:scale-100');
  let idleLabel = $derived(action === 'implement'
    ? 'Implement proposed plan'
    : action === 'refine'
      ? 'Refine proposed plan'
      : action === 'send-comments'
        ? 'Send plan comments'
        : action === 'run'
          ? 'Run command'
          : 'Send message');
  let showImplementMenu = $derived(action === 'implement' && !showStop && Boolean(onSendInNewThread));
  let showCommentsMenu = $derived(action === 'send-comments' && !showStop && Boolean(onSendWithoutPlanComments));
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

  let turnRunning = $derived(isTurnActive || sendInFlight);
  let showSessionMenu = $derived(Boolean(onEndSession) && !showImplementMenu && !showCommentsMenu);

  function handleMenuInterrupt(): void {
    closeMenu();
    onInterrupt();
  }

  function handleEndSession(): void {
    closeMenu();
    onEndSession?.();
  }
</script>

{#snippet pendingSpinner()}
  {#if showPending}
    <span class="absolute inset-0 inline-flex items-center justify-center" data-testid="composer-send-spinner">
      <SteppedSpinner size={16} class="opacity-100" />
    </span>
  {/if}
{/snippet}

{#snippet mainButton(split: boolean)}
  <button
    type="button"
    onclick={handleClick}
    onpointerdown={keepInputFocus}
    {disabled}
    data-testid={showStop ? 'composer-interrupt' : 'composer-send'}
    data-pending={showPending || null}
    aria-busy={showPending}
    aria-label={showPending ? PENDING_LABEL : showStop ? interruptLabel : idleLabel}
    title={showPending ? PENDING_LABEL : showStop ? interruptLabel : (disabledReason ?? idleLabel)}
    class={[
      'relative inline-flex h-8 compact:h-10 items-center justify-center shrink-0',
      split ? 'rounded-l-full' : 'rounded-full',
      label && !showStop ? 'px-3 text-xs font-medium' : 'w-8 compact:w-10',
      'transition-[background-color,transform,opacity] duration-150 cursor-pointer',
      'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-0 focus-visible:ring-accent/50',
      split ? '' : 'hover:scale-105 active:scale-95',
      showStop
        ? 'bg-error text-surface-0 hover:bg-error/85'
        : 'bg-accent text-surface-0 hover:bg-accent/85',
      disabledClass,
    ].join(' ')}
  >
    {#if showStop}
      <Icon icon={Square} size={12} strokeWidth={2.5} class="opacity-100" />
    {:else}
      {@render pendingSpinner()}
      <span class={['inline-flex items-center gap-1.5', showPending ? 'invisible' : ''].join(' ')} data-testid="composer-send-content">
        {#if action === 'implement'}
          <Icon icon={Play} size={13} strokeWidth={2.5} class="opacity-100" />
          <span>{label}</span>
        {:else if label}
          <span>{label}</span>
          {#if showCommentCount}
            <span class="inline-flex min-w-4 items-center justify-center rounded-full bg-surface-0/20 px-1 text-[0.625rem] font-semibold leading-4 text-surface-0">
              {planCommentCount}
            </span>
          {/if}
        {:else}
          <Icon icon={ArrowUp} size={16} strokeWidth={2.5} class="opacity-100" />
        {/if}
      </span>
    {/if}
  </button>
{/snippet}

{#if showImplementMenu || showCommentsMenu}
  <div class="flex">
    <button
      type="button"
      onclick={handleClick}
      onpointerdown={keepInputFocus}
      {disabled}
      data-testid="composer-send"
      data-pending={showPending || null}
      aria-busy={showPending}
      aria-label={showPending ? PENDING_LABEL : idleLabel}
      title={showPending ? PENDING_LABEL : (disabledReason ?? idleLabel)}
      class={[
        'relative inline-flex h-8 items-center justify-center rounded-l-full shrink-0 px-3 text-xs font-medium',
        'transition-[background-color,transform,opacity] duration-150 cursor-pointer',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-0 focus-visible:ring-accent/50',
        'hover:scale-105 active:scale-95',
        'bg-accent text-surface-0 hover:bg-accent/85',
        disabledClass,
      ].join(' ')}
    >
      {@render pendingSpinner()}
      <span class={['inline-flex items-center gap-1.5', showPending ? 'invisible' : ''].join(' ')} data-testid="composer-send-content">
        {#if showImplementMenu}
          <Icon icon={Play} size={13} strokeWidth={2.5} class="opacity-100" />
        {/if}
        <span>{label}</span>
        {#if showCommentCount}
          <span class="inline-flex min-w-4 items-center justify-center rounded-full bg-surface-0/20 px-1 text-[0.625rem] font-semibold leading-4 text-surface-0">
            {planCommentCount}
          </span>
        {/if}
      </span>
    </button>
    <button
      bind:this={menuTriggerEl}
      type="button"
      onclick={() => (menuOpen = !menuOpen)}
      disabled={disabled}
      aria-label={showImplementMenu ? 'More implementation options' : 'More send options'}
      title={disabledReason}
      aria-haspopup="menu"
      aria-expanded={menuOpen}
      class={[
        'inline-flex h-8 w-7 compact:h-10 compact:w-9 items-center justify-center rounded-r-full border-l border-surface-0/20',
        'bg-accent text-surface-0 hover:bg-accent/85 transition-colors cursor-pointer',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-0 focus-visible:ring-accent/50',
        showPending ? 'disabled:cursor-default' : 'disabled:opacity-30 disabled:cursor-not-allowed',
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
{:else if showSessionMenu}
  <div class="flex">
    {@render mainButton(true)}
    <button
      bind:this={menuTriggerEl}
      type="button"
      onclick={() => (menuOpen = !menuOpen)}
      aria-label="Interrupt or end the session"
      title="Interrupt or end the session"
      aria-haspopup="menu"
      aria-expanded={menuOpen}
      class={[
        'inline-flex h-8 w-7 compact:h-10 compact:w-9 items-center justify-center rounded-r-full border-l border-surface-0/20',
        'transition-colors cursor-pointer text-surface-0',
        showStop ? 'bg-error hover:bg-error/85' : 'bg-accent hover:bg-accent/85',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-0 focus-visible:ring-accent/50',
      ].join(' ')}
      data-testid="composer-session-menu"
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
      <Menu ariaLabel="Session" onClose={closeMenu}>
        <MenuItem
          label="Interrupt"
          disabled={!turnRunning || sessionGrant !== 'granted'}
          title={sessionGrant === 'refused' ? 'Not granted to this device' : undefined}
          onSelect={handleMenuInterrupt}
        />
        <MenuItem
          label="End Session"
          disabled={sessionLive === false || sessionGrant !== 'granted'}
          title={sessionGrant === 'refused' ? 'Not granted to this device' : sessionLive === false ? 'No session is running' : undefined}
          onSelect={handleEndSession}
        />
      </Menu>
    {/snippet}
  </Popover>
{:else}
  {@render mainButton(false)}
{/if}
