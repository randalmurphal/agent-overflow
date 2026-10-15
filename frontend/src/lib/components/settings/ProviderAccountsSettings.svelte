<script lang="ts">
  import { settingsComputer } from './settingsComputer';
  const { backend, hasScope } = settingsComputer();
  // The Accounts section of a provider's settings page. The account
  // logic itself lives in stores/providerAccounts.svelte.ts — shared with the
  // account-switcher picker, so a switch made in either surface is the same
  // state change. This component owns only the settings-side chrome: the
  // "log in to another account" button, the cards, their order, and the
  // removal confirm.

  import { onDestroy, onMount, tick } from 'svelte';
  import GripVertical from '@lucide/svelte/icons/grip-vertical';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import Icon from '../primitives/Icon.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import ProviderAccountLimits from '../shared/ProviderAccountLimits.svelte';
  import ProviderLoginFlow from '../accounts/ProviderLoginFlow.svelte';
  import type { ManagedProviderAccount } from '../../stores/bindings';
  import {
    getProviderAccountActions,
    getProviderAccountsFor,
    getProviderLogin,
    isProviderAccountsLoading,
    isProviderCredentialOpInFlight,
    isProviderLoginActive,
    loadProviderAccounts,
    startProviderLogin,
    providerAccountActionLabel,
    providerAccountLoginExpiry,
    providerAccountName,
    providerAccountOrgLabel,
    providerLabel as resolveProviderLabel,
    refreshProviderAccountUsage,
    removeProviderAccount,
    reorderProviderAccounts,
    switchProviderAccount,
  } from '../../stores/providerAccounts.svelte';
  import { displacement, dropIndex, moveItem } from '../../utils/listReorder';
  import { motionReduced } from '../../utils/reducedMotion';
  import { getProviderRateLimits } from '../../stores/rateLimitsInfo.svelte';
  import { providerFieldId, type SettingsProvider } from './fields';
  import SettingsHeader from './SettingsHeader.svelte';
  import { PRIMARY_BUTTON_CLASS, GHOST_BUTTON_CLASS } from './styles';

  let { provider }: { provider: SettingsProvider } = $props();

  let providerLabel = $derived(resolveProviderLabel(provider));
  // Every credential write here (StartProviderLogin, SwitchProviderAccount,
  // RefreshProviderAccountUsage, RemoveProviderAccount) rides `access:admin`
  // on this computer. The cards still list; their controls go inert.
  let admin = $derived(hasScope('access:admin'));
  let ungrantedTitle = $derived(admin ? undefined : 'Not granted to this device');
  let accounts = $derived(getProviderAccountsFor(provider, backend));
  let loading = $derived(isProviderAccountsLoading(backend));
  let actions = $derived(getProviderAccountActions(provider, backend));
  // One answer for "is a credential op running", shared with the store's own
  // refusal, so a disabled button and a rejected call can never disagree.
  let credentialOpInFlight = $derived(isProviderCredentialOpInFlight(provider, backend));
  let pendingRemoval = $state<ManagedProviderAccount | null>(null);
  // The flow panel renders for a live sign-in AND for one that ended badly,
  // since a failure is what the user has to read before trying again.
  let login = $derived(getProviderLogin(provider, backend));
  let showLoginFlow = $derived(
    isProviderLoginActive(provider, backend) || login.phase === 'failed',
  );

  onMount(() => {
    void loadProviderAccounts(backend);
  });

  function requestRemoval(account: ManagedProviderAccount): void {
    if (credentialOpInFlight) return;
    pendingRemoval = account;
  }

  async function confirmRemoval(): Promise<void> {
    const account = pendingRemoval;
    if (!account) return;
    pendingRemoval = null;
    await removeProviderAccount(provider, account, backend);
  }

  function removalDescription(account: ManagedProviderAccount): string {
    if (!account.active) {
      return `Remove ${providerAccountName(account)} from saved accounts? You’ll need to log in again to use it.`;
    }
    if (accounts.length === 1) {
      return `Remove ${providerAccountName(account)} and sign out of ${providerLabel}? You’ll need to log in again to use it.`;
    }
    return `Remove ${providerAccountName(account)}? The next saved ${providerLabel} account will become active.`;
  }

  // A saved account whose credential is gone cannot be selected. Logging in
  // resolves back to this same account by identity (email + organization), so
  // it keeps its usage history rather than becoming a second card.
  function cardAction(account: ManagedProviderAccount): () => void {
    if (account.needsLogin) return () => void startProviderLogin(provider, backend);
    return () => void switchProviderAccount(provider, account, backend);
  }

  // ---- order ---------------------------------------------------------------
  //
  // The cards are in the order the user keeps them. A card moves by dragging
  // its grip (mouse, pen or touch) or by arrow keys on the focused grip. While
  // a card is held, the others slide aside to show where it will land; on
  // release the new order is written and every card settles from where it
  // was drawn to where it now sits.

  interface CardDrag {
    from: number;
    to: number;
    pointerId: number;
    startY: number;
    pointerY: number;
    // Middles of every card when the drag began, and the held card's height
    // plus the list gap: the distance each displaced card moves.
    middles: number[];
    span: number;
    scroller: HTMLElement | null;
    startScroll: number;
    scrollDelta: number;
    // The pointer has travelled past the threshold, so this is a drag rather
    // than a press on the grip.
    moving: boolean;
  }

  const DRAG_THRESHOLD_PX = 4;
  const EDGE_SCROLL_PX = 48;
  const MAX_EDGE_SCROLL_STEP_PX = 16;
  const SETTLE_MS = 180;

  let listEl: HTMLDivElement | undefined = $state(undefined);
  let drag = $state.raw<CardDrag | null>(null);
  let edgeScrollFrame = 0;
  let reorderNotice = $state('');
  let canReorder = $derived(admin && accounts.length > 1);

  // Settings can close mid-drag (Escape, navigation): the edge-scroll frame
  // must not outlive the cards it moves.
  onDestroy(() => {
    if (edgeScrollFrame) cancelAnimationFrame(edgeScrollFrame);
    edgeScrollFrame = 0;
    drag = null;
  });

  function cardOffset(index: number): number {
    if (!drag?.moving) return 0;
    if (index === drag.from) return drag.pointerY - drag.startY + drag.scrollDelta;
    return displacement(index, drag.from, drag.to, drag.span);
  }

  function cardElements(): HTMLElement[] {
    return listEl ? [...listEl.querySelectorAll<HTMLElement>('[data-account-card]')] : [];
  }

  function drawnTops(): Map<string, number> {
    return new Map(cardElements().map((card) => [card.dataset.accountCard ?? '', card.getBoundingClientRect().top]));
  }

  // Slides every card from where it was drawn (`before`) to where the new
  // order puts it, so neither a release nor a key press jumps.
  async function settle(before: Map<string, number>, focusId?: string): Promise<void> {
    await tick();
    if (focusId) listEl?.querySelector<HTMLElement>(`[data-reorder-handle="${focusId}"]`)?.focus();
    if (motionReduced()) return;
    for (const card of cardElements()) {
      const from = before.get(card.dataset.accountCard ?? '');
      if (from === undefined) continue;
      const delta = from - card.getBoundingClientRect().top;
      if (Math.abs(delta) < 0.5) continue;
      card.animate(
        [{ transform: `translateY(${delta}px)` }, { transform: 'none' }],
        { duration: SETTLE_MS, easing: 'cubic-bezier(0.33, 1, 0.68, 1)' },
      );
    }
  }

  function commitOrder(from: number, to: number, before: Map<string, number>, focusId?: string): void {
    const moved = accounts[from];
    if (!moved || from === to) return;
    void reorderProviderAccounts(provider, moveItem(accounts.map((account) => account.id), from, to), backend);
    reorderNotice = `Moved ${providerAccountName(moved)} to position ${to + 1} of ${accounts.length}.`;
    void settle(before, focusId);
  }

  function scrollParent(node: HTMLElement): HTMLElement | null {
    for (let el = node.parentElement; el; el = el.parentElement) {
      const overflow = getComputedStyle(el).overflowY;
      if (overflow === 'auto' || overflow === 'scroll') return el;
    }
    return null;
  }

  function startDrag(event: PointerEvent, index: number): void {
    if (!canReorder || drag || (event.pointerType === 'mouse' && event.button !== 0)) return;
    const rects = cardElements().map((card) => card.getBoundingClientRect());
    const held = rects[index];
    if (!held) return;
    const neighbour = rects[index + 1] ?? rects[index - 1];
    const gap = neighbour
      ? (neighbour.top > held.top ? neighbour.top - held.bottom : held.top - neighbour.bottom)
      : 0;
    const handle = event.currentTarget as HTMLElement;
    handle.setPointerCapture(event.pointerId);
    const scroller = scrollParent(handle);
    drag = {
      from: index,
      to: index,
      pointerId: event.pointerId,
      startY: event.clientY,
      pointerY: event.clientY,
      middles: rects.map((rect) => (rect.top + rect.bottom) / 2),
      span: held.height + gap,
      scroller,
      startScroll: scroller?.scrollTop ?? 0,
      scrollDelta: 0,
      moving: false,
    };
  }

  // Re-aims the held card at the pointer, in the coordinates the drag began
  // in: the pointer's travel plus however far the page scrolled under it.
  function aim(current: CardDrag, pointerY: number): CardDrag {
    const scrollDelta = (current.scroller?.scrollTop ?? 0) - current.startScroll;
    const middle = current.middles[current.from] + pointerY - current.startY + scrollDelta;
    return { ...current, pointerY, scrollDelta, to: dropIndex(current.middles, current.from, middle) };
  }

  function moveDrag(event: PointerEvent): void {
    if (!drag || event.pointerId !== drag.pointerId) return;
    const moving = drag.moving || Math.abs(event.clientY - drag.startY) >= DRAG_THRESHOLD_PX;
    if (!moving) return;
    drag = aim({ ...drag, moving }, event.clientY);
    if (!edgeScrollFrame) edgeScrollFrame = requestAnimationFrame(edgeScroll);
  }

  // Scrolls the page while the held card is near its top or bottom edge, so
  // a card can travel past what is on screen.
  function edgeScroll(): void {
    edgeScrollFrame = 0;
    const scroller = drag?.moving ? drag.scroller : null;
    if (!drag || !scroller) return;
    const box = scroller.getBoundingClientRect();
    const above = box.top + EDGE_SCROLL_PX - drag.pointerY;
    const below = drag.pointerY - (box.bottom - EDGE_SCROLL_PX);
    const reach = Math.max(above, below);
    if (reach <= 0) return;
    const step = Math.ceil(MAX_EDGE_SCROLL_STEP_PX * Math.min(1, reach / EDGE_SCROLL_PX));
    const previous = scroller.scrollTop;
    scroller.scrollTop += above > 0 ? -step : step;
    if (scroller.scrollTop === previous) return;
    drag = aim(drag, drag.pointerY);
    edgeScrollFrame = requestAnimationFrame(edgeScroll);
  }

  function endDrag(event: PointerEvent, commit: boolean): void {
    if (!drag || event.pointerId !== drag.pointerId) return;
    const ended = drag;
    if (edgeScrollFrame) cancelAnimationFrame(edgeScrollFrame);
    edgeScrollFrame = 0;
    const before = drawnTops();
    drag = null;
    if (!ended.moving) return;
    if (commit && ended.to !== ended.from) commitOrder(ended.from, ended.to, before);
    else void settle(before);
  }

  function cancelDragOnEscape(event: KeyboardEvent): void {
    if (event.key !== 'Escape' || !drag?.moving) return;
    event.preventDefault();
    event.stopPropagation();
    const before = drawnTops();
    if (edgeScrollFrame) cancelAnimationFrame(edgeScrollFrame);
    edgeScrollFrame = 0;
    drag = null;
    void settle(before);
  }

  function handleReorderKey(event: KeyboardEvent, index: number): void {
    if (!canReorder || drag) return;
    const last = accounts.length - 1;
    const to =
      event.key === 'ArrowUp' ? Math.max(0, index - 1)
      : event.key === 'ArrowDown' ? Math.min(last, index + 1)
      : event.key === 'Home' ? 0
      : event.key === 'End' ? last
      : null;
    if (to === null) return;
    event.preventDefault();
    commitOrder(index, to, drawnTops(), accounts[index].id);
  }
</script>

<svelte:window onkeydowncapture={cancelDragOnEscape} />

<!--
  Not a SettingsField — the block is a header, a button and a list of cards
  rather than one labelled row — so it stamps the search index's anchor and
  label itself. See fields.ts.
-->
<div
  data-testid="provider-accounts-{provider}"
  data-settings-field={providerFieldId(provider, 'accounts')}
  data-settings-label="Accounts"
>
  <SettingsHeader title="Accounts" description="Saved native logins and their last-known limits.">
    {#snippet badge()}
      <button
        type="button"
        class={PRIMARY_BUTTON_CLASS}
        disabled={credentialOpInFlight || !admin}
        title={ungrantedTitle}
        onclick={() => void startProviderLogin(provider, backend)}
      >
        {actions.loggingIn ? 'Signing in…' : 'Log in to another account'}
      </button>
    {/snippet}
  </SettingsHeader>

  {#if showLoginFlow}
    <ProviderLoginFlow {backend} {provider} {login} />
  {/if}

  {#if loading}
    <p class="text-[0.71875rem] text-fg-hint">Loading accounts…</p>
  {:else if accounts.length === 0}
    <p class="text-[0.71875rem] text-fg-hint">
      The current native login will be saved automatically when detected.
    </p>
  {:else}
    <p class="sr-only" aria-live="polite">{reorderNotice}</p>
    <div class="flex flex-col gap-2" bind:this={listEl}>
      {#each accounts as account, index (account.id)}
        {@const limits = getProviderRateLimits(provider, account.id, backend)}
        {@const orgLabel = providerAccountOrgLabel(account)}
        {@const loginExpiry = providerAccountLoginExpiry(account)}
        {@const held = drag?.moving === true && drag.from === index}
        {@const offset = cardOffset(index)}
        <div
          class="relative rounded-[var(--radius-field)] border px-3 py-2.5 {account.needsLogin
            ? 'border-warning/40 bg-surface-0'
            : account.active
              ? 'border-accent/45 bg-accent/5'
              : 'border-border-subtle bg-surface-0'} {held
            ? 'z-10 shadow-menu'
            : drag?.moving
              ? 'transition-transform duration-150 ease-out'
              : ''}"
          style:transform={offset ? `translateY(${offset}px)` : undefined}
          data-testid="provider-account-{account.id}"
          data-account-card={account.id}
        >
          <div class="flex items-start justify-between gap-3">
            {#if canReorder}
              <button
                type="button"
                class="-ml-1.5 flex h-5 w-4 shrink-0 touch-none items-center justify-center rounded-[var(--radius-field)] text-fg-hint hover:text-fg-muted focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:h-9 compact:w-7 {held
                  ? 'cursor-grabbing'
                  : 'cursor-grab'}"
                aria-label={`Reorder ${providerAccountName(account)}`}
                aria-roledescription="drag handle"
                aria-keyshortcuts="ArrowUp ArrowDown Home End"
                title="Drag to reorder, or use the arrow keys"
                data-reorder-handle={account.id}
                data-testid="provider-account-reorder-{account.id}"
                onpointerdown={(event) => startDrag(event, index)}
                onpointermove={moveDrag}
                onpointerup={(event) => endDrag(event, true)}
                onpointercancel={(event) => endDrag(event, false)}
                onkeydown={(event) => handleReorderKey(event, index)}
              >
                <Icon icon={GripVertical} size={12} strokeWidth={1.75} />
              </button>
            {/if}
            <button
              type="button"
              class="min-w-0 flex-1 cursor-pointer text-left disabled:cursor-default"
              disabled={(account.active && !account.needsLogin) || credentialOpInFlight || !admin}
              title={ungrantedTitle}
              onclick={cardAction(account)}
              aria-label={providerAccountActionLabel(account)}
            >
              <span class="flex items-center gap-2">
                <span
                  class="h-2 w-2 shrink-0 rounded-full border {account.needsLogin
                    ? 'border-warning'
                    : account.active
                      ? 'border-accent bg-accent'
                      : 'border-fg-hint'}"
                  aria-hidden="true"
                ></span>
                <span
                  class="truncate text-[0.75rem] font-medium {account.needsLogin
                    ? 'text-fg-muted'
                    : 'text-fg'}"
                >
                  {providerAccountName(account)}
                </span>
                {#if account.active}
                  <span class="rounded-full bg-accent/10 px-1.5 py-0.5 text-[0.625rem] font-medium text-accent">
                    Active
                  </span>
                {/if}
              </span>
              {#if (account.email && account.displayName) || orgLabel}
                <span class="mt-0.5 block truncate pl-4 text-[0.6875rem] text-fg-hint">
                  {[account.displayName ? account.email : '', orgLabel].filter(Boolean).join(' · ')}
                </span>
              {/if}
              <!-- Why this account needs a sign-in, and for a live one, when
                   it will. An expired login is not a missing credential: the
                   card says which it is. -->
              {#if loginExpiry}
                <span
                  class="mt-0.5 block pl-4 text-[0.6875rem] {loginExpiry.urgent
                    ? 'text-warning'
                    : 'text-fg-hint'}"
                  data-testid="provider-account-login-expiry-{account.id}"
                >
                  {loginExpiry.text}
                </span>
              {:else if account.needsLogin}
                <span class="mt-0.5 block pl-4 text-[0.6875rem] text-fg-hint">
                  Its saved {providerLabel} credentials are gone.
                </span>
              {/if}
            </button>

            <div class="flex shrink-0 items-center gap-1">
              <!-- Signing in renews whichever saved account the login belongs
                   to, and never switches; offered from the week before the
                   login ends. -->
              {#if loginExpiry || account.needsLogin}
                <button
                  type="button"
                  class="{GHOST_BUTTON_CLASS} font-medium text-warning hover:text-warning disabled:cursor-not-allowed disabled:opacity-50"
                  disabled={credentialOpInFlight || !admin}
                  title={ungrantedTitle}
                  onclick={() => void startProviderLogin(provider, backend)}
                  aria-label={`Sign in again to ${providerAccountName(account)}`}
                  data-testid="provider-account-sign-in-{account.id}"
                >
                  Sign in again
                </button>
              {/if}
              <button
                type="button"
                class={GHOST_BUTTON_CLASS}
                disabled={account.needsLogin || !!actions.refreshingID || credentialOpInFlight || !admin}
                onclick={() => void refreshProviderAccountUsage(provider, account, backend)}
                title={ungrantedTitle ?? (account.needsLogin
                  ? 'Sign in again to refresh usage limits'
                  : 'Refresh usage limits')}
                aria-label={`Refresh usage for ${providerAccountName(account)}`}
              >
                <Icon
                  icon={RefreshCw}
                  size={12}
                  strokeWidth={1.75}
                  class={actions.refreshingID === account.id ? 'animate-spin' : ''}
                />
              </button>
              <button
                type="button"
                class="{GHOST_BUTTON_CLASS} hover:text-error"
                disabled={credentialOpInFlight || !admin}
                onclick={() => requestRemoval(account)}
                title={ungrantedTitle ?? 'Remove saved account'}
                aria-label={`Remove ${providerAccountName(account)}`}
              >
                <Icon icon={Trash2} size={12} strokeWidth={1.75} />
              </button>
            </div>
          </div>

          <ProviderAccountLimits {limits} />
        </div>
      {/each}
    </div>
  {/if}
</div>

<ConfirmDialog
  open={pendingRemoval !== null}
  title={`Remove ${providerLabel} account?`}
  description={pendingRemoval ? removalDescription(pendingRemoval) : ''}
  confirmLabel="Remove"
  destructive
  onConfirm={() => void confirmRemoval()}
  onCancel={() => {
    pendingRemoval = null;
  }}
/>
