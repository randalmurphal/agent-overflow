<script lang="ts">
  import { onMount } from 'svelte';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import Trash2 from 'lucide-svelte/icons/trash-2';
  import Icon from '../primitives/Icon.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import ProviderAccountLimits from './ProviderAccountLimits.svelte';
  import {
    ListProviderAccounts,
    LoginProviderAccount,
    RemoveProviderAccount,
    RefreshProviderAccountUsage,
    SwitchProviderAccount,
  } from '../../stores/bindings';
  import type { ManagedProviderAccount } from '../../stores/bindings';
  import {
    clearProviderAccount,
    setProviderAccount,
  } from '../../stores/accountInfo.svelte';
  import {
    clearProviderRateLimits,
    getProviderRateLimits,
    setProviderRateLimits,
  } from '../../stores/rateLimitsInfo.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { ProviderID } from '../../types/providers';
  import { userFacingError } from '../../utils/userFacingError';
  import { PRIMARY_BUTTON_CLASS, GHOST_BUTTON_CLASS } from './styles';

  let {
    provider,
    providerLabel,
  }: {
    provider: ProviderID;
    providerLabel: string;
  } = $props();

  let accounts = $state<ManagedProviderAccount[]>([]);
  let loading = $state(true);
  let loggingIn = $state(false);
  let switchingID = $state('');
  let refreshingID = $state('');
  let removingID = $state('');
  let pendingRemoval = $state<ManagedProviderAccount | null>(null);

  onMount(() => {
    void loadAccounts();
  });

  async function loadAccounts(): Promise<void> {
    try {
      const all = await ListProviderAccounts();
      accounts = all.filter((account) => account.provider === provider);
      let foundActive = false;
      for (const account of accounts) {
        if (account.rateLimits) setProviderRateLimits(account.rateLimits);
        if (account.active) {
          foundActive = true;
          setProviderAccount(provider, account, account.id, account.generation);
        }
      }
      if (!foundActive) clearProviderAccount(provider);
    } catch (error) {
      console.error(`Failed to load ${providerLabel} accounts:`, error);
      addToast('error', `Failed to load ${providerLabel} accounts.`);
    } finally {
      loading = false;
    }
  }

  async function login(): Promise<void> {
    loggingIn = true;
    try {
      await LoginProviderAccount(provider);
      await loadAccounts();
      addToast('success', `${providerLabel} account connected.`);
    } catch (error) {
      console.error(`${providerLabel} login failed:`, error);
      addToast('error', userFacingError(error, `${providerLabel} login failed.`));
    } finally {
      loggingIn = false;
    }
  }

  async function switchAccount(account: ManagedProviderAccount): Promise<void> {
    if (account.active || switchingID) return;
    switchingID = account.id;
    try {
      await SwitchProviderAccount(provider, account.id);
      await loadAccounts();
      addToast('success', `Switched ${providerLabel} account.`);
    } catch (error) {
      console.error(`${providerLabel} account switch failed:`, error);
      addToast(
        'error',
        `${providerLabel} account did not switch. ${userFacingError(error, 'Try again.')}`,
      );
    } finally {
      switchingID = '';
    }
  }

  async function refreshUsage(account: ManagedProviderAccount): Promise<void> {
    refreshingID = account.id;
    try {
      await RefreshProviderAccountUsage(provider, account.id);
      await loadAccounts();
    } catch (error) {
      console.error(`${providerLabel} usage refresh failed:`, error);
      addToast('error', userFacingError(error, `Failed to refresh ${providerLabel} usage.`));
    } finally {
      refreshingID = '';
    }
  }

  function requestRemoval(account: ManagedProviderAccount): void {
    if (removingID) return;
    pendingRemoval = account;
  }

  async function confirmRemoval(): Promise<void> {
    const account = pendingRemoval;
    if (!account) return;
    pendingRemoval = null;
    removingID = account.id;
    try {
      await RemoveProviderAccount(provider, account.id);
      clearProviderRateLimits(provider, account.id);
      projectRemovedAccount(account);
      await loadAccounts();
      addToast('success', `${providerLabel} account removed.`);
    } catch (error) {
      console.error(`${providerLabel} account removal failed:`, error);
      addToast(
        'error',
        `${providerLabel} account was not removed. ${userFacingError(error, 'Try again.')}`,
      );
    } finally {
      removingID = '';
    }
  }

  function projectRemovedAccount(account: ManagedProviderAccount): void {
    const removedIndex = accounts.findIndex((candidate) => candidate.id === account.id);
    const remaining = accounts.filter((candidate) => candidate.id !== account.id);
    if (!account.active || remaining.length === 0) {
      accounts = remaining;
      if (account.active) clearProviderAccount(provider);
      return;
    }

    const nextIndex = removedIndex >= remaining.length ? 0 : Math.max(0, removedIndex);
    const replacementID = remaining[nextIndex].id;
    accounts = remaining.map((candidate) => ({
      ...candidate,
      active: candidate.id === replacementID,
    }));
    const replacement = accounts[nextIndex];
    setProviderAccount(provider, replacement, replacement.id, replacement.generation);
  }

  function removalDescription(account: ManagedProviderAccount): string {
    if (!account.active) {
      return `Remove ${accountName(account)} from saved accounts? You’ll need to log in again to use it.`;
    }
    if (accounts.length === 1) {
      return `Remove ${accountName(account)} and sign out of ${providerLabel}? You’ll need to log in again to use it.`;
    }
    return `Remove ${accountName(account)}? The next saved ${providerLabel} account will become active.`;
  }

  function accountName(account: ManagedProviderAccount): string {
    return account.displayName || account.email || account.subscriptionType || 'Saved account';
  }

  // A saved account whose credential is gone cannot be selected. Logging in
  // resolves back to this same account by email, so it keeps its usage
  // history rather than becoming a second card.
  function cardAction(account: ManagedProviderAccount): () => void {
    if (account.needsLogin) return () => void login();
    return () => void switchAccount(account);
  }

  function cardActionLabel(account: ManagedProviderAccount): string {
    if (account.needsLogin) return `Sign in again to ${accountName(account)}`;
    if (account.active) return `${accountName(account)} is active`;
    return `Switch to ${accountName(account)}`;
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
      disabled={loggingIn}
      onclick={() => void login()}
    >
      {loggingIn ? 'Waiting for login…' : 'Log in to another account'}
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
              disabled={(account.active && !account.needsLogin)
                || !!switchingID
                || (account.needsLogin && loggingIn)}
              onclick={cardAction(account)}
              aria-label={cardActionLabel(account)}
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
                  {accountName(account)}
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
              {#if account.email && account.displayName}
                <span class="mt-0.5 block truncate pl-4 text-[0.6875rem] text-fg-hint">
                  {account.email}
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
                disabled={account.needsLogin || !!refreshingID || !!removingID}
                onclick={() => void refreshUsage(account)}
                title={account.needsLogin
                  ? 'Sign in again to refresh usage limits'
                  : 'Refresh usage limits'}
                aria-label={`Refresh usage for ${accountName(account)}`}
              >
                <Icon
                  icon={RefreshCw}
                  size={12}
                  strokeWidth={1.75}
                  class={refreshingID === account.id ? 'animate-spin' : ''}
                />
              </button>
              <button
                type="button"
                class="{GHOST_BUTTON_CLASS} hover:text-error"
                disabled={!!removingID || !!switchingID}
                onclick={() => requestRemoval(account)}
                title="Remove saved account"
                aria-label={`Remove ${accountName(account)}`}
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
