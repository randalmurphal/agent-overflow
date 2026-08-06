// Fixture factory for `ManagedProviderAccount`, shared by the providerAccounts
// store tests and the AccountSwitcher component tests. Both drive the same
// store through the same binding mocks, so they must agree on what a saved
// account looks like — a hand-copied literal in each file drifts the moment
// the wire type grows a field.

import type { ManagedProviderAccount } from '../../lib/stores/bindings';

export function account(
  overrides: Partial<ManagedProviderAccount> = {},
): ManagedProviderAccount {
  return {
    id: 'acct-1',
    provider: 'claude',
    email: '',
    displayName: '',
    subscriptionType: '',
    tokenSource: '',
    apiProvider: '',
    addedAt: 0,
    lastUsedAt: 0,
    rateLimits: null,
    active: false,
    generation: 1,
    needsLogin: false,
    ...overrides,
  } as ManagedProviderAccount;
}

/**
 * A promise plus its resolver, for tests that need two RPCs in flight at once
 * (load-racing-load, op-while-op). `vi.fn` mocks capture the resolver so the
 * test decides the settle ORDER — the whole point of a transition test.
 */
export function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}
