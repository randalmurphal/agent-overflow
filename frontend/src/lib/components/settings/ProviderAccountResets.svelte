<script lang="ts">
  // A Codex account's "Usage resets (N)" button, at the end of the card's
  // limits (ProviderAccountLimits lineEnd), and the menu it opens: one line
  // per kind of reset (Codex's resetType) with its count, its first expiry
  // and a Use behind a confirm. The list is the backend's
  // (ManagedProviderAccount.usageResets), earliest-expiring first; Use spends
  // a line's earliest-expiring reset. Resets Codex counted but did not list
  // (beyond a capped list, or with no list at all) are one plain line with no
  // Use. A spend Codex has not confirmed
  // (pendingReset) is the first line, with Finish, and no Use is offered
  // until it settles.
  //
  // The box the button sits in is always there, sized for the widest label,
  // so the button appearing, its count changing and it going away move
  // nothing. A line narrower than that label wraps it inside the box rather
  // than cut it off. On compact the box is at least the tap target's height
  // less the card's bottom padding (`--card-pad-bottom`, which the card
  // sets), and the target reaches down into that padding.
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import type { ManagedProviderAccount, ProviderAccountResetOption } from '../../stores/bindings';
  import type { ProviderAccountResetChoice } from '../../stores/computerAccounts.svelte';
  import {
    finishProviderAccountReset,
    getProviderAccountActions,
    providerAccountName,
    spendProviderAccountReset,
  } from '../../stores/providerAccounts.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { formatCalendarDate, formatTimeOfDay } from '../../utils/format';
  import { restorePickerFocus } from '../panes/paneComposerFocus';

  let {
    account,
    backend,
    admin,
    credentialOpInFlight,
    ungrantedTitle,
  }: {
    account: ManagedProviderAccount;
    backend: BackendKey;
    admin: boolean;
    /** A switch, sign-in or removal is running for this provider. */
    credentialOpInFlight: boolean;
    ungrantedTitle?: string;
  } = $props();

  /** The count the button shows past which it says "99+"; the box is sized for that label. */
  const MAX_SHOWN = 99;

  interface ResetRow {
    key: string;
    label: string;
    refills: string;
    left: number;
    /** Unix seconds; 0 when it does not expire. */
    expiresAt: number;
    usable: boolean;
    reason: string;
    /** What Use spends: the row's earliest-expiring option. */
    spend: ProviderAccountResetOption;
  }

  // What the person confirmed, as the menu showed it when Use was pressed.
  // The spend sends this, not the list as it is at confirm time.
  interface Confirming extends ProviderAccountResetChoice {
    label: string;
    refills: string;
    accountName: string;
  }

  // One row per kind and state: usable resets of a kind add up into one row,
  // and each reason a reset can't be used now is a row of its own. The label
  // is the first reset's; it does not decide the grouping.
  let rows = $derived.by((): ResetRow[] => {
    const out: ResetRow[] = [];
    const byKey = new Map<string, ResetRow>();
    for (const option of account.usageResets?.options ?? []) {
      const reason = option.usable ? '' : (option.reason ?? '');
      const key = `${option.kind}\u0000${option.usable}\u0000${reason}`;
      const row = byKey.get(key);
      if (row) {
        row.left += 1;
        continue;
      }
      const next: ResetRow = {
        key,
        label: option.label,
        refills: option.refills ?? '',
        left: 1,
        expiresAt: option.expiresAt ?? 0,
        usable: option.usable,
        reason,
        spend: option,
      };
      byKey.set(key, next);
      out.push(next);
    }
    return out;
  });

  let unlisted = $derived(account.usageResets?.unlisted ?? 0);
  let unsettled = $derived(account.pendingReset ?? null);
  let finishLabel = $derived(
    unsettled ? `Finish the ${unsettled.label} on ${providerAccountName(account)}` : '',
  );
  let usableCount = $derived(rows.reduce((sum, row) => (row.usable ? sum + row.left : sum), 0) + unlisted);
  let shownCount = $derived(usableCount > MAX_SHOWN ? `${MAX_SHOWN}+` : `${usableCount}`);
  let shown = $derived(rows.length > 0 || unlisted > 0 || unsettled !== null);

  let actions = $derived(getProviderAccountActions('codex', backend));
  let spendingHere = $derived(actions.spendingResetID === account.id);
  // Why Use and Finish can't be pressed now; empty when they can.
  let blockedReason = $derived.by((): string => {
    if (!admin) return ungrantedTitle ?? 'This session cannot manage accounts.';
    if (account.needsLogin) return "Sign in again to use this account's resets.";
    if (credentialOpInFlight) return 'Wait for the account action that is running to finish.';
    if (actions.spendingResetID !== '' || actions.refreshingID !== '') {
      return 'Wait for the reset or refresh that is running to finish.';
    }
    return '';
  });

  let trigger: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let confirming = $state<Confirming | null>(null);

  function close(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl: trigger });
  }

  // The button goes away with the last thing it showed, and the menu with it.
  $effect(() => {
    if (open && !shown) open = false;
  });

  function detail(row: ResetRow): string {
    const parts = [`${row.left} left`];
    if (row.expiresAt > 0) parts.push(`first expires ${formatCalendarDate(row.expiresAt * 1000)}`);
    return parts.join(' · ');
  }

  function startUse(row: ResetRow): void {
    const revision = account.usageResets?.revision;
    if (revision === undefined) return;
    confirming = {
      accountId: account.id,
      optionId: row.spend.id,
      revision,
      label: row.label,
      refills: row.refills,
      accountName: providerAccountName(account),
    };
    // Focus goes back to the button before the confirm opens, so the confirm
    // returns it there.
    trigger?.focus();
    open = false;
  }

  function finish(spendId: string): void {
    trigger?.focus();
    open = false;
    void finishProviderAccountReset('codex', account.id, spendId, backend);
  }

  function confirmDescription(choice: Confirming): string {
    const refills = choice.refills ? ` ${choice.refills}` : '';
    return `Use one ${choice.label} on ${choice.accountName}?${refills} A reset can't be given back once it is used.`;
  }

  function confirmSpend(): void {
    const choice = confirming;
    if (!choice) return;
    confirming = null;
    void spendProviderAccountReset('codex', choice, backend);
  }

  const LABEL_CLASS = 'inline-flex items-center gap-0.5 px-1 font-medium';
</script>

<!-- The invisible sizer holds the box's width and, off compact, its height.
     The button fills the box with its label at the top, level with the
     line's text. -->
<span
  class="ml-auto inline-grid max-w-full shrink-0 text-[0.625rem] compact:min-h-[calc(2.25rem-var(--card-pad-bottom))]"
  data-testid="provider-account-resets-box-{account.id}"
>
  <span class="invisible col-start-1 row-start-1 {LABEL_CLASS}" aria-hidden="true">
    Usage resets ({MAX_SHOWN}+)
    <Icon icon={ChevronDown} size={10} strokeWidth={1.75} />
  </span>
  {#if shown}
    <button
      bind:this={trigger}
      type="button"
      class="col-start-1 row-start-1 flex cursor-pointer items-start justify-end rounded-[var(--radius-field)] transition-colors hover:bg-surface-2/40 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:-mb-[var(--card-pad-bottom)] compact:select-none {unsettled
        ? 'text-warning hover:text-warning'
        : 'text-fg-hint hover:text-fg-muted'}"
      aria-haspopup="menu"
      aria-expanded={open}
      aria-label={unsettled
        ? `Usage resets for ${providerAccountName(account)}, ${usableCount} usable, one not confirmed`
        : `Usage resets for ${providerAccountName(account)}, ${usableCount} usable`}
      title={unsettled ? 'A reset is not confirmed yet. Open to finish it.' : undefined}
      data-testid="provider-account-resets-{account.id}"
      data-unsettled={unsettled ? 'true' : undefined}
      onclick={() => (open = !open)}
    >
      <span class={LABEL_CLASS}>
        Usage resets ({shownCount})
        <Icon icon={ChevronDown} size={10} strokeWidth={1.75} />
      </span>
    </button>
  {/if}
</span>
{#if shown}
  <!-- claimTab: the menu is portaled, so a Tab from it would leave a modal
       around the card (the switcher); it closes back onto the button. -->
  <Popover anchor={trigger} {open} onClose={close} placement="bottom-end" role="none" claimTab restoreFocusTo={trigger}>
    <div data-testid="provider-account-resets-list-{account.id}">
      <Menu
        ariaLabel={`Usage resets for ${providerAccountName(account)}`}
        minWidthClass="w-72 max-w-[calc(100vw-1rem)] min-w-0"
        onClose={close}
      >
        {#if unsettled}
          <div role="none" data-testid="provider-account-reset-unsettled">
            <MenuItem
              wrap
              label={`${unsettled.label} not confirmed`}
              description={`Started at ${formatTimeOfDay(unsettled.startedAt)}. Finish completes it without using a second reset.`}
              disabled={blockedReason !== ''}
              title={blockedReason || undefined}
              actionText={spendingHere ? 'Finishing…' : 'Finish'}
              actionLabel={finishLabel}
              actionDisabled={blockedReason !== ''}
              actionTitle={blockedReason || undefined}
              testId="provider-account-reset-finish"
              onSelect={() => finish(unsettled.id)}
              onAction={() => finish(unsettled.id)}
            />
          </div>
        {/if}
        {#each rows as row (row.key)}
          <div role="none" data-testid="provider-account-reset-row" data-usable={row.usable ? 'true' : 'false'}>
            {#if row.usable && !unsettled}
              <MenuItem
                wrap
                label={row.label}
                description={detail(row)}
                disabled={blockedReason !== ''}
                title={blockedReason || row.refills || undefined}
                actionText="Use"
                actionLabel={`Use a ${row.label} on ${providerAccountName(account)}`}
                actionDisabled={blockedReason !== ''}
                actionTitle={blockedReason || row.refills || undefined}
                testId="provider-account-reset-use"
                onSelect={() => startUse(row)}
                onAction={() => startUse(row)}
              />
            {:else}
              <MenuItem
                wrap
                label={row.label}
                description={row.usable ? detail(row) : row.reason || "Can't be used right now."}
                disabled
              />
            {/if}
          </div>
        {/each}
        {#if unlisted > 0}
          <div role="none" data-testid="provider-account-reset-unlisted">
            <MenuItem
              wrap
              label={rows.length > 0 ? `${unlisted} more` : `${unlisted} available`}
              description="Codex counted these but didn't list them, so they can't be picked here."
              disabled
            />
          </div>
        {/if}
      </Menu>
    </div>
  </Popover>
{/if}

<ConfirmDialog
  open={confirming !== null}
  title={confirming ? `Use a ${confirming.label}?` : ''}
  description={confirming ? confirmDescription(confirming) : ''}
  confirmLabel="Use reset"
  onConfirm={confirmSpend}
  onCancel={() => {
    confirming = null;
  }}
/>
