<script lang="ts">
  import ComputerSelect from '../primitives/ComputerSelect.svelte';
  import { hasMultipleBackends } from '../../stores/attachedBackends.svelte';
  import { selectedBackend } from '../../stores/selectedBackend.svelte';
  let backend = $state(untrack(selectedBackend));
  // Account switcher: the quick surface for the one recurring Settings trip —
  // "which account am I on, how much is left on it, switch me to the other
  // one". Top-aligned picker over every saved Claude and Codex account.
  //
  // It owns no account logic: load / switch / login / refresh all run through
  // stores/providerAccounts.svelte.ts, the same module the provider settings pages
  // uses, so the two surfaces can never disagree about what is active.

  import { untrack } from 'svelte';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Modal from '../primitives/Modal.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import ProviderIcon from '../shared/ProviderIcon.svelte';
  import ProviderAccountLimits from '../shared/ProviderAccountLimits.svelte';
  import ProviderLoginFlow from './ProviderLoginFlow.svelte';
  import { SETTINGS_PROVIDERS } from '../settings/fields';
  import type { ManagedProviderAccount } from '../../stores/bindings';
  import {
    getProviderAccountActions,
    getProviderAccountGroups,
    getProviderLogin,
    isProviderAccountsLoading,
    isProviderCredentialOpInFlight,
    isProviderLoginActive,
    loadProviderAccounts,
    startProviderLogin,
    providerAccountActionLabel,
    providerAccountName,
    providerAccountOrgLabel,
    refreshProviderAccountUsage,
    switchProviderAccount,
  } from '../../stores/providerAccounts.svelte';
  import { getProviderRateLimits } from '../../stores/rateLimitsInfo.svelte';
  import { openSettingsOverlay } from '../../stores/settingsOverlay.svelte';
  import { PROVIDER_SETTINGS_ORDER } from '../../providers/catalog';
  import { hasScope } from '../../transport/scopes';
  import type { ProviderID } from '../../types/providers';
  import { isImeComposingEvent } from '../../utils/imeComposition';

  interface Props {
    open: boolean;
    onClose: () => void;
  }

  let { open, onClose }: Props = $props();

  interface PickerRow {
    provider: ProviderID;
    account: ManagedProviderAccount;
    /** Position across ALL sections — arrow keys walk one flat list. */
    index: number;
  }

  interface PickerGroup {
    provider: ProviderID;
    label: string;
    rows: PickerRow[];
  }

  $effect(() => {
    if (open) backend = untrack(selectedBackend);
  });

  let activeIndex = $state(0);
  let listEl: HTMLDivElement | undefined = $state(undefined);

  let loading = $derived(isProviderAccountsLoading(backend));

  // Only providers with something saved get a section; the flat index is
  // assigned here so the template never has to compute a row's position.
  let groups: PickerGroup[] = $derived.by(() => {
    let index = 0;
    const out: PickerGroup[] = [];
    for (const group of getProviderAccountGroups(backend)) {
      if (group.accounts.length === 0) continue;
      out.push({
        provider: group.provider,
        label: group.label,
        rows: group.accounts.map((account) => ({
          provider: group.provider,
          account,
          index: index++,
        })),
      });
    }
    return out;
  });

  let rows: PickerRow[] = $derived(groups.flatMap((group) => group.rows));

  // A sign-in started from a card in Settings shows here too, and vice versa:
  // one flow, two windows onto it. Failed flows are included, because the
  // reason is what the user needs before trying again.
  let activeLogins = $derived(
    PROVIDER_SETTINGS_ORDER.filter(
      (provider) =>
        isProviderLoginActive(provider, backend) || getProviderLogin(provider, backend).phase === 'failed',
    ),
  );

  // Fresh listing on every open — an account may have been switched from
  // Settings, or its quotas refreshed, since the last time this was up. Only
  // the LISTING is refetched: usage is whatever was last recorded, so opening
  // the picker never fans out a probe per account.
  //
  // The provider-account surface is billing identity, which `access:admin`
  // covers. The command that opens this picker asks the same question, but a
  // browser can reach this effect before the bootstrap manifest resolves the
  // answer, and a refused RPC would surface as a raw failure toast.
  $effect(() => {
    if (!open || !hasScope('access:admin', backend)) return;
    activeIndex = 0;
    void loadProviderAccounts(backend);
    requestAnimationFrame(() => listEl?.focus());
  });

  // Clamp when the list shrinks under the cursor (a removal in Settings). Only
  // the row COUNT may re-run this: reading activeIndex tracked would re-run it
  // on every arrow press and every hover, for a clamp that is already satisfied.
  $effect(() => {
    const count = rows.length;
    untrack(() => {
      if (count === 0) activeIndex = 0;
      else if (activeIndex >= count) activeIndex = count - 1;
      else if (activeIndex < 0) activeIndex = 0;
    });
  });

  async function select(row: PickerRow): Promise<void> {
    const target = backend;
    const { provider, account } = row;
    // A saved account whose credential is gone can't be selected; the login
    // flow resolves back to this same account by identity (email +
    // organization, same behaviour as the Settings card), so the picker stays
    // open for its result.
    if (account.needsLogin) {
      await startProviderLogin(provider, backend);
      return;
    }
    // Picking what is already active is a confirmation, not a no-op button.
    if (account.active) {
      onClose();
      return;
    }
    if (await switchProviderAccount(provider, account, target) && backend === target && open) onClose();
  }

  function refreshLabel(account: ManagedProviderAccount): string {
    const name = providerAccountName(account);
    return account.needsLogin
      ? `Sign in again to refresh usage for ${name}`
      : `Refresh usage for ${name}`;
  }

  // Escape is Modal's; it bubbles from here untouched.
  function handleKeydown(event: KeyboardEvent): void {
    // Keyboard navigation belongs to the list, not to anything Tab-focused
    // inside it. A row button or its refresh button owns Enter/Space itself,
    // and the highlight no longer tracks DOM focus once Tab has moved — so
    // acting on `activeIndex` here would switch an account the user isn't on.
    if (event.target !== listEl) return;
    if (rows.length === 0) return;
    const key = event.key;
    // j/k mirror the arrows when no modifier is held (there is no text input
    // in this picker). With a modifier they fall through so global chords
    // still reach the window-level handler — same rule as Menu.
    const plain =
      !event.ctrlKey && !event.metaKey && !event.altKey && !event.shiftKey;
    if (key === 'ArrowDown' || (plain && key === 'j')) {
      event.preventDefault();
      activeIndex = (activeIndex + 1) % rows.length;
    } else if (key === 'ArrowUp' || (plain && key === 'k')) {
      event.preventDefault();
      activeIndex = (activeIndex - 1 + rows.length) % rows.length;
    } else if (key === 'Enter') {
      if (isImeComposingEvent(event)) return;
      event.preventDefault();
      const row = rows[activeIndex];
      if (row) void select(row);
    }
  }

  function openProviderSettings(): void {
    onClose();
    openSettingsOverlay(SETTINGS_PROVIDERS[0], backend);
  }
</script>

<Modal {open} title="Switch account" {onClose} width="md" padding="tight" align="top">
  {#snippet children()}
    {#if hasMultipleBackends()}
      <div class="px-2 pb-3"><ComputerSelect value={backend} onchange={(value) => { backend = value; }} scope="access:admin" /></div>
    {/if}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div
      bind:this={listEl}
      data-testid="account-switcher"
      tabindex={-1}
      class="focus:outline-none"
      onkeydown={handleKeydown}
    >
      {#each activeLogins as provider (provider)}
        <ProviderLoginFlow {backend} {provider} login={getProviderLogin(provider, backend)} />
      {/each}

      {#if loading && rows.length === 0}
        <p class="px-2 py-3 text-[0.75rem] text-fg-muted" data-testid="account-switcher-loading">
          Loading accounts…
        </p>
      {:else if rows.length === 0}
        <div class="px-2 py-3" data-testid="account-switcher-empty">
          <p class="text-[0.75rem] text-fg-muted">No saved accounts yet.</p>
          <button
            type="button"
            class="mt-2 cursor-pointer rounded-[var(--radius-field)] py-1 text-[0.71875rem] text-fg-hint transition-colors hover:text-accent focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
            onclick={openProviderSettings}
          >
            Open provider settings
          </button>
        </div>
      {:else}
        {#each groups as group (group.provider)}
          {@const actions = getProviderAccountActions(group.provider, backend)}
          {@const credentialOpInFlight = isProviderCredentialOpInFlight(group.provider, backend)}
          <section class="mb-3 last:mb-0" data-testid="account-switcher-group-{group.provider}">
            <h3
              class="mb-1 flex items-center gap-1.5 px-1 text-[0.625rem] font-medium uppercase tracking-[0.14em] text-fg-hint"
            >
              <ProviderIcon provider={group.provider} size={11} />
              {group.label}
            </h3>
            <ul class="flex flex-col gap-1">
              {#each group.rows as row (row.account.id)}
                {@const account = row.account}
                {@const limits = getProviderRateLimits(group.provider, account.id, backend)}
                <li
                  class={[
                    'rounded-[var(--radius-field)] px-2 py-1.5 transition-colors',
                    activeIndex === row.index ? 'bg-accent/10' : 'hover:bg-surface-2/30',
                  ].join(' ')}
                  data-testid="account-switcher-row-{account.id}"
                  data-active={account.active}
                >
                  <div class="flex items-center gap-2">
                    <button
                      type="button"
                      class="min-w-0 flex-1 cursor-pointer text-left disabled:cursor-default"
                      disabled={credentialOpInFlight}
                      aria-current={activeIndex === row.index}
                      aria-label={providerAccountActionLabel(account)}
                      onclick={() => void select(row)}
                      onmouseenter={() => (activeIndex = row.index)}
                    >
                      <span class="flex min-w-0 items-center gap-2">
                        <span
                          class="truncate text-[0.8125rem] {account.needsLogin
                            ? 'text-fg-muted'
                            : 'text-fg'}"
                        >
                          {providerAccountName(account)}
                        </span>
                        {#if providerAccountOrgLabel(account)}
                          <span class="max-w-32 shrink-0 truncate text-[0.6875rem] text-fg-hint">
                            {providerAccountOrgLabel(account)}
                          </span>
                        {/if}
                        {#if account.needsLogin}
                          <span
                            class="shrink-0 rounded-full bg-warning/10 px-1.5 py-0.5 text-[0.625rem] font-medium text-warning"
                          >
                            Sign in again
                          </span>
                        {:else if account.active}
                          <span
                            class="shrink-0 rounded-full bg-accent/10 px-1.5 py-0.5 text-[0.625rem] font-medium text-accent"
                          >
                            Active
                          </span>
                        {/if}
                      </span>
                    </button>
                    <!-- IconButton owns its own chrome and takes no class
                         prop; the wrapper only holds it at its natural size
                         against the truncating name beside it. -->
                    <div class="shrink-0">
                      <IconButton
                        size="sm"
                        label={refreshLabel(account)}
                        disabled={account.needsLogin || !!actions.refreshingID}
                        testId="account-switcher-refresh-{account.id}"
                        onClick={() => void refreshProviderAccountUsage(group.provider, account, backend)}
                      >
                        <Icon
                          icon={RefreshCw}
                          size={12}
                          strokeWidth={1.75}
                          class={actions.refreshingID === account.id ? 'animate-spin' : ''}
                        />
                      </IconButton>
                    </div>
                  </div>
                  <ProviderAccountLimits {limits} />
                </li>
              {/each}
            </ul>
          </section>
        {/each}
      {/if}
    </div>
  {/snippet}
</Modal>
