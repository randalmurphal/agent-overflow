// Per-provider active authenticated-account snapshot, hydrated from the
// startup probe and updated live after account login or switching. The rate-limit
// ring popover reads `subscriptionType` to render "Plan: pro" / "Plan:
// Claude Max"; absence is rendered as a neutral fallback in the popover
// so a misconfigured environment doesn't show a blank field.
//
// The store is process-global rather than per-pane because:
//   1. The probe runs once per app boot. Threading it through every
//      pane registry would mean re-firing the probe per pane (or
//      adding a global cache anyway).
//   2. Account info is an account property, not a thread property —
//      every Claude pane on this machine sees the same plan, every
//      Codex pane sees the same plan.
//   3. The wire shape (`provider:account` event with one entry) maps
//      cleanly to a `Map<provider, account>` rather than a per-pane
//      flag.

import type { ProviderAccountEvent } from '../types/events';

type Provider = ProviderAccountEvent['provider'];
type Account = ProviderAccountEvent['account'] & { accountId?: string };

let accounts: Map<Provider, Account> = $state(new Map());

export function setProviderAccount(
  provider: Provider,
  account: ProviderAccountEvent['account'],
  accountId?: string,
): void {
  // Reassign the Map rather than mutating in place so readers using
  // `$derived` over `accounts.get(...)` invalidate. Svelte 5 runes
  // track Map identity, not internal mutations.
  const next = new Map(accounts);
  next.set(provider, { ...account, accountId });
  accounts = next;
}

export function getProviderAccount(provider: Provider): Account | null {
  return accounts.get(provider) ?? null;
}

// Test-only reset. Production updates replace the active account via
// setProviderAccount; they never clear the map. Mirrors the `resetForTest`
// pattern in providerStatus.svelte.ts.
export function resetForTest(): void {
  accounts = new Map();
}
