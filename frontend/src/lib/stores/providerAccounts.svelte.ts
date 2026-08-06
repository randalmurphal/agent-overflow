// Saved native provider accounts (Claude + Codex): the ONE load / login /
// switch / refresh / remove path in the app.
//
// Two surfaces consume it — Settings → Providers → Accounts (per-provider
// slice, with removal) and the account-switcher picker (all providers, switch
// + refresh only). They share this module rather than each owning a copy so a
// switch made in one is immediately visible in the other, and so there is
// exactly one place that projects a listing into the downstream stores
// (`accountInfo` = which account is selected per provider; `rateLimitsInfo` =
// last-known quotas per account).
//
// `ListProviderAccounts` answers for every provider at once, so the cache is
// one flat list and callers slice it. In-flight flags, in contrast, are
// PER-PROVIDER: logging in to Claude must not disable the Codex buttons, and
// an account id belongs to exactly one provider anyway.
//
// Every one of these RPCs is LocalOnly on the transport
// (internal/transport/internalmethods.go). The command that opens the picker
// is gated on `!viewOnlySession`, but the listing is also pulled unprompted at
// startup, so `loadProviderAccounts` refuses in a view-only session rather than
// relying on every entry point to remember.

import {
  ListProviderAccounts,
  LoginProviderAccount,
  RefreshProviderAccountUsage,
  RemoveProviderAccount,
  SwitchProviderAccount,
} from './bindings';
import type { ManagedProviderAccount } from './bindings';
import { clearProviderAccount, setProviderAccount } from './accountInfo.svelte';
import { clearProviderRateLimits, setProviderRateLimits } from './rateLimitsInfo.svelte';
import { addToast } from './toast.svelte';
import { getProviderDefinition, PROVIDER_SETTINGS_ORDER } from '../providers/catalog';
import { isViewOnlySession } from '../transport/runMode';
import { PROVIDER_IDS, type ProviderID } from '../types/providers';
import { userFacingError } from '../utils/userFacingError';

/** In-flight action state for one provider. */
export interface ProviderAccountActions {
  loggingIn: boolean;
  /** Account id being switched to. Empty string = nothing in flight. */
  switchingID: string;
  /** Account id whose usage is being re-probed. Empty string = nothing. */
  refreshingID: string;
  /** Account id being removed. Empty string = nothing in flight. */
  removingID: string;
}

/** One provider's section of the listing, in provider-settings order. */
export interface ProviderAccountGroup {
  provider: ProviderID;
  label: string;
  accounts: ManagedProviderAccount[];
}

// Providers that own native logins. claude-tui reuses Claude's credentials and
// has no account surface of its own, so it never appears here.
const ACCOUNT_PROVIDERS: readonly ProviderID[] = PROVIDER_SETTINGS_ORDER;

function idleActions(): ProviderAccountActions {
  return { loggingIn: false, switchingID: '', refreshingID: '', removingID: '' };
}

function seedActions(): Record<ProviderID, ProviderAccountActions> {
  const seeded = {} as Record<ProviderID, ProviderAccountActions>;
  // Seeded for EVERY provider id, not just the account-capable ones, so
  // `actions[provider]` is total — a lazily-created entry would have to be
  // written during a tracked read, which Svelte forbids inside a derived.
  for (const provider of PROVIDER_IDS) seeded[provider] = idleActions();
  return seeded;
}

let accounts = $state<ManagedProviderAccount[]>([]);
let loading = $state(true);
let actions = $state<Record<ProviderID, ProviderAccountActions>>(seedActions());
// Monotonic per-load token. Responses that are no longer the newest request
// are dropped rather than projected, so a slow first load can't overwrite the
// listing a later switch already refreshed.
let loadGeneration = 0;
// The one in-flight load, shared by concurrent callers. Settings mounts an
// accounts block per provider and the picker adds a third caller, so an open
// would otherwise fire N identical `ListProviderAccounts` RPCs — each of which
// costs a per-account credential check backend-side.
let pendingLoad: Promise<void> | null = null;

/** Display name for a provider, as every account surface spells it. */
export function providerLabel(provider: ProviderID): string {
  return getProviderDefinition(provider).label;
}

/** Every saved account, all providers, in backend order. */
export function getProviderAccounts(): ManagedProviderAccount[] {
  return accounts;
}

/** One provider's saved accounts. */
export function getProviderAccountsFor(provider: ProviderID): ManagedProviderAccount[] {
  return accounts.filter((account) => account.provider === provider);
}

/**
 * Sections for a cross-provider surface: account-capable providers in settings
 * order, each with its accounts. Providers with nothing saved are included —
 * the caller decides whether an empty section is worth rendering.
 */
export function getProviderAccountGroups(): ProviderAccountGroup[] {
  return ACCOUNT_PROVIDERS.map((provider) => ({
    provider,
    label: providerLabel(provider),
    accounts: getProviderAccountsFor(provider),
  }));
}

/** True until the first listing lands. Later reloads do not re-raise it. */
export function isProviderAccountsLoading(): boolean {
  return loading;
}

export function getProviderAccountActions(provider: ProviderID): ProviderAccountActions {
  return actions[provider];
}

/**
 * True while a credential-mutating op (login / switch / remove) is running for
 * this provider. All three check it before starting, so the ordering safety is
 * in the API rather than in each caller's `disabled` expression — and the UI
 * disables against the same predicate, so button state and API refusal can
 * never disagree.
 *
 * A usage refresh is deliberately NOT part of it: it reads quotas and mutates
 * nothing, so blocking a switch behind a slow probe would be wrong.
 */
export function isProviderCredentialOpInFlight(provider: ProviderID): boolean {
  const action = actions[provider];
  return action.loggingIn || action.switchingID !== '' || action.removingID !== '';
}

/** The name a card, row, or confirm dialog shows for an account. */
export function providerAccountName(account: ManagedProviderAccount): string {
  return account.displayName || account.email || account.subscriptionType || 'Saved account';
}

/**
 * The accessible name for the control that selects an account. Both surfaces
 * (settings card, picker row) render the same three states, so they name them
 * with the same words.
 */
export function providerAccountActionLabel(account: ManagedProviderAccount): string {
  const name = providerAccountName(account);
  if (account.needsLogin) return `Sign in again to ${name}`;
  if (account.active) return `${name} is active`;
  return `Switch to ${name}`;
}

/**
 * Fetch the listing and project it into `accountInfo` / `rateLimitsInfo`.
 *
 * Concurrent callers share one RPC: whoever asks while a load is in flight
 * awaits that load instead of starting a second one. Sequential reloads still
 * race properly — the generation token drops a response that a newer load has
 * already superseded.
 *
 * Never rejects: a failure is reported to the user as a toast and leaves the
 * previous listing in place, because a transient RPC failure is not evidence
 * that the user's accounts went away.
 */
export function loadProviderAccounts(): Promise<void> {
  // Every RPC in this module is LocalOnly on the transport, so in a view-only
  // remote session the listing call can only be refused — and this one runs
  // unprompted at startup (eventsProvider's hydrate) and on transport-gap
  // recovery, where the refusal would surface as an unexplained error toast.
  // Settle into "loaded, nothing saved" instead of asking.
  if (isViewOnlySession()) {
    loading = false;
    return Promise.resolve();
  }
  return pendingLoad ?? startLoad();
}

/**
 * Reload after a mutation. Never joins an in-flight request, because a listing
 * fetched BEFORE the switch/remove landed cannot describe the state it
 * produced; the generation token then drops that older response.
 */
function reloadProviderAccounts(): Promise<void> {
  return startLoad();
}

function startLoad(): Promise<void> {
  const load = runLoad();
  pendingLoad = load;
  // Attached here, synchronously, so the latch is always cleared before any
  // awaiter's continuation runs — handlers fire in registration order.
  return load.finally(() => {
    if (pendingLoad === load) pendingLoad = null;
  });
}

async function runLoad(): Promise<void> {
  const generation = ++loadGeneration;
  try {
    const listed = (await ListProviderAccounts()) as ManagedProviderAccount[];
    if (generation !== loadGeneration) return;
    accounts = listed ?? [];
    projectListing(accounts);
  } catch (error) {
    console.error('Failed to load provider accounts:', error);
    addToast('error', 'Failed to load provider accounts.');
  } finally {
    if (generation === loadGeneration) loading = false;
  }
}

// Downstream projection of a listing: last-known quotas for every account, and
// the selected account per provider (cleared when the provider has none).
function projectListing(listing: readonly ManagedProviderAccount[]): void {
  for (const provider of ACCOUNT_PROVIDERS) {
    let active: ManagedProviderAccount | null = null;
    for (const account of listing) {
      if (account.provider !== provider) continue;
      if (account.rateLimits) setProviderRateLimits(account.rateLimits);
      if (account.active) active = account;
    }
    if (active) setProviderAccount(provider, active, active.id, active.generation);
    else clearProviderAccount(provider);
  }
}

/**
 * Run the provider's native login flow, then reload. Resolves true when a
 * login completed.
 */
export async function loginProviderAccount(provider: ProviderID): Promise<boolean> {
  if (isProviderCredentialOpInFlight(provider)) return false;
  const action = actions[provider];
  const label = providerLabel(provider);
  action.loggingIn = true;
  try {
    await LoginProviderAccount(provider);
    await reloadProviderAccounts();
    addToast('success', `${label} account connected.`);
    return true;
  } catch (error) {
    console.error(`${label} login failed:`, error);
    addToast('error', userFacingError(error, `${label} login failed.`));
    return false;
  } finally {
    action.loggingIn = false;
  }
}

/**
 * Make `account` the provider's active credential. Resolves true only when the
 * switch landed, so a caller that closes a surface on success keeps it open on
 * failure with the error already toasted.
 */
export async function switchProviderAccount(
  provider: ProviderID,
  account: ManagedProviderAccount,
): Promise<boolean> {
  if (account.active || isProviderCredentialOpInFlight(provider)) return false;
  const action = actions[provider];
  const label = providerLabel(provider);
  action.switchingID = account.id;
  try {
    await SwitchProviderAccount(provider, account.id);
    await reloadProviderAccounts();
    addToast('success', `Switched ${label} account.`);
    return true;
  } catch (error) {
    console.error(`${label} account switch failed:`, error);
    addToast(
      'error',
      `${label} account did not switch. ${userFacingError(error, 'Try again.')}`,
    );
    return false;
  } finally {
    action.switchingID = '';
  }
}

/** Re-read one account's quotas from the provider and reload the listing. */
export async function refreshProviderAccountUsage(
  provider: ProviderID,
  account: ManagedProviderAccount,
): Promise<void> {
  const action = actions[provider];
  if (action.refreshingID) return;
  const label = providerLabel(provider);
  action.refreshingID = account.id;
  try {
    await RefreshProviderAccountUsage(provider, account.id);
    await reloadProviderAccounts();
  } catch (error) {
    console.error(`${label} usage refresh failed:`, error);
    addToast('error', userFacingError(error, `Failed to refresh ${label} usage.`));
  } finally {
    action.refreshingID = '';
  }
}

/**
 * Forget a saved account. The caller owns the confirmation — this is the
 * destructive half only. Resolves true when the account was removed.
 */
export async function removeProviderAccount(
  provider: ProviderID,
  account: ManagedProviderAccount,
): Promise<boolean> {
  if (isProviderCredentialOpInFlight(provider)) return false;
  const action = actions[provider];
  const label = providerLabel(provider);
  action.removingID = account.id;
  try {
    await RemoveProviderAccount(provider, account.id);
    clearProviderRateLimits(provider, account.id);
    projectRemovedAccount(provider, account);
    await reloadProviderAccounts();
    addToast('success', `${label} account removed.`);
    return true;
  } catch (error) {
    console.error(`${label} account removal failed:`, error);
    addToast(
      'error',
      `${label} account was not removed. ${userFacingError(error, 'Try again.')}`,
    );
    return false;
  } finally {
    action.removingID = '';
  }
}

/**
 * Optimistic local projection of a removal, applied before the reload so the
 * card disappears in the same frame the confirm closes. Removing the ACTIVE
 * account promotes the account that takes its slot (the next one down, wrapping
 * to the first) — the same choice the backend makes — so the selected-account
 * badge doesn't blank out for the duration of the reload.
 */
function projectRemovedAccount(provider: ProviderID, account: ManagedProviderAccount): void {
  const providerAccounts = getProviderAccountsFor(provider);
  const removedIndex = providerAccounts.findIndex((candidate) => candidate.id === account.id);
  // Not in the listing we hold — a caller acting on a snapshot another surface
  // has already reloaded away. There is no slot to promote into, so leave the
  // listing to the reload rather than guessing at a replacement.
  if (removedIndex < 0) return;
  const remaining = providerAccounts.filter((candidate) => candidate.id !== account.id);
  const others = accounts.filter(
    (candidate) => candidate.provider !== provider || candidate.id !== account.id,
  );

  if (!account.active || remaining.length === 0) {
    accounts = others;
    if (account.active) clearProviderAccount(provider);
    return;
  }

  const nextIndex = removedIndex >= remaining.length ? 0 : removedIndex;
  const replacementID = remaining[nextIndex].id;
  accounts = others.map((candidate) =>
    candidate.provider === provider
      ? { ...candidate, active: candidate.id === replacementID }
      : candidate,
  );
  const replacement = { ...remaining[nextIndex], active: true };
  setProviderAccount(provider, replacement, replacement.id, replacement.generation);
}

/** Test-only reset, mirroring the sibling provider stores. */
export function resetForTest(): void {
  accounts = [];
  loading = true;
  actions = seedActions();
  // Bumped, never zeroed: a load still in flight from the previous test must
  // not match a generation handed out after the reset and project into it.
  loadGeneration++;
  pendingLoad = null;
}
