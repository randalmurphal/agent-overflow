// Selected account and generation belong to a computer and provider.
import type { ProviderAccountEvent } from '../types/events';

import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { onBackendDetached } from '../transport/backends';
import { compositeKey } from '../utils/compositeKey';

type Provider = ProviderAccountEvent['provider'];
type Account = ProviderAccountEvent['account'] & {
  accountId?: string;
  generation?: number;
};

let accounts: Map<string, Account> = $state(new Map());
let generations: Map<string, number> = $state(new Map());

export function setProviderAccount(
  provider: Provider,
  account: ProviderAccountEvent['account'],
  accountId?: string,
  generation?: number,
  backend: BackendKey = HOME_BACKEND,
): void {
  const key = compositeKey(backend, provider);
  // Reassign the Map rather than mutating in place so readers using
  // `$derived` over `accounts.get(...)` invalidate. Svelte 5 runes
  // track Map identity, not internal mutations.
  if (generation !== undefined && (generations.get(key) ?? 0) > generation) return;
  const next = new Map(accounts);
  next.set(key, { ...account, accountId, generation });
  accounts = next;
  if (generation !== undefined) {
    const nextGenerations = new Map(generations);
    nextGenerations.set(key, generation);
    generations = nextGenerations;
  }
}

export function getProviderAccount(provider: Provider, backend: BackendKey = HOME_BACKEND): Account | null {
  const key = compositeKey(backend, provider);
  return accounts.get(key) ?? null;
}

export function clearProviderAccount(provider: Provider, generation?: number, backend: BackendKey = HOME_BACKEND): void {
  const key = compositeKey(backend, provider);
  if (generation !== undefined && (generations.get(key) ?? 0) > generation) return;
  if (accounts.has(key)) {
    const next = new Map(accounts);
    next.delete(key);
    accounts = next;
  }
  if (generation !== undefined) {
    const nextGenerations = new Map(generations);
    nextGenerations.set(key, generation);
    generations = nextGenerations;
  }
}

// Test-only reset. Mirrors the `resetForTest` pattern in
// providerStatus.svelte.ts.
export function resetForTest(): void {
  accounts = new Map();
  generations = new Map();
}

onBackendDetached(({ backendId }) => {
  const prefix = compositeKey(backendId, '');
  accounts = new Map([...accounts].filter(([key]) => !key.startsWith(prefix)));
  generations = new Map([...generations].filter(([key]) => !key.startsWith(prefix)));
});
