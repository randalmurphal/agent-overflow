// Last-known provider quotas, isolated by native account and dynamic limit ID.
// The toolbar asks for the active account's default 5h/7d allowance; Provider
// Settings renders every server-advertised bucket for every saved account.

import type { RateLimitEntry, RateLimitsSnapshot } from '../types/events';
import { asProviderID, type ProviderID } from '../types/providers';
import { getProviderAccount } from './accountInfo.svelte';

const LEGACY_ACCOUNT = '__active__';
const MAX_TIMER_DELAY = 2_147_000_000;

type LimitMap = Map<string, RateLimitEntry>;
type AccountMap = Map<string, LimitMap>;

let limitsByProvider: Map<ProviderID, AccountMap> = $state(new Map());
let expiryTimer: ReturnType<typeof setTimeout> | undefined;

function entryKey(entry: RateLimitEntry): string {
  return `${entry.limitId.trim().toLowerCase()}\u0000${entry.windowMins}`;
}

function accountKey(snapshot: RateLimitsSnapshot): string {
  return snapshot.accountId?.trim() || LEGACY_ACCOUNT;
}

export function setProviderRateLimits(snapshot: RateLimitsSnapshot): void {
  if (!snapshot?.limits?.length) return;
  const provider = asProviderID(snapshot.provider);
  if (!provider) return;

  const providerAccounts = limitsByProvider.get(provider) ?? new Map<string, LimitMap>();
  const key = accountKey(snapshot);
  const existing = providerAccounts.get(key) ?? new Map<string, RateLimitEntry>();
  const merged = new Map(existing);
  let changed = false;

  for (const entry of snapshot.limits) {
    // A provider can introduce a scoped bucket before exposing a stable
    // duration for it. Keep windowMins=0 for the Settings list; toolbar
    // lookups remain limited to concrete durations.
    if (entry.windowMins < 0 || !entry.limitId?.trim()) continue;
    const key = entryKey(entry);
    const prior = merged.get(key);
    if (prior && prior.resetsAt > entry.resetsAt) continue;
    if (
      prior
      && prior.resetsAt === entry.resetsAt
      && Number.isFinite(prior.usedPercent)
      && Number.isFinite(entry.usedPercent)
      && prior.usedPercent > entry.usedPercent
    ) {
      continue;
    }
    if (prior && rateLimitEntriesEqual(prior, entry)) continue;
    merged.set(key, { ...entry });
    changed = true;
  }
  if (!changed) return;

  const nextAccounts = new Map(providerAccounts);
  nextAccounts.set(key, merged);
  const next = new Map(limitsByProvider);
  next.set(provider, nextAccounts);
  limitsByProvider = next;
  scheduleExpiry();
}

function rateLimitEntriesEqual(a: RateLimitEntry, b: RateLimitEntry): boolean {
  return a.limitId === b.limitId
    && a.limitName === b.limitName
    && a.usedPercent === b.usedPercent
    && a.windowMins === b.windowMins
    && a.resetsAt === b.resetsAt;
}

function activeAccountKey(provider: ProviderID): string {
  return getProviderAccount(provider)?.accountId || LEGACY_ACCOUNT;
}

function limitsForAccount(provider: ProviderID, accountId?: string): LimitMap | undefined {
  const accounts = limitsByProvider.get(provider);
  if (!accounts) return undefined;
  const key = accountId || activeAccountKey(provider);
  return accounts.get(key) ?? (key !== LEGACY_ACCOUNT ? accounts.get(LEGACY_ACCOUNT) : undefined);
}

function defaultLimit(provider: ProviderID, entries: RateLimitEntry[]): RateLimitEntry | null {
  if (provider === 'codex') {
    return entries.find((entry) => entry.limitId.toLowerCase() === 'codex') ?? null;
  }
  if (provider === 'claude') {
    return entries.find((entry) => {
      const id = entry.limitId.toLowerCase();
      return id === 'session' || id === 'weekly_all'
        || id === 'five_hour' || id === 'seven_day';
    }) ?? null;
  }
  return null;
}

export function rateLimitDisplayName(
  entry: Pick<RateLimitEntry, 'limitId' | 'limitName'>,
): string {
  const name = entry.limitName.trim();
  if (name) return name;

  const id = entry.limitId.trim().toLowerCase();
  switch (id) {
    case 'codex':
    case 'weekly_all':
    case 'seven_day':
      return 'All models';
    case 'session':
    case 'five_hour':
      return 'Current session';
  }

  const scopedID = id.includes(':') ? id.slice(id.lastIndexOf(':') + 1) : id;
  const humanized = scopedID
    .replace(/[_-]+/g, ' ')
    .replace(/\b[a-z]/g, (character) => character.toUpperCase());
  return humanized || 'Usage limit';
}

export function getProviderRateLimit(
  provider: ProviderID | undefined,
  windowMins: number,
): RateLimitEntry | null {
  if (!provider || windowMins <= 0) return null;
  const candidates = [...(limitsForAccount(provider)?.values() ?? [])]
    .filter((entry) => entry.windowMins === windowMins);
  return defaultLimit(provider, candidates);
}

export interface RateLimitWindowGroup {
  primary: RateLimitEntry | null;
  limits: RateLimitEntry[];
}

// Returns every account limit for one composer duration, with the recognized
// provider-wide default first and scoped/model limits sorted by display name.
// `primary` stays null for a scoped-only update so hover details can render
// without letting that scoped quota drive the account-wide ring.
export function getProviderRateLimitsForWindow(
  provider: ProviderID | undefined,
  windowMins: number,
): RateLimitWindowGroup {
  if (!provider || windowMins <= 0) return { primary: null, limits: [] };
  const candidates = [...(limitsForAccount(provider)?.values() ?? [])]
    .filter((entry) => entry.windowMins === windowMins);
  const primary = defaultLimit(provider, candidates);
  const primaryKey = primary ? entryKey(primary) : '';
  const scoped = candidates
    .filter((entry) => entryKey(entry) !== primaryKey)
    .sort((a, b) => rateLimitDisplayName(a).localeCompare(rateLimitDisplayName(b)));
  return {
    primary,
    limits: primary ? [primary, ...scoped] : scoped,
  };
}

export function getProviderRateLimits(
  provider: ProviderID,
  accountId?: string,
): RateLimitEntry[] {
  return [...(limitsForAccount(provider, accountId)?.values() ?? [])]
    .sort((a, b) => a.windowMins - b.windowMins
      || rateLimitDisplayName(a).localeCompare(rateLimitDisplayName(b)));
}

function normalizeExpired(entry: RateLimitEntry | null): RateLimitEntry | null {
  if (!entry || entry.resetsAt <= 0 || entry.resetsAt > Date.now() / 1000) return entry;
  let resetsAt = entry.resetsAt;
  const windowSeconds = entry.windowMins * 60;
  if (windowSeconds > 0) {
    const elapsedWindows = Math.floor((Date.now() / 1000 - resetsAt) / windowSeconds) + 1;
    resetsAt += elapsedWindows * windowSeconds;
  }
  return { ...entry, usedPercent: 0, resetsAt };
}

function scheduleExpiry(): void {
  if (expiryTimer) clearTimeout(expiryTimer);
  let nextReset = Number.POSITIVE_INFINITY;
  const nowSeconds = Date.now() / 1000;
  for (const accounts of limitsByProvider.values()) {
    for (const limits of accounts.values()) {
      for (const entry of limits.values()) {
        if (entry.resetsAt > nowSeconds && entry.resetsAt < nextReset) {
          nextReset = entry.resetsAt;
        }
      }
    }
  }
  if (!Number.isFinite(nextReset)) {
    expiryTimer = undefined;
    return;
  }
  const delay = Math.min(MAX_TIMER_DELAY, Math.max(1, (nextReset - nowSeconds) * 1000));
  expiryTimer = setTimeout(() => {
    expiryTimer = undefined;
    expireElapsedLimits();
  }, delay);
}

function expireElapsedLimits(): void {
  const next = new Map<ProviderID, AccountMap>();
  for (const [provider, accounts] of limitsByProvider) {
    const nextAccounts = new Map<string, LimitMap>();
    for (const [accountId, limits] of accounts) {
      const nextLimits = new Map<string, RateLimitEntry>();
      for (const [key, entry] of limits) {
        nextLimits.set(key, normalizeExpired(entry) ?? entry);
      }
      nextAccounts.set(accountId, nextLimits);
    }
    next.set(provider, nextAccounts);
  }
  limitsByProvider = next;
  scheduleExpiry();
}

export function resetForTest(): void {
  if (expiryTimer) clearTimeout(expiryTimer);
  expiryTimer = undefined;
  limitsByProvider = new Map();
}
