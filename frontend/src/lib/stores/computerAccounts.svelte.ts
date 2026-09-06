import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
// Saved provider logins and pending sign-in flows belong to one computer.
// The picker and Settings share these operations; every awaited RPC captures
// that computer, so navigation cannot redirect a credential mutation.
import {
  CancelProviderLogin,
  GetProviderLoginState,
  ListProviderAccounts,
  ProviderLoginMethod,
  ProviderLoginPhase,
  ProviderLoginState,
  RefreshProviderAccountUsage,
  RemoveProviderAccount,
  StartProviderLogin,
  SubmitProviderLoginCode,
  SwitchProviderAccount,
} from './bindings';
import type { ManagedProviderAccount } from './bindings';
import { canUseHostOpenExternalURL } from '../utils/externalLinks';
import { clearProviderAccount, setProviderAccount } from './accountInfo.svelte';
import { clearProviderRateLimits, setProviderRateLimits } from './rateLimitsInfo.svelte';
import { addToast } from './toast.svelte';
import { recheckProviderAccount } from '../providers/actions';
import { PROVIDER_SETTINGS_ORDER } from '../providers/catalog';
import { hasScope, pageGrantsResolved } from '../transport/scopes';
import { PROVIDER_IDS, type ProviderID } from '../types/providers';
import { userFacingError } from '../utils/userFacingError';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { providerLabel } from './providerAccountLabels';

export interface ProviderAccountActions {
  loggingIn: boolean;
  /** Account id being switched to. Empty string = nothing in flight. */
  switchingID: string;
  /** Account id whose usage is being re-probed. Empty string = nothing. */
  refreshingID: string;
  /** Account id being removed. Empty string = nothing in flight. */
  removingID: string;
}
export interface ProviderAccountGroup {
  provider: ProviderID;
  label: string;
  accounts: ManagedProviderAccount[];
}

export class ComputerAccounts {
  constructor(private readonly backend: BackendKey) {}
  private disposed = false;

  private async call<T>(issue: () => PromiseLike<T>): Promise<T> {
    if (this.disposed) throw new Error('Computer was removed.');
    const result = await withBackendTarget(this.backend, issue);
    if (this.disposed) throw new Error('Computer was removed.');
    return result;
  }

  dispose(): void {
    this.disposed = true;
    this.loadGeneration++;
  }
  // Providers that own native logins. claude-tui reuses Claude's credentials and
  // has no account surface of its own, so it never appears here.
  private ACCOUNT_PROVIDERS: readonly ProviderID[] = PROVIDER_SETTINGS_ORDER;

  private idleActions(): ProviderAccountActions {
    return { loggingIn: false, switchingID: '', refreshingID: '', removingID: '' };
  }

  private seedActions(): Record<ProviderID, ProviderAccountActions> {
    const seeded = {} as Record<ProviderID, ProviderAccountActions>;
    // Seeded for EVERY provider id, not just the account-capable ones, so
    // `actions[provider]` is total — a lazily-created entry would have to be
    // written during a tracked read, which Svelte forbids inside a derived.
    for (const provider of PROVIDER_IDS) seeded[provider] = this.idleActions();
    return seeded;
  }

  private accounts = $state<ManagedProviderAccount[]>([]);

  private loading = $state(true);

  private actions = $state<Record<ProviderID, ProviderAccountActions>>(this.seedActions());

  // Monotonic per-load token. Responses that are no longer the newest request
  // are dropped rather than projected, so a slow first load can't overwrite the
  // listing a later switch already refreshed.
  private loadGeneration = 0;

  // The one in-flight load, shared by concurrent callers. Settings mounts an
  // accounts block per provider and the picker adds a third caller, so an open
  // would otherwise fire N identical `ListProviderAccounts` RPCs — each of which
  // costs a per-account credential check backend-side.
  private pendingLoad: Promise<void> | null = null;

  /** One provider's saved accounts. */
  getProviderAccountsFor(provider: ProviderID): ManagedProviderAccount[] {
    return this.accounts.filter((account) => account.provider === provider);
  }

  /**
   * Sections for a cross-provider surface: account-capable providers in settings
   * order, each with its accounts. Providers with nothing saved are included —
   * the caller decides whether an empty section is worth rendering.
   */
  getProviderAccountGroups(): ProviderAccountGroup[] {
    return this.ACCOUNT_PROVIDERS.map((provider) => ({
      provider,
      label: providerLabel(provider),
      accounts: this.getProviderAccountsFor(provider),
    }));
  }

  /** True until the first listing lands. Later reloads do not re-raise it. */
  isProviderAccountsLoading(): boolean {
    return this.loading;
  }

  getProviderAccountActions(provider: ProviderID): ProviderAccountActions {
    return this.actions[provider];
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
  isProviderCredentialOpInFlight(provider: ProviderID): boolean {
    const action = this.actions[provider];
    return action.loggingIn || action.switchingID !== '' || action.removingID !== '';
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
  async loadProviderAccounts(): Promise<void> {
    // This runs unprompted at startup (eventsProvider's hydrate) and on
    // transport-gap recovery — OUTSIDE a reactive context, before the bootstrap
    // manifest has answered this page's locality. A `hasScope` read now would
    // answer the placeholder and never revisit it, settling a page that is in
    // fact admin into "loaded, nothing saved" for its whole life
    // (transport/AGENTS.md § scopes.ts). Wait for the answer first.
    if (this.backend === HOME_BACKEND) await pageGrantsResolved();
    if (this.disposed) return;
    // The provider-account surface is billing identity, which `access:admin`
    // covers. Without the grant the listing call can only be refused, and the
    // refusal would surface as an unexplained error toast; settle into
    // "loaded, nothing saved" instead of asking.
    if (!hasScope('access:admin', this.backend)) {
      this.loading = false;
      return;
    }
    return this.pendingLoad ?? this.startLoad();
  }

  /**
   * Reload after a mutation. Never joins an in-flight request, because a listing
   * fetched BEFORE the switch/remove landed cannot describe the state it
   * produced; the generation token then drops that older response.
   */
  private reloadProviderAccounts(): Promise<void> {
    return this.startLoad();
  }

  /**
   * `provider:accounts_changed` — the saved-account SET moved on some client:
   * a sign-in added a card, a switch moved which one is active, a removal took
   * one away.
   *
   * Its own channel rather than a reaction to `provider:account`, which reports
   * one card's CONTENTS and fires on every usage probe: re-listing on those
   * would spend an RPC per probe, and never re-listing (the state before this
   * existed) missed a removal outright — removing an account that was not the
   * active one published nothing at all, so the card stayed on every other
   * client's Settings screen until reload.
   *
   * A reload rather than a merge, because the listing carries per-account quota
   * snapshots and a `needsLogin` verdict only the backend can compute. Refuses
   * without the grant for the same reason `loadProviderAccounts` does: an
   * ungranted session would turn each frame into an unexplained error toast.
   */
  applyProviderAccountsChanged(): void {
    if (this.disposed) return;
    if (!hasScope('access:admin', this.backend)) return;
    void this.reloadProviderAccounts();
  }

  private startLoad(): Promise<void> {
    if (this.disposed) return Promise.resolve();
    const load = this.runLoad();
    this.pendingLoad = load;
    // Attached here, synchronously, so the latch is always cleared before any
    // awaiter's continuation runs — handlers fire in registration order.
    return load.finally(() => {
      if (this.pendingLoad === load) this.pendingLoad = null;
    });
  }

  private async runLoad(): Promise<void> {
    const generation = ++this.loadGeneration;
    try {
      const listed = (await this.call(() => ListProviderAccounts())) as ManagedProviderAccount[];
      if (generation !== this.loadGeneration) return;
      this.accounts = listed ?? [];
      this.projectListing(this.accounts);
    } catch (error) {
      if (this.disposed || generation !== this.loadGeneration) return;
      if (isPassiveConnectionFailure(error)) return;
      console.error('Failed to load provider accounts:', error);
      addToast('error', 'Failed to load provider accounts.');
    } finally {
      if (generation === this.loadGeneration) this.loading = false;
    }
  }

  // Downstream projection of a listing: last-known quotas for every account, and
  // the selected account per provider (cleared when the provider has none).
  private projectListing(listing: readonly ManagedProviderAccount[]): void {
    for (const provider of this.ACCOUNT_PROVIDERS) {
      let active: ManagedProviderAccount | null = null;
      for (const account of listing) {
        if (account.provider !== provider) continue;
        if (account.rateLimits) setProviderRateLimits(account.rateLimits, this.backend);
        if (account.active) active = account;
      }
      if (active) setProviderAccount(provider, active, active.id, active.generation, this.backend);
      else clearProviderAccount(provider, undefined, this.backend);
    }
  }

  /**
   * Make `account` the provider's active credential. Resolves true only when the
   * switch landed, so a caller that closes a surface on success keeps it open on
   * failure with the error already toasted.
   */
  async switchProviderAccount(
    provider: ProviderID,
    account: ManagedProviderAccount,
  ): Promise<boolean> {
    if (account.active || this.isProviderCredentialOpInFlight(provider)) return false;
    const action = this.actions[provider];
    const label = providerLabel(provider);
    action.switchingID = account.id;
    try {
      await this.call(() => SwitchProviderAccount(provider, account.id));
      await this.reloadProviderAccounts();
      addToast('success', `Switched ${label} account.`);
      return true;
    } catch (error) {
      if (this.disposed) return false;
      console.error(`${label} account switch failed:`, error);
      addToast(
        'error',
        `${label} account did not switch. ${userFacingError(error, 'Try again.')}`,
      );
      // A refusal is often a verdict about the account itself — a slot the
      // provider signed out. Re-read the listing so the card shows that state
      // instead of continuing to advertise a switch that cannot work. After the
      // toast: the reason for the failure must not wait on another round trip.
      await this.reloadProviderAccounts();
      return false;
    } finally {
      action.switchingID = '';
    }
  }

  /** Re-read one account's quotas from the provider and reload the listing. */
  async refreshProviderAccountUsage(
    provider: ProviderID,
    account: ManagedProviderAccount,
  ): Promise<void> {
    const action = this.actions[provider];
    if (action.refreshingID) return;
    const label = providerLabel(provider);
    action.refreshingID = account.id;
    try {
      await this.call(() => RefreshProviderAccountUsage(provider, account.id));
      // Quotas are only half of what the refresh button promises: the row also
      // shows who the account IS, and that identity comes from a probe whose
      // result is cached per process. Re-probe it here so a login changed
      // outside AO stops being described by a stale answer — both render sites
      // (Settings → Accounts and the switcher) inherit it from this one path.
      // A probe failure is not a reason to fail the usage refresh or skip the
      // reload, so it reports the way actions.ts's other callers do and the
      // flow carries on.
      try {
        await recheckProviderAccount(provider, this.backend);
      } catch (error) {
        if (this.disposed) return;
        console.error(`${label} account recheck failed:`, error);
      }
      await this.reloadProviderAccounts();
    } catch (error) {
      if (this.disposed) return;
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
  async removeProviderAccount(
    provider: ProviderID,
    account: ManagedProviderAccount,
  ): Promise<boolean> {
    if (this.isProviderCredentialOpInFlight(provider)) return false;
    const action = this.actions[provider];
    const label = providerLabel(provider);
    action.removingID = account.id;
    try {
      await this.call(() => RemoveProviderAccount(provider, account.id));
      clearProviderRateLimits(provider, account.id, this.backend);
      this.projectRemovedAccount(provider, account);
      await this.reloadProviderAccounts();
      addToast('success', `${label} account removed.`);
      return true;
    } catch (error) {
      if (this.disposed) return false;
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
  private projectRemovedAccount(provider: ProviderID, account: ManagedProviderAccount): void {
    const providerAccounts = this.getProviderAccountsFor(provider);
    const removedIndex = providerAccounts.findIndex((candidate) => candidate.id === account.id);
    // Not in the listing we hold — a caller acting on a snapshot another surface
    // has already reloaded away. There is no slot to promote into, so leave the
    // listing to the reload rather than guessing at a replacement.
    if (removedIndex < 0) return;
    const remaining = providerAccounts.filter((candidate) => candidate.id !== account.id);
    const others = this.accounts.filter(
      (candidate) => candidate.provider !== provider || candidate.id !== account.id,
    );

    if (!account.active || remaining.length === 0) {
      this.accounts = others;
      if (account.active) clearProviderAccount(provider, undefined, this.backend);
      return;
    }

    const nextIndex = removedIndex >= remaining.length ? 0 : removedIndex;
    const replacementID = remaining[nextIndex].id;
    this.accounts = others.map((candidate) =>
      candidate.provider === provider
        ? { ...candidate, active: candidate.id === replacementID }
        : candidate,
    );
    const replacement = { ...remaining[nextIndex], active: true };
    setProviderAccount(provider, replacement, replacement.id, replacement.generation, this.backend);
  }

  // ---- sign-in --------------------------------------------------------------
  //
  // A provider sign-in is a session on the backend, not one blocking call: the
  // person finishing it may be at a different screen than the one that started
  // it. This half holds the live state, drives the four RPCs, and is the only
  // consumer of the `provider:login` push.

  private seedLogins(): Record<ProviderID, ProviderLoginState> {
    const seeded = {} as Record<ProviderID, ProviderLoginState>;
    for (const provider of PROVIDER_IDS) seeded[provider] = this.idleLogin(provider);
    return seeded;
  }

  private idleLogin(provider: ProviderID): ProviderLoginState {
    return new ProviderLoginState({ provider, phase: ProviderLoginPhase.LoginPhaseIdle });
  }

  private logins = $state<Record<ProviderID, ProviderLoginState>>(this.seedLogins());

  /** One provider's sign-in, idle when nothing is running. */
  getProviderLogin(provider: ProviderID): ProviderLoginState {
    return this.logins[provider];
  }

  /**
   * True while a sign-in is running — anything that is not idle and not already
   * over. It is what both surfaces render their flow panel on, and what keeps a
   * switch or a removal from racing the credential the sign-in is about to
   * replace.
   */
  isProviderLoginActive(provider: ProviderID): boolean {
    const phase = this.logins[provider].phase;
    return (
      phase !== ProviderLoginPhase.LoginPhaseIdle
      && phase !== ProviderLoginPhase.LoginPhaseSucceeded
      && phase !== ProviderLoginPhase.LoginPhaseFailed
    );
  }

  /**
   * Which way this client can finish a sign-in. `browser` means the backend
   * opens the page on ITS machine, which is only useful when that machine is
   * also the one being read — the same question `handleExternalURL` asks before
   * choosing between the host binding and `window.open`.
   */
  private loginMethodForThisClient(): ProviderLoginMethod {
    return this.backend === HOME_BACKEND && canUseHostOpenExternalURL()
      ? ProviderLoginMethod.LoginMethodBrowser
      : ProviderLoginMethod.LoginMethodRemote;
  }

  /**
   * Apply one pushed transition. The backend is the only author of this state:
   * a client that missed a frame and one that polled see the same thing, so
   * nothing here merges.
   */
  applyProviderLogin(state: ProviderLoginState | null | undefined): void {
    const provider = state?.provider as ProviderID | undefined;
    if (!provider || !(provider in this.logins)) return;
    this.adoptProviderLogin(provider, state as ProviderLoginState);
  }

  private adoptProviderLogin(provider: ProviderID, state: ProviderLoginState): void {
    if (this.disposed) return;
    this.logins[provider] = state;
    this.actions[provider].loggingIn = this.isProviderLoginActive(provider);
    if (state.phase === ProviderLoginPhase.LoginPhaseSucceeded) {
      // The panel has nothing left to show, and the listing behind it is stale
      // by exactly the account that just landed.
      this.logins[provider] = this.idleLogin(provider);
      void this.reloadProviderAccounts();
      addToast('success', `${providerLabel(provider)} account connected.`);
    }
  }

  /**
   * Rejoin a sign-in that is already running — on mount, and after a transport
   * gap. Only a LIVE flow is adopted: a succeeded or failed state is retained
   * on the backend so the last transition is never lost, but a client arriving
   * long afterwards must not open a panel about a sign-in nobody is watching.
   */
  async hydrateProviderLogins(): Promise<void> {
    // Mount and transport-gap recovery both reach here before grants resolve;
    // wait so an admin page is not read as unprivileged and left never
    // rejoining a live sign-in (transport/AGENTS.md § scopes.ts).
    if (this.backend === HOME_BACKEND) await pageGrantsResolved();
    if (this.disposed) return;
    if (!hasScope('access:admin', this.backend)) return;
    await Promise.all(
      this.ACCOUNT_PROVIDERS.map(async (provider) => {
        try {
          const state = await this.call(() => GetProviderLoginState(provider));
          if (!state) return;
          const live = state.phase !== ProviderLoginPhase.LoginPhaseIdle
            && state.phase !== ProviderLoginPhase.LoginPhaseSucceeded
            && state.phase !== ProviderLoginPhase.LoginPhaseFailed;
          if (live) this.adoptProviderLogin(provider, state);
        } catch (error) {
          if (this.disposed) return;
          console.warn(`providerAccounts: read ${provider} sign-in state failed`, error);
        }
      }),
    );
  }

  /**
   * Begin a sign-in. Resolves once there is something to show; how it ends
   * arrives as state, because on every path but a browser on this machine the
   * ending depends on a person at another screen.
   */
  async startProviderLogin(provider: ProviderID): Promise<void> {
    if (this.isProviderCredentialOpInFlight(provider)) return;
    this.actions[provider].loggingIn = true;
    this.logins[provider] = new ProviderLoginState({
      provider,
      phase: ProviderLoginPhase.LoginPhaseStarting,
    });
    try {
      this.adoptProviderLogin(provider, await this.call(() => StartProviderLogin(provider, this.loginMethodForThisClient())));
    } catch (error) {
      if (this.disposed) return;
      console.error(`${providerLabel(provider)} sign-in failed to start:`, error);
      this.adoptProviderLogin(
        provider,
        new ProviderLoginState({
          provider,
          phase: ProviderLoginPhase.LoginPhaseFailed,
          error: userFacingError(error, `${providerLabel(provider)} sign-in could not be started.`),
        }),
      );
    }
  }

  /**
   * Hand back the code Claude's sign-in page shows after approval. A refusal
   * here is about the code itself, so it lands in the flow's own prose rather
   * than as a toast over a panel the user is reading.
   */
  async submitProviderLoginCode(
    provider: ProviderID,
    code: string,
  ): Promise<void> {
    try {
      this.adoptProviderLogin(provider, await this.call(() => SubmitProviderLoginCode(provider, code)));
    } catch (error) {
      if (this.disposed) return;
      this.logins[provider] = new ProviderLoginState({
        ...this.logins[provider],
        error: userFacingError(error, 'That code could not be submitted.'),
      });
    }
  }

  /** End a sign-in, or dismiss one that is already over. */
  async cancelProviderLogin(provider: ProviderID): Promise<void> {
    this.logins[provider] = this.idleLogin(provider);
    this.actions[provider].loggingIn = false;
    try {
      await this.call(() => CancelProviderLogin(provider));
    } catch (error) {
      if (this.disposed) return;
      console.warn(`providerAccounts: cancel ${provider} sign-in failed`, error);
    }
  }

}
