<script lang="ts">
  // Per-provider Accounts block inside Settings → Providers. The account
  // logic itself lives in stores/providerAccounts.svelte.ts — shared with the
  // account-switcher picker, so a switch made in either surface is the same
  // state change. This component owns only the settings-side chrome: the
  // "log in to another account" button, the cards, and the removal confirm.

  import { onMount } from 'svelte';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import Icon from '../primitives/Icon.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import ProviderAccountLimits from '../shared/ProviderAccountLimits.svelte';
  import type { ManagedProviderAccount } from '../../stores/bindings';
  import {
    getProviderAccountActions,
    getProviderAccountsFor,
    isProviderAccountsLoading,
    isProviderCredentialOpInFlight,
    loadProviderAccounts,
    loginProviderAccount,
    providerAccountActionLabel,
    providerAccountName,
    providerAccountOrgLabel,
    providerLabel as resolveProviderLabel,
    refreshProviderAccountUsage,
    removeProviderAccount,
    switchProviderAccount,
  } from '../../stores/providerAccounts.svelte';
  import { getProviderRateLimits } from '../../stores/rateLimitsInfo.svelte';
  import type { ProviderID } from '../../types/providers';
  import { PRIMARY_BUTTON_CLASS, GHOST_BUTTON_CLASS } from './styles';

  let { provider }: { provider: ProviderID } = $props();

  let providerLabel = $derived(resolveProviderLabel(provider));
  let accounts = $derived(getProviderAccountsFor(provider));
  let loading = $derived(isProviderAccountsLoading());
  let actions = $derived(getProviderAccountActions(provider));
  // One answer for "is a credential op running", shared with the store's own
  // refusal, so a disabled button and a rejected call can never disagree.
  let credentialOpInFlight = $derived(isProviderCredentialOpInFlight(provider));
  let pendingRemoval = $state<ManagedProviderAccount | null>(null);

  onMount(() => {
    void loadProviderAccounts();
  });

  function requestRemoval(account: ManagedProviderAccount): void {
    if (credentialOpInFlight) return;
    pendingRemoval = account;
  }

  async function confirmRemoval(): Promise<void> {
    const account = pendingRemoval;
    if (!account) return;
    pendingRemoval = null;
    await removeProviderAccount(provider, account);
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
    if (account.needsLogin) return () => void loginProviderAccount(provider);
    return () => void switchProviderAccount(provider, account);
  }
</script>

<div class="mt-5 border-t border-border-subtle pt-4" data-testid="provider-accounts-{provider}">
  <div class="flex items-center justify-between gap-3">
    <div>
      <h4 class="text-[0.75rem] font-medium text-fg">Accounts</h4>
      <p class="mt-0.5 text-[0.6875rem] text-fg-hint">
        Saved native logins and their last-known limits.
      </p>
    </div>
    <button
      type="button"
      class={PRIMARY_BUTTON_CLASS}
      disabled={credentialOpInFlight}
      onclick={() => void loginProviderAccount(provider)}
    >
      {actions.loggingIn ? 'Waiting for login…' : 'Log in to another account'}
    </button>
  </div>

  {#if loading}
    <p class="mt-3 text-[0.71875rem] text-fg-hint">Loading accounts…</p>
  {:else if accounts.length === 0}
    <p class="mt-3 text-[0.71875rem] text-fg-hint">
      The current native login will be saved automatically when detected.
    </p>
  {:else}
    <div class="mt-3 flex flex-col gap-2">
      {#each accounts as account (account.id)}
        {@const limits = getProviderRateLimits(provider, account.id)}
        {@const orgLabel = providerAccountOrgLabel(account)}
        <div
          class="rounded-[var(--radius-field)] border px-3 py-2.5 {account.needsLogin
            ? 'border-warning/40 bg-surface-0'
            : account.active
              ? 'border-accent/45 bg-accent/5'
              : 'border-border-subtle bg-surface-0'}"
          data-testid="provider-account-{account.id}"
        >
          <div class="flex items-start justify-between gap-3">
            <button
              type="button"
              class="min-w-0 flex-1 cursor-pointer text-left disabled:cursor-default"
              disabled={(account.active && !account.needsLogin) || credentialOpInFlight}
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
                {#if account.needsLogin}
                  <span class="rounded-full bg-warning/10 px-1.5 py-0.5 text-[0.625rem] font-medium text-warning">
                    Sign in again
                  </span>
                {:else if account.active}
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
              {#if account.needsLogin}
                <span class="mt-0.5 block pl-4 text-[0.6875rem] text-fg-hint">
                  Its saved {providerLabel} credentials are gone.
                </span>
              {/if}
            </button>

            <div class="flex shrink-0 items-center gap-1">
              <button
                type="button"
                class={GHOST_BUTTON_CLASS}
                disabled={account.needsLogin || !!actions.refreshingID || credentialOpInFlight}
                onclick={() => void refreshProviderAccountUsage(provider, account)}
                title={account.needsLogin
                  ? 'Sign in again to refresh usage limits'
                  : 'Refresh usage limits'}
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
                disabled={credentialOpInFlight}
                onclick={() => requestRemoval(account)}
                title="Remove saved account"
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
