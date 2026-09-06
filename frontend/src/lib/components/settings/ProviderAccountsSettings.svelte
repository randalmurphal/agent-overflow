<script lang="ts">
  import { settingsComputer } from './settingsComputer';
  const { backend } = settingsComputer();
  // The Accounts section of a provider's settings page. The account
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
    providerAccountName,
    providerAccountOrgLabel,
    providerLabel as resolveProviderLabel,
    refreshProviderAccountUsage,
    removeProviderAccount,
    switchProviderAccount,
  } from '../../stores/providerAccounts.svelte';
  import { getProviderRateLimits } from '../../stores/rateLimitsInfo.svelte';
  import { providerFieldId, type SettingsProvider } from './fields';
  import SettingsHeader from './SettingsHeader.svelte';
  import { PRIMARY_BUTTON_CLASS, GHOST_BUTTON_CLASS } from './styles';

  let { provider }: { provider: SettingsProvider } = $props();

  let providerLabel = $derived(resolveProviderLabel(provider));
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
</script>

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
        disabled={credentialOpInFlight}
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
    <div class="flex flex-col gap-2">
      {#each accounts as account (account.id)}
        {@const limits = getProviderRateLimits(provider, account.id, backend)}
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
                onclick={() => void refreshProviderAccountUsage(provider, account, backend)}
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
