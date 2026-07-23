<script lang="ts">
  import { onMount } from 'svelte';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import Icon from '../primitives/Icon.svelte';
  import {
    ListProviderAccounts,
    LoginProviderAccount,
    RefreshProviderAccountUsage,
    SwitchProviderAccount,
  } from '../../stores/bindings';
  import type { ManagedProviderAccount } from '../../stores/bindings';
  import { setProviderAccount } from '../../stores/accountInfo.svelte';
  import {
    getProviderRateLimits,
    rateLimitDisplayName,
    setProviderRateLimits,
  } from '../../stores/rateLimitsInfo.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { ProviderID } from '../../types/providers';
  import { formatResetCountdown } from '../../utils/format';
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

  onMount(() => {
    void loadAccounts();
  });

  async function loadAccounts(): Promise<void> {
    try {
      const all = await ListProviderAccounts();
      accounts = all.filter((account) => account.provider === provider);
      for (const account of accounts) {
        if (account.rateLimits) setProviderRateLimits(account.rateLimits);
        if (account.active) {
          setProviderAccount(provider, account, account.id);
        }
      }
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

  function accountName(account: ManagedProviderAccount): string {
    return account.displayName || account.email || account.subscriptionType || 'Saved account';
  }

  function limitLabel(limit: { limitId: string; limitName: string; windowMins: number }): string {
    const name = rateLimitDisplayName(limit);
    if (limit.windowMins <= 0) return name || 'Usage limit';
    const window = limit.windowMins === 300
      ? '5h'
      : limit.windowMins === 10080
        ? '7d'
        : `${Math.max(1, Math.round(limit.windowMins / 60))}h`;
    if (!name || name.toLowerCase() === window.toLowerCase()) return window;
    return `${name} · ${window}`;
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
          class="rounded-[var(--radius-field)] border px-3 py-2.5 {account.active
            ? 'border-accent/45 bg-accent/5'
            : 'border-border-subtle bg-surface-0'}"
          data-testid="provider-account-{account.id}"
        >
          <div class="flex items-start justify-between gap-3">
            <button
              type="button"
              class="min-w-0 flex-1 cursor-pointer text-left disabled:cursor-default"
              disabled={account.active || !!switchingID}
              onclick={() => void switchAccount(account)}
              aria-label={account.active
                ? `${accountName(account)} is active`
                : `Switch to ${accountName(account)}`}
            >
              <span class="flex items-center gap-2">
                <span
                  class="h-2 w-2 shrink-0 rounded-full border {account.active
                    ? 'border-accent bg-accent'
                    : 'border-fg-hint'}"
                  aria-hidden="true"
                ></span>
                <span class="truncate text-[0.75rem] font-medium text-fg">
                  {accountName(account)}
                </span>
                {#if account.active}
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
            </button>

            <button
              type="button"
              class={GHOST_BUTTON_CLASS}
              disabled={!!refreshingID}
              onclick={() => void refreshUsage(account)}
              title="Refresh usage limits"
              aria-label={`Refresh usage for ${accountName(account)}`}
            >
              <Icon
                icon={RefreshCw}
                size={12}
                strokeWidth={1.75}
                class={refreshingID === account.id ? 'animate-spin' : ''}
              />
            </button>
          </div>

          {#if limits.length > 0}
            <div class="mt-2 grid gap-x-4 gap-y-1.5 sm:grid-cols-2">
              {#each limits as limit (`${limit.limitId}:${limit.windowMins}`)}
                <div class="min-w-0">
                  <div class="flex items-center justify-between gap-2 text-[0.65625rem]">
                    <span class="truncate text-fg-muted">{limitLabel(limit)}</span>
                    <span class="tabular-nums text-fg-hint">{Math.round(limit.usedPercent)}%</span>
                  </div>
                  <div class="mt-1 h-1 overflow-hidden rounded-full bg-surface-3">
                    <div
                      class="h-full rounded-full bg-accent"
                      style:width={`${Math.max(0, Math.min(100, limit.usedPercent))}%`}
                    ></div>
                  </div>
                  {#if limit.resetsAt > 0}
                    <p class="mt-0.5 text-[0.625rem] text-fg-hint">
                      {formatResetCountdown(limit.resetsAt)}
                    </p>
                  {/if}
                </div>
              {/each}
            </div>
          {:else}
            <p class="mt-2 pl-4 text-[0.65625rem] text-fg-hint">Usage not checked yet.</p>
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</div>
