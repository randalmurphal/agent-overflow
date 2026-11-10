// Last-known provider quotas, isolated by native account and dynamic limit ID.
// The toolbar asks for the active account's 5h/7d limits by duration; Provider
// Settings renders every server-advertised bucket for every saved account.

import type { RateLimitEntry, RateLimitsSnapshot } from '../types/events';
import { asProviderID, type ProviderID } from '../types/providers';
import { getProviderAccount } from './accountInfo.svelte';
import { onBackendDetached } from '../transport/backends';
import { compositeKey } from '../utils/compositeKey';

import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';

const LEGACY_ACCOUNT = '__active__';
const MAX_TIMER_DELAY = 2_147_000_000;
const RESET_JITTER_TOLERANCE_SECONDS = 60;

type LimitMap = Map<string, RateLimitEntry>;
type AccountMap = Map<string, LimitMap>;

let limitsByProvider: Map<string, AccountMap> = $state(new Map());
let expiryTimer: ReturnType<typeof setTimeout> | undefined;
// Bumped when a stored reset boundary passes so getter-driven $deriveds
// re-read. The stored entries themselves are never rewritten at expiry —
// see entriesForAccount.
let expiryGeneration = $state(0);

function entryKey(entry: RateLimitEntry): string {
  return compositeKey(entry.limitId.trim().toLowerCase(), entry.windowMins);
}

function accountKey(snapshot: RateLimitsSnapshot): string {
  return snapshot.accountId?.trim() || LEGACY_ACCOUNT;
}

export function setProviderRateLimits(snapshot: RateLimitsSnapshot, backend: BackendKey = HOME_BACKEND): void {
  if (!snapshot?.limits?.length) return;
  const provider = asProviderID(snapshot.provider);
  if (!provider) return;

  const providerAccounts = limitsByProvider.get(compositeKey(backend, provider)) ?? new Map<string, LimitMap>();
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
    let candidate = entry;
    if (prior) {
      const resetDelta = entry.resetsAt - prior.resetsAt;
      // A partial reading from an older window is stale. A complete reading
      // is the server's whole answer and replaces the stored entry even when
      // the stored boundary is later: a wrong later boundary would otherwise
      // reject every true reading until it passed.
      if (resetDelta < -RESET_JITTER_TOLERANCE_SECONDS && !snapshot.complete) continue;
      // Same window: the NEWEST reading wins, even when its used-percent is
      // lower. Every event carries the server's current answer, and the
      // server legitimately lowers same-window utilization — a limit
      // increase, or an outright server-side usage reset under an unchanged
      // boundary (2026-09-01: Anthropic manually reset weekly usage
      // mid-window; the backend accepted the drop while this store's copy of
      // the keep-the-max rule froze the meters at the stale figure until the
      // next app restart). MergeSnapshot in internal/providerlifecycleapp
      // is this rule's backend twin — change both together.
      if (Math.abs(resetDelta) <= RESET_JITTER_TOLERANCE_SECONDS) {
        // Claude's endpoint can move an otherwise identical reset boundary by
        // a few seconds. Stabilize it so the UI accepts updates without
        // churning on every periodic probe.
        candidate = { ...entry, resetsAt: prior.resetsAt };
      }
    }
    if (prior && rateLimitEntriesEqual(prior, candidate)) continue;
    merged.set(key, { ...candidate });
    changed = true;
  }
  if (snapshot.complete) {
    // The whole answer for this account, so a stored limit it omits is gone.
    // A provider drops a bucket once it has no usage, which is what a
    // mid-window reset produces, and merging alone kept the pre-reset
    // percentage for the rest of the window (2026-09-01: a Fable weekly row
    // stuck at 90% while session and all-models read 0%). MergeSnapshot in
    // internal/providerlifecycleapp is this rule's twin; change both.
    const present = new Set<string>();
    for (const entry of snapshot.limits) {
      if (!entry.limitId?.trim()) continue;
      present.add(entryKey(entry));
    }
    for (const key of merged.keys()) {
      if (present.has(key)) continue;
      merged.delete(key);
      changed = true;
    }
  }
  if (!changed) return;

  const nextAccounts = new Map(providerAccounts);
  nextAccounts.set(key, merged);
  const next = new Map(limitsByProvider);
  next.set(compositeKey(backend, provider), nextAccounts);
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

function activeAccountKey(provider: ProviderID, backend: BackendKey): string {
  return getProviderAccount(provider, backend)?.accountId || LEGACY_ACCOUNT;
}

function limitsForAccount(provider: ProviderID, accountId: string | undefined, backend: BackendKey): LimitMap | undefined {
  const accounts = limitsByProvider.get(compositeKey(backend, provider));
  if (!accounts) return undefined;
  const explicitAccount = accountId?.trim();
  if (explicitAccount) return accounts.get(explicitAccount);
  const key = activeAccountKey(provider, backend);
  return accounts.get(key) ?? (key !== LEGACY_ACCOUNT ? accounts.get(LEGACY_ACCOUNT) : undefined);
}

// Every getter reads through this accessor: it projects an entry whose reset
// boundary has passed to 0% used with no reset time, and re-runs consumers
// when a boundary passes (expiryGeneration). The next window starts at the
// account's first request after the reset, so its boundary is unknown until a
// reading reports it (Claude anchors both 5h and weekly windows on first use;
// a fabricated `old + window` boundary sat 10 minutes past the real one on
// 2026-08-07). The projection is read-time only: the stored entry keeps the
// server-reported boundary for the stale-reading guard in
// setProviderRateLimits.
function entriesForAccount(provider: ProviderID, accountId: string | undefined, backend: BackendKey): RateLimitEntry[] {
  void expiryGeneration;
  const nowSeconds = Date.now() / 1000;
  return [...(limitsForAccount(provider, accountId, backend)?.values() ?? [])]
    .map((entry) => projectExpired(entry, nowSeconds));
}

function projectExpired(entry: RateLimitEntry, nowSeconds: number): RateLimitEntry {
  if (entry.resetsAt <= 0 || entry.resetsAt > nowSeconds) return entry;
  return { ...entry, usedPercent: 0, resetsAt: 0 };
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
  accountId?: string,
  backend: BackendKey = HOME_BACKEND,
): RateLimitEntry | null {
  if (!provider || windowMins <= 0) return null;
  const candidates = entriesForAccount(provider, accountId, backend)
    .filter((entry) => entry.windowMins === windowMins);
  return defaultLimit(provider, candidates);
}

export interface RateLimitWindowGroup {
  primary: RateLimitEntry | null;
  // The most-used scoped/model limit of this duration, or null when the
  // account reports none. The composer draws it as the meter's inner ring.
  secondary: RateLimitEntry | null;
  limits: RateLimitEntry[];
}

function comparableUsedPercent(entry: RateLimitEntry): number {
  return Number.isFinite(entry.usedPercent) ? entry.usedPercent : 0;
}

// Returns every account limit for one composer duration, with the recognized
// provider-wide default first and scoped/model limits sorted by display name.
// `primary` stays null for a scoped-only update so hover details can render
// without letting that scoped quota drive the account-wide ring.
export function getProviderRateLimitsForWindow(
  provider: ProviderID | undefined,
  windowMins: number,
  accountId?: string,
  backend: BackendKey = HOME_BACKEND,
): RateLimitWindowGroup {
  if (!provider || windowMins <= 0) return { primary: null, secondary: null, limits: [] };
  const candidates = entriesForAccount(provider, accountId, backend)
    .filter((entry) => entry.windowMins === windowMins);
  const primary = defaultLimit(provider, candidates);
  const primaryKey = primary ? entryKey(primary) : '';
  const scoped = candidates
    .filter((entry) => entryKey(entry) !== primaryKey)
    .sort((a, b) => rateLimitDisplayName(a).localeCompare(rateLimitDisplayName(b)));
  // Ties keep display-name order, so equal readings never swap rings.
  let secondary: RateLimitEntry | null = null;
  for (const entry of scoped) {
    if (!secondary || comparableUsedPercent(entry) > comparableUsedPercent(secondary)) {
      secondary = entry;
    }
  }
  return {
    primary,
    secondary,
    limits: primary ? [primary, ...scoped] : scoped,
  };
}

const WEEKLY_WINDOW_MINS = 10080;

// The durations the composer shows a meter for, shortest first: every
// duration the account has a provider-wide default limit of, which is the
// limit a meter's main ring draws. An account with none (no reading yet, or
// scoped limits only) gets the weekly meter alone, so the composer always
// has one place to show "awaiting first update" and the hover details.
export function getComposerRateLimitWindows(
  provider: ProviderID | undefined,
  accountId?: string,
  backend: BackendKey = HOME_BACKEND,
): number[] {
  if (!provider) return [WEEKLY_WINDOW_MINS];
  const byWindow = new Map<number, RateLimitEntry[]>();
  for (const entry of entriesForAccount(provider, accountId, backend)) {
    if (entry.windowMins <= 0) continue;
    const group = byWindow.get(entry.windowMins);
    if (group) group.push(entry);
    else byWindow.set(entry.windowMins, [entry]);
  }
  const windows = [...byWindow]
    .filter(([, entries]) => defaultLimit(provider, entries) !== null)
    .map(([windowMins]) => windowMins)
    .sort((a, b) => a - b);
  return windows.length > 0 ? windows : [WEEKLY_WINDOW_MINS];
}

// Short duration label shared by the composer meters and the Settings list.
export function rateLimitWindowLabel(windowMins: number): string {
  if (windowMins === 300) return '5h';
  if (windowMins === WEEKLY_WINDOW_MINS) return '7d';
  return `${Math.max(1, Math.round(windowMins / 60))}h`;
}

export function getProviderRateLimits(
  provider: ProviderID,
  accountId?: string,
  backend: BackendKey = HOME_BACKEND,
): RateLimitEntry[] {
  return entriesForAccount(provider, accountId, backend)
    .sort((a, b) => a.windowMins - b.windowMins
      || rateLimitDisplayName(a).localeCompare(rateLimitDisplayName(b)));
}

export function clearProviderRateLimits(provider: ProviderID, accountId: string, backend: BackendKey = HOME_BACKEND): void {
  const providerAccounts = limitsByProvider.get(compositeKey(backend, provider));
  if (!providerAccounts?.has(accountId)) return;
  const nextAccounts = new Map(providerAccounts);
  nextAccounts.delete(accountId);
  const next = new Map(limitsByProvider);
  if (nextAccounts.size > 0) {
    next.set(compositeKey(backend, provider), nextAccounts);
  } else {
    next.delete(compositeKey(backend, provider));
  }
  limitsByProvider = next;
  scheduleExpiry();
}

// Arms one timer for the nearest future reset boundary. Firing only bumps
// expiryGeneration — the read-time projection in entriesForAccount is what
// flips the display to 0% — then re-arms for the next boundary.
function scheduleExpiry(): void {
  if (expiryTimer) clearTimeout(expiryTimer);
  expiryTimer = undefined;
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
  if (!Number.isFinite(nextReset)) return;
  const delay = Math.min(MAX_TIMER_DELAY, Math.max(1, (nextReset - nowSeconds) * 1000));
  expiryTimer = setTimeout(() => {
    expiryTimer = undefined;
    expiryGeneration += 1;
    scheduleExpiry();
  }, delay);
}

export function resetForTest(): void {
  if (expiryTimer) clearTimeout(expiryTimer);
  expiryTimer = undefined;
  limitsByProvider = new Map();
}

onBackendDetached(({ backendId }) => {
  const prefix = compositeKey(backendId, '');
  limitsByProvider = new Map([...limitsByProvider].filter(([key]) => !key.startsWith(prefix)));
  scheduleExpiry();
});
