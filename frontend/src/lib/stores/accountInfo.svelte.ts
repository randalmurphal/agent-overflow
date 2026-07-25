// Per-provider selected authenticated-account snapshot, hydrated from the
// startup probe and updated live after account login or switching. A running
// thread may temporarily use a different account (notably while a Codex
// app-server waits for its safe reconnect); that session-scoped identity lives
// on ThreadPane. The rate-limit ring popover reads `subscriptionType` to render
// "Plan: pro" / "Plan: Claude Max"; absence is rendered as a neutral fallback
// so a misconfigured environment doesn't show a blank field.
//
// The store is process-global rather than per-pane because:
//   1. The probe runs once per app boot. Threading it through every
//      pane registry would mean re-firing the probe per pane (or
//      adding a global cache anyway).
//   2. This store represents provider-wide selection; per-thread live account
//      attribution is kept with the pane that owns the provider session.
//   3. The wire shape (`provider:account` event with one entry) maps
//      cleanly to a `Map<provider, account>` rather than a per-pane
//      flag.

import type { ProviderAccountEvent } from '../types/events';

type Provider = ProviderAccountEvent['provider'];
type Account = ProviderAccountEvent['account'] & {
  accountId?: string;
  generation?: number;
};

let accounts: Map<Provider, Account> = $state(new Map());
let generations: Map<Provider, number> = $state(new Map());

export function setProviderAccount(
  provider: Provider,
  account: ProviderAccountEvent['account'],
  accountId?: string,
  generation?: number,
): void {
  // Reassign the Map rather than mutating in place so readers using
  // `$derived` over `accounts.get(...)` invalidate. Svelte 5 runes
  // track Map identity, not internal mutations.
  if (generation !== undefined && (generations.get(provider) ?? 0) > generation) return;
  const next = new Map(accounts);
  next.set(provider, { ...account, accountId, generation });
  accounts = next;
  if (generation !== undefined) {
    const nextGenerations = new Map(generations);
    nextGenerations.set(provider, generation);
    generations = nextGenerations;
  }
}

export function getProviderAccount(provider: Provider): Account | null {
  return accounts.get(provider) ?? null;
}

export function clearProviderAccount(provider: Provider, generation?: number): void {
  if (generation !== undefined && (generations.get(provider) ?? 0) > generation) return;
  if (accounts.has(provider)) {
    const next = new Map(accounts);
    next.delete(provider);
    accounts = next;
  }
  if (generation !== undefined) {
    const nextGenerations = new Map(generations);
    nextGenerations.set(provider, generation);
    generations = nextGenerations;
  }
}

// Test-only reset. Mirrors the `resetForTest` pattern in
// providerStatus.svelte.ts.
export function resetForTest(): void {
  accounts = new Map();
  generations = new Map();
}
