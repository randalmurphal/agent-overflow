// providerAccounts is the ONE account path shared by the provider settings
// pages and the account-switcher picker, so these tests cover the contract both surfaces
// depend on: what a listing projects into accountInfo / rateLimitsInfo, what a
// switch resolves to (the picker closes only on true), and the optimistic
// removal projection that keeps the active badge from blanking.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  getProviderAccountActions,
  getProviderAccountGroups,
  getProviderAccountsFor,
  isProviderAccountsLoading,
  isProviderCredentialOpInFlight,
  loadProviderAccounts,
  applyProviderLogin,
  cancelProviderLogin,
  getProviderLogin,
  hydrateProviderLogins,
  isProviderLoginActive,
  startProviderLogin,
  submitProviderLoginCode,
  providerAccountActionLabel,
  providerAccountName,
  refreshProviderAccountUsage,
  removeProviderAccount,
  resetForTest as resetProviderAccounts,
  switchProviderAccount,
} from './providerAccounts.svelte';
import {
  getProviderAccount,
  resetForTest as resetAccountInfo,
} from './accountInfo.svelte';
import {
  getProviderRateLimits,
  resetForTest as resetRateLimits,
} from './rateLimitsInfo.svelte';
import { getToasts, removeToast } from './toast.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { setPageGrantsFromBootstrap } from '../transport/scopes';
import { account, deferred } from '../../test/helpers/providerAccounts';
import type { ManagedProviderAccount } from './bindings';
import { ProviderLoginMethod, ProviderLoginPhase, ProviderLoginState } from './bindings';

function toastMessages(): string[] {
  return getToasts().map((toast) => toast.message);
}

beforeEach(() => {
  for (const toast of getToasts()) removeToast(toast.id);
  resetBindingMocks();
  resetProviderAccounts();
  resetAccountInfo();
  resetRateLimits();
});

afterEach(() => {
  resetProviderAccounts();
  resetAccountInfo();
  resetRateLimits();
});

describe('loadProviderAccounts', () => {
  it('projects the active account and every account’s limits, per provider', async () => {
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', displayName: 'Work', active: true }),
      account({
        id: 'claude-b',
        displayName: 'Personal',
        rateLimits: {
          provider: 'claude',
          accountId: 'claude-b',
          updatedAt: 0,
          limits: [
            { limitId: 'five_hour', limitName: '', usedPercent: 42, windowMins: 300, resetsAt: 0 },
          ],
        },
      }),
      account({ id: 'codex-a', provider: 'codex', displayName: 'Codex', active: true }),
    ]);

    await loadProviderAccounts();

    expect(isProviderAccountsLoading()).toBe(false);
    expect(getProviderAccountsFor('claude').map((a) => a.id)).toEqual(['claude-a', 'claude-b']);
    expect(getProviderAccountsFor('codex').map((a) => a.id)).toEqual(['codex-a']);
    expect(getProviderAccount('claude')?.accountId).toBe('claude-a');
    expect(getProviderAccount('codex')?.accountId).toBe('codex-a');
    // Limits are stored under the account they belong to, not the active one.
    expect(getProviderRateLimits('claude', 'claude-b')).toHaveLength(1);
    expect(getProviderRateLimits('claude', 'claude-a')).toHaveLength(0);
  });

  it('clears the selected account for a provider whose listing has none active', async () => {
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', active: true }),
    ]);
    await loadProviderAccounts();
    expect(getProviderAccount('claude')).not.toBeNull();

    setBindingMock('ListProviderAccounts', async () => [account({ id: 'claude-a' })]);
    await loadProviderAccounts();

    expect(getProviderAccount('claude')).toBeNull();
  });

  it('keeps the previous listing and toasts when the RPC fails', async () => {
    setBindingMock('ListProviderAccounts', async () => [account({ id: 'claude-a' })]);
    await loadProviderAccounts();

    setBindingMock('ListProviderAccounts', async () => {
      throw new Error('transport closed');
    });
    await loadProviderAccounts();

    expect(getProviderAccountsFor('claude')).toHaveLength(1);
    expect(toastMessages()).toContain('Failed to load provider accounts.');
  });

  it('shares one RPC between concurrent callers', async () => {
    // Settings mounts an accounts block per provider and the picker is a third
    // caller; each listing costs a per-account credential check backend-side.
    const listMock = setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', active: true }),
    ]);

    const first = loadProviderAccounts();
    const second = loadProviderAccounts();
    await Promise.all([first, second]);

    expect(listMock).toHaveBeenCalledOnce();
    expect(getProviderAccountsFor('claude')).toHaveLength(1);

    // The latch releases: a load issued after the first settles is a new RPC.
    await loadProviderAccounts();
    expect(listMock).toHaveBeenCalledTimes(2);
  });

  it('drops a stale listing that lands after a newer one', async () => {
    // The two loads overlap because a post-switch reload never joins a
    // request issued BEFORE the switch landed — a listing fetched then cannot
    // describe the state the switch produced.
    const stale = deferred<ManagedProviderAccount[]>();
    const fresh = deferred<ManagedProviderAccount[]>();
    let call = 0;
    setBindingMock('ListProviderAccounts', () => (call++ === 0 ? stale.promise : fresh.promise));
    setBindingMock('SwitchProviderAccount', async () => undefined);

    const staleLoad = loadProviderAccounts();
    const switched = switchProviderAccount('claude', account({ id: 'claude-b' }));

    fresh.resolve([account({ id: 'claude-b', active: true })]);
    await switched;
    expect(getProviderAccountsFor('claude').map((a) => a.id)).toEqual(['claude-b']);
    expect(isProviderAccountsLoading()).toBe(false);

    stale.resolve([account({ id: 'claude-a', active: true })]);
    await staleLoad;

    expect(getProviderAccountsFor('claude').map((a) => a.id)).toEqual(['claude-b']);
    expect(getProviderAccount('claude')?.accountId).toBe('claude-b');
    expect(isProviderAccountsLoading()).toBe(false);
  });

  it('asks for nothing in a view-only session, and stops reporting as loading', async () => {
    // The RPC needs `access:admin`, so it can only be refused — and this load runs
    // unprompted at startup, where the refusal would be an unexplained toast.
    const listMock = setBindingMock('ListProviderAccounts', async () => [account()]);
    setPageGrantsFromBootstrap(true);
    try {
      await loadProviderAccounts();
    } finally {
      setPageGrantsFromBootstrap(false);
    }

    expect(listMock).not.toHaveBeenCalled();
    expect(isProviderAccountsLoading()).toBe(false);
    expect(getProviderAccountsFor('claude')).toHaveLength(0);
    expect(toastMessages()).toHaveLength(0);
  });

  it('groups only account-capable providers, in settings order', async () => {
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'codex-a', provider: 'codex' }),
      account({ id: 'claude-a' }),
    ]);
    await loadProviderAccounts();

    expect(getProviderAccountGroups().map((group) => group.provider)).toEqual([
      'claude',
      'codex',
    ]);
  });
});

describe('switchProviderAccount', () => {
  it('switches, reloads, and resolves true', async () => {
    let active = 'claude-a';
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', active: active === 'claude-a' }),
      account({ id: 'claude-b', active: active === 'claude-b' }),
    ]);
    const switchMock = setBindingMock('SwitchProviderAccount', async (_p, id: unknown) => {
      active = String(id);
    });
    await loadProviderAccounts();

    const target = getProviderAccountsFor('claude')[1];
    await expect(switchProviderAccount('claude', target)).resolves.toBe(true);

    expect(switchMock).toHaveBeenCalledWith('claude', 'claude-b');
    expect(getProviderAccount('claude')?.accountId).toBe('claude-b');
    expect(toastMessages()).toContain('Switched Claude account.');
    expect(getProviderAccountActions('claude').switchingID).toBe('');
  });

  it('resolves false and surfaces the error when the switch fails', async () => {
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', active: true }),
      account({ id: 'claude-b' }),
    ]);
    setBindingMock('SwitchProviderAccount', async () => {
      throw new Error('credential slot locked');
    });
    await loadProviderAccounts();

    const target = getProviderAccountsFor('claude')[1];
    await expect(switchProviderAccount('claude', target)).resolves.toBe(false);

    expect(getProviderAccount('claude')?.accountId).toBe('claude-a');
    expect(toastMessages()).toContain(
      'Claude account did not switch. Credential slot locked.',
    );
    expect(getProviderAccountActions('claude').switchingID).toBe('');
  });

  it('re-reads the listing when a switch is refused, so a dead slot stops looking switchable', async () => {
    // The backend refuses a switch into a slot the provider signed out. That
    // refusal is a verdict about the account, so the card has to stop offering
    // the switch instead of waiting for the next unrelated reload.
    let listed: ManagedProviderAccount[] = [
      account({ id: 'claude-a', active: true }),
      account({ id: 'claude-b' }),
    ];
    setBindingMock('ListProviderAccounts', async () => listed);
    setBindingMock('SwitchProviderAccount', async () => {
      listed = [
        account({ id: 'claude-a', active: true }),
        account({ id: 'claude-b', needsLogin: true }),
      ];
      throw new Error('sign in to this account again to reconnect it');
    });
    await loadProviderAccounts();

    const target = getProviderAccountsFor('claude')[1];
    await expect(switchProviderAccount('claude', target)).resolves.toBe(false);

    expect(getProviderAccountsFor('claude')[1].needsLogin).toBe(true);
  });

  it('refuses a second switch while one is in flight', async () => {
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', active: true }),
      account({ id: 'claude-b' }),
      account({ id: 'claude-c' }),
    ]);
    const gate = deferred<void>();
    const switchMock = setBindingMock('SwitchProviderAccount', () => gate.promise);
    await loadProviderAccounts();
    const [, second, third] = getProviderAccountsFor('claude');

    const first = switchProviderAccount('claude', second);
    await expect(switchProviderAccount('claude', third)).resolves.toBe(false);
    expect(switchMock).toHaveBeenCalledOnce();

    gate.resolve();
    await first;
    expect(isProviderCredentialOpInFlight('claude')).toBe(false);
  });

  it('refuses a removal issued while a switch is in flight', async () => {
    // The guard is the API's, not the settings component's `disabled` prop:
    // the picker has no remove affordance at all and must still be safe.
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', active: true }),
      account({ id: 'claude-b' }),
    ]);
    const gate = deferred<void>();
    setBindingMock('SwitchProviderAccount', () => gate.promise);
    const removeMock = setBindingMock('RemoveProviderAccount', async () => undefined);
    await loadProviderAccounts();
    const [first, second] = getProviderAccountsFor('claude');

    const switching = switchProviderAccount('claude', second);
    await expect(removeProviderAccount('claude', first)).resolves.toBe(false);
    expect(removeMock).not.toHaveBeenCalled();

    gate.resolve();
    await switching;
    // Off → on → off: the guard released, so the removal now runs.
    await expect(removeProviderAccount('claude', first)).resolves.toBe(true);
    expect(removeMock).toHaveBeenCalledOnce();
  });

  it('refuses to switch to the already-active account', async () => {
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', active: true }),
    ]);
    const switchMock = setBindingMock('SwitchProviderAccount', async () => undefined);
    await loadProviderAccounts();

    await expect(
      switchProviderAccount('claude', getProviderAccountsFor('claude')[0]),
    ).resolves.toBe(false);
    expect(switchMock).not.toHaveBeenCalled();
  });
});

// A sign-in is a session: Start returns as soon as there is a link to show,
// and how it ended arrives later as state. These cover the three things the
// two surfaces read off it — the live link, the code round trip, and the
// terminal transition that reloads the listing.
describe('startProviderLogin', () => {
  it('holds the link the backend answered with and keeps the flow active', async () => {
    setBindingMock('ListProviderAccounts', async () => []);
    const startMock = setBindingMock('StartProviderLogin', async () =>
      new ProviderLoginState({
        provider: 'claude',
        phase: ProviderLoginPhase.LoginPhaseAwaitingCode,
        method: ProviderLoginMethod.LoginMethodRemote,
        authorizeUrl: 'https://claude.ai/oauth/authorize?state=abc',
      }),
    );
    await loadProviderAccounts();

    await startProviderLogin('claude');

    expect(startMock).toHaveBeenCalledWith('claude', expect.any(String));
    expect(getProviderLogin('claude').authorizeUrl).toBe(
      'https://claude.ai/oauth/authorize?state=abc',
    );
    expect(isProviderLoginActive('claude')).toBe(true);
    // A live sign-in owns the provider's credential slot, so a switch or a
    // removal must not start beside it.
    expect(isProviderCredentialOpInFlight('claude')).toBe(true);
  });

  it('reports a start that never produced a link, and leaves the button usable', async () => {
    setBindingMock('ListProviderAccounts', async () => []);
    setBindingMock('StartProviderLogin', async () => {
      throw new Error('claude binary not found');
    });
    await loadProviderAccounts();

    await startProviderLogin('claude');

    expect(getProviderLogin('claude').phase).toBe(ProviderLoginPhase.LoginPhaseFailed);
    expect(getProviderLogin('claude').error).toBe('Claude binary not found.');
    expect(isProviderCredentialOpInFlight('claude')).toBe(false);
  });
});

describe('applyProviderLogin', () => {
  it('reloads the listing and reports success once the sign-in lands', async () => {
    let saved: ManagedProviderAccount[] = [];
    setBindingMock('ListProviderAccounts', async () => saved);
    await loadProviderAccounts();
    saved = [account({ id: 'claude-a', displayName: 'Work', active: true })];

    applyProviderLogin(
      new ProviderLoginState({
        provider: 'claude',
        phase: ProviderLoginPhase.LoginPhaseSucceeded,
      }),
    );
    await Promise.resolve();
    await Promise.resolve();

    expect(getProviderAccountsFor('claude').map((a) => a.id)).toEqual(['claude-a']);
    expect(getProviderAccount('claude')?.accountId).toBe('claude-a');
    expect(toastMessages()).toContain('Claude account connected.');
    // Succeeding closes the panel: there is nothing left for it to show.
    expect(getProviderLogin('claude').phase).toBe(ProviderLoginPhase.LoginPhaseIdle);
    expect(isProviderCredentialOpInFlight('claude')).toBe(false);
  });

  it('keeps a failed sign-in on screen with its reason', async () => {
    setBindingMock('ListProviderAccounts', async () => []);
    await loadProviderAccounts();

    applyProviderLogin(
      new ProviderLoginState({
        provider: 'claude',
        phase: ProviderLoginPhase.LoginPhaseFailed,
        error: 'Request failed with status code 400',
      }),
    );

    expect(getProviderLogin('claude').error).toBe('Request failed with status code 400');
    expect(isProviderLoginActive('claude')).toBe(false);
    expect(isProviderCredentialOpInFlight('claude')).toBe(false);
  });

  it('ignores a frame for a provider it does not track', async () => {
    setBindingMock('ListProviderAccounts', async () => []);
    await loadProviderAccounts();

    applyProviderLogin({ provider: 'nope', phase: 'starting' } as never);

    expect(getProviderLogin('claude').phase).toBe(ProviderLoginPhase.LoginPhaseIdle);
  });
});

describe('submitProviderLoginCode', () => {
  it('adopts the state the submission answered with', async () => {
    const submitMock = setBindingMock('SubmitProviderLoginCode', async () =>
      new ProviderLoginState({
        provider: 'claude',
        phase: ProviderLoginPhase.LoginPhaseVerifying,
      }),
    );

    await submitProviderLoginCode('claude', 'code#state');

    expect(submitMock).toHaveBeenCalledWith('claude', 'code#state');
    expect(getProviderLogin('claude').phase).toBe(ProviderLoginPhase.LoginPhaseVerifying);
  });

  it('puts a refused code in the flow rather than in a toast over it', async () => {
    applyProviderLogin(
      new ProviderLoginState({
        provider: 'claude',
        phase: ProviderLoginPhase.LoginPhaseAwaitingCode,
        authorizeUrl: 'https://claude.ai/oauth/authorize',
      }),
    );
    setBindingMock('SubmitProviderLoginCode', async () => {
      throw new Error('that is not the whole code');
    });

    await submitProviderLoginCode('claude', 'half');

    expect(getProviderLogin('claude').error).toBe('That is not the whole code.');
    // The link the user is holding survives a refusal about the code.
    expect(getProviderLogin('claude').authorizeUrl).toBe('https://claude.ai/oauth/authorize');
    expect(toastMessages()).toEqual([]);
  });
});

describe('hydrateProviderLogins', () => {
  it('rejoins a sign-in that is still running', async () => {
    setPageGrantsFromBootstrap(false);
    setBindingMock('GetProviderLoginState', async (provider: never) =>
      new ProviderLoginState({
        provider: provider as unknown as string,
        phase:
          provider === ('codex' as never)
            ? ProviderLoginPhase.LoginPhaseAwaitingCode
            : ProviderLoginPhase.LoginPhaseIdle,
        userCode: 'ABCD-EFGH',
      }),
    );

    await hydrateProviderLogins();

    expect(getProviderLogin('codex').userCode).toBe('ABCD-EFGH');
    expect(isProviderLoginActive('claude')).toBe(false);
  });

  // The backend retains a terminal state so the last transition is never
  // lost. A client arriving long afterwards must not open a panel about a
  // sign-in nobody is watching.
  it('does not adopt a sign-in that already ended', async () => {
    setPageGrantsFromBootstrap(false);
    setBindingMock('GetProviderLoginState', async (provider: never) =>
      new ProviderLoginState({
        provider: provider as unknown as string,
        phase: ProviderLoginPhase.LoginPhaseFailed,
        error: 'an hour ago',
      }),
    );

    await hydrateProviderLogins();

    expect(getProviderLogin('claude').phase).toBe(ProviderLoginPhase.LoginPhaseIdle);
  });
});

describe('cancelProviderLogin', () => {
  it('clears the panel before the backend answers, then tells the backend', async () => {
    const cancelMock = setBindingMock('CancelProviderLogin', async () => undefined);
    applyProviderLogin(
      new ProviderLoginState({
        provider: 'codex',
        phase: ProviderLoginPhase.LoginPhaseAwaitingCode,
        userCode: 'ABCD-EFGH',
      }),
    );

    await cancelProviderLogin('codex');

    expect(cancelMock).toHaveBeenCalledWith('codex');
    expect(getProviderLogin('codex').phase).toBe(ProviderLoginPhase.LoginPhaseIdle);
    expect(isProviderCredentialOpInFlight('codex')).toBe(false);
  });
});

describe('refreshProviderAccountUsage', () => {
  // The button refreshes what the row SHOWS, and identity is half of that.
  // The probe behind it is cached per process, so without this re-probe a
  // login changed outside AO keeps being described by the stale answer.
  it('re-probes the account identity between the usage refresh and the reload', async () => {
    const calls: string[] = [];
    setBindingMock('ListProviderAccounts', async () => {
      calls.push('list');
      return [account({ id: 'claude-a' })];
    });
    setBindingMock('RefreshProviderAccountUsage', async () => {
      calls.push('usage');
    });
    setBindingMock('RecheckClaudeAccount', async () => {
      calls.push('recheck');
      return {};
    });
    await loadProviderAccounts();
    calls.length = 0;

    await refreshProviderAccountUsage('claude', getProviderAccountsFor('claude')[0]);

    expect(calls).toEqual(['usage', 'recheck', 'list']);
  });

  it('rechecks the account of the provider it was asked about', async () => {
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'codex-a', provider: 'codex' }),
    ]);
    setBindingMock('RefreshProviderAccountUsage', async () => {});
    const recheckClaude = setBindingMock('RecheckClaudeAccount', async () => ({}));
    const recheckCodex = setBindingMock('RecheckCodexAccount', async () => ({}));
    await loadProviderAccounts();

    await refreshProviderAccountUsage('codex', getProviderAccountsFor('codex')[0]);

    expect(recheckCodex).toHaveBeenCalledOnce();
    expect(recheckClaude).not.toHaveBeenCalled();
  });

  // A failed identity probe is not a failed refresh: the quotas already
  // landed, so the listing still has to be re-read and no error toast is
  // owed for the half that succeeded.
  it('completes the refresh when the recheck fails', async () => {
    setBindingMock('ListProviderAccounts', async () => [account({ id: 'claude-a' })]);
    const refresh = setBindingMock('RefreshProviderAccountUsage', async () => {});
    setBindingMock('RecheckClaudeAccount', async () => {
      throw new Error('probe exploded');
    });
    await loadProviderAccounts();

    await refreshProviderAccountUsage('claude', getProviderAccountsFor('claude')[0]);

    expect(refresh).toHaveBeenCalledOnce();
    expect(toastMessages()).toEqual([]);
    expect(getProviderAccountActions('claude').refreshingID).toBe('');
  });

  it('reports a refresh failure rather than swallowing it', async () => {
    setBindingMock('ListProviderAccounts', async () => [account({ id: 'claude-a' })]);
    setBindingMock('RefreshProviderAccountUsage', async () => {
      throw new Error('usage endpoint 503');
    });
    await loadProviderAccounts();

    await refreshProviderAccountUsage('claude', getProviderAccountsFor('claude')[0]);

    expect(toastMessages()).toContain('Usage endpoint 503.');
    expect(getProviderAccountActions('claude').refreshingID).toBe('');
  });
});

describe('removeProviderAccount', () => {
  it('promotes the account that takes the removed active one’s slot', async () => {
    // The reload is deliberately stale (it still lists the removed account) so
    // the assertion can only pass via the optimistic projection.
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', active: true }),
      account({ id: 'claude-b' }),
      account({ id: 'claude-c' }),
    ]);
    setBindingMock('RemoveProviderAccount', async () => undefined);
    await loadProviderAccounts();
    const removed = getProviderAccountsFor('claude')[0];

    // Freeze the listing so the post-remove reload can't answer for us.
    setBindingMock('ListProviderAccounts', async () => {
      throw new Error('offline');
    });
    await expect(removeProviderAccount('claude', removed)).resolves.toBe(true);

    expect(getProviderAccountsFor('claude').map((a) => a.id)).toEqual(['claude-b', 'claude-c']);
    expect(getProviderAccount('claude')?.accountId).toBe('claude-b');
  });

  it('clears the selection when the last account of a provider goes', async () => {
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', active: true }),
      account({ id: 'codex-a', provider: 'codex', active: true }),
    ]);
    setBindingMock('RemoveProviderAccount', async () => undefined);
    await loadProviderAccounts();
    const removed = getProviderAccountsFor('claude')[0];

    setBindingMock('ListProviderAccounts', async () => {
      throw new Error('offline');
    });
    await removeProviderAccount('claude', removed);

    expect(getProviderAccountsFor('claude')).toHaveLength(0);
    expect(getProviderAccount('claude')).toBeNull();
    // The other provider's slice is untouched.
    expect(getProviderAccountsFor('codex').map((a) => a.id)).toEqual(['codex-a']);
    expect(getProviderAccount('codex')?.accountId).toBe('codex-a');
  });

  it('promotes nothing when the removed account is not in the listing', async () => {
    // Reachable through a pendingRemoval captured before another surface
    // reloaded the account away; `Math.max(0, -1)` used to promote whatever
    // sat at index 0 instead.
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', active: true }),
      account({ id: 'claude-b' }),
    ]);
    setBindingMock('RemoveProviderAccount', async () => undefined);
    await loadProviderAccounts();

    setBindingMock('ListProviderAccounts', async () => {
      throw new Error('offline');
    });
    await removeProviderAccount('claude', account({ id: 'claude-gone', active: true }));

    expect(getProviderAccountsFor('claude').map((a) => a.id)).toEqual(['claude-a', 'claude-b']);
    expect(getProviderAccount('claude')?.accountId).toBe('claude-a');
  });

  it('leaves the listing alone and toasts when removal fails', async () => {
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', active: true }),
      account({ id: 'claude-b' }),
    ]);
    setBindingMock('RemoveProviderAccount', async () => {
      throw new Error('file busy');
    });
    await loadProviderAccounts();

    await expect(
      removeProviderAccount('claude', getProviderAccountsFor('claude')[1]),
    ).resolves.toBe(false);

    expect(getProviderAccountsFor('claude')).toHaveLength(2);
    expect(toastMessages()).toContain('Claude account was not removed. File busy.');
    expect(getProviderAccountActions('claude').removingID).toBe('');
  });
});

describe('providerAccountName', () => {
  it('falls back through display name, email, plan, then a generic label', () => {
    expect(providerAccountName(account({ displayName: 'Work', email: 'a@b.c' }))).toBe('Work');
    expect(providerAccountName(account({ email: 'a@b.c' }))).toBe('a@b.c');
    expect(providerAccountName(account({ subscriptionType: 'max' }))).toBe('max');
    expect(providerAccountName(account())).toBe('Saved account');
  });
});

describe('providerAccountActionLabel', () => {
  it('names the three row states the same way on both surfaces', () => {
    expect(providerAccountActionLabel(account({ displayName: 'Work' }))).toBe('Switch to Work');
    expect(providerAccountActionLabel(account({ displayName: 'Work', active: true }))).toBe(
      'Work is active',
    );
    // needsLogin wins over active: the credential is gone either way.
    expect(
      providerAccountActionLabel(account({ displayName: 'Work', active: true, needsLogin: true })),
    ).toBe('Sign in again to Work');
  });
});
