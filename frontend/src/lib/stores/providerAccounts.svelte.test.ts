// providerAccounts is the ONE account path shared by Settings → Providers and
// the account-switcher picker, so these tests cover the contract both surfaces
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
  loginProviderAccount,
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
import { setViewOnlySessionFromBootstrap } from '../transport/runMode';
import { account, deferred } from '../../test/helpers/providerAccounts';
import type { ManagedProviderAccount } from './bindings';

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
    // The RPC is LocalOnly, so it can only be refused — and this load runs
    // unprompted at startup, where the refusal would be an unexplained toast.
    const listMock = setBindingMock('ListProviderAccounts', async () => [account()]);
    setViewOnlySessionFromBootstrap(true);
    try {
      await loadProviderAccounts();
    } finally {
      setViewOnlySessionFromBootstrap(false);
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

describe('loginProviderAccount', () => {
  it('reloads and reports success once a login completes', async () => {
    let saved: ManagedProviderAccount[] = [];
    setBindingMock('ListProviderAccounts', async () => saved);
    const loginMock = setBindingMock('LoginProviderAccount', async () => {
      saved = [account({ id: 'claude-a', displayName: 'Work', active: true })];
    });
    await loadProviderAccounts();

    await expect(loginProviderAccount('claude')).resolves.toBe(true);

    expect(loginMock).toHaveBeenCalledWith('claude');
    expect(getProviderAccountsFor('claude').map((a) => a.id)).toEqual(['claude-a']);
    expect(getProviderAccount('claude')?.accountId).toBe('claude-a');
    expect(toastMessages()).toContain('Claude account connected.');
    expect(isProviderCredentialOpInFlight('claude')).toBe(false);
  });

  it('resolves false and surfaces the error when the login flow fails', async () => {
    setBindingMock('ListProviderAccounts', async () => []);
    setBindingMock('LoginProviderAccount', async () => {
      throw new Error('browser never returned');
    });
    await loadProviderAccounts();

    await expect(loginProviderAccount('claude')).resolves.toBe(false);

    expect(toastMessages()).toContain('Browser never returned.');
    // The flag clears on the failure path too, so the button comes back.
    expect(isProviderCredentialOpInFlight('claude')).toBe(false);
    await expect(loginProviderAccount('claude')).resolves.toBe(false);
  });
});

describe('refreshProviderAccountUsage', () => {
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
