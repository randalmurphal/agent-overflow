// The localStorage-backed SizePriorsStorageAdapter (utils/virtual/priors.ts):
// makes per-thread row-size priors survive an app restart, not just a
// same-session thread switch. priors.ts stays DOM-free by design, so this
// module owns every localStorage read/write and plugs itself into
// priors.ts's storage-adapter seam via `setSizePriorsStorageAdapter`.
//
// Storage shape: one JSON entry per thread under
// `agent-overflow.sizePriors.v1.<threadId>`, plus one JSON string[] index
// under `agent-overflow.sizePriors.v1.index` (LRU order, most-recent
// LAST) so a 50-thread storage cap can evict the oldest thread without
// scanning every key. The `v1` segment is a schema version: `install`
// sweeps any `agent-overflow.sizePriors.*` key that isn't under the
// current version, so a future shape change can bump the prefix instead
// of migrating old data.
//
// Writes are debounced (trailing, ~1s) and coalesced per thread — a
// streaming thread's rapid captures collapse into one write per quiet
// period — and flushed early on `pagehide`/`visibilitychange→hidden` so a
// closed tab or quit doesn't lose the last capture. Storage is inherently
// unreliable (quota, disabled storage, corrupt JSON from an older
// version or a hand-edited profile): every failure path warns once and
// degrades to "priors just don't persist this session" rather than
// throwing — a crash here would take the whole timeline down with it.

import type { SizePriorsEntry, SizePriorsStorageAdapter } from './priors';
import { setSizePriorsStorageAdapter } from './priors';

const PREFIX = 'agent-overflow.sizePriors.';
const VERSION_PREFIX = `${PREFIX}v1.`;
const INDEX_KEY = `${VERSION_PREFIX}index`;
const MAX_STORED_THREADS = 50;
const FLUSH_DEBOUNCE_MS = 1000;

/** On-disk shape — Map isn't JSON-serializable, so entries round-trip through pairs. */
interface StoredEntry {
  width: number;
  expansionSig: string;
  rows: [string, number][];
}

function hasLocalStorage(): boolean {
  return typeof localStorage !== 'undefined';
}

function entryKey(threadId: string): string {
  return `${VERSION_PREFIX}${threadId}`;
}

// LRU order, most-recent LAST. Lazily hydrated from storage on first use
// per module instance; `indexLoaded` avoids re-parsing on every call.
let indexOrder: string[] = [];
let indexLoaded = false;
let indexDirty = false;

// Entries queued for the next debounced flush, keyed by threadId (a
// re-persist before the flush fires simply replaces the pending value —
// only the latest capture per thread is ever written).
const pendingEntries = new Map<string, SizePriorsEntry>();
let flushTimer: ReturnType<typeof setTimeout> | undefined;

// Set once a write fails even after the evict-oldest retry. Persistence
// for the rest of the session becomes a no-op rather than retrying a
// storage that has already proven full/unavailable on every capture.
let disabled = false;
let warnedQuota = false;

function ensureIndexLoaded(): void {
  if (indexLoaded) return;
  indexLoaded = true;
  if (!hasLocalStorage()) return;
  const raw = localStorage.getItem(INDEX_KEY);
  if (!raw) return;
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    warnAndDrop(INDEX_KEY);
    return;
  }
  if (Array.isArray(parsed) && parsed.every((id) => typeof id === 'string')) {
    indexOrder = parsed;
  } else {
    warnAndDrop(INDEX_KEY);
  }
}

function warnAndDrop(key: string): void {
  localStorage.removeItem(key);
  console.warn(`priorsStorage: malformed entry at "${key}", dropping`);
}

function bumpRecency(threadId: string): void {
  const i = indexOrder.indexOf(threadId);
  if (i !== -1) indexOrder.splice(i, 1);
  indexOrder.push(threadId);
  indexDirty = true;
}

function scheduleFlush(): void {
  if (disabled) return;
  clearTimeout(flushTimer);
  flushTimer = setTimeout(flush, FLUSH_DEBOUNCE_MS);
}

/**
 * Writes one key, retrying once after evicting the single oldest stored
 * thread if the browser reports the write as over quota. Returns whether
 * the value is now durably stored.
 */
function writeItem(key: string, value: string): boolean {
  try {
    localStorage.setItem(key, value);
    return true;
  } catch (err) {
    const oldest = indexOrder.shift();
    if (oldest !== undefined) {
      indexDirty = true;
      localStorage.removeItem(entryKey(oldest));
      try {
        localStorage.setItem(key, value);
        return true;
      } catch {
        // Falls through to the disable path below.
      }
    }
    if (!warnedQuota) {
      warnedQuota = true;
      console.warn(
        'priorsStorage: write failed after evicting the oldest stored thread; disabling size-priors persistence for this session',
        err,
      );
    }
    disabled = true;
    return false;
  }
}

function flush(): void {
  flushTimer = undefined;
  if (disabled || !hasLocalStorage()) {
    pendingEntries.clear();
    indexDirty = false;
    return;
  }
  ensureIndexLoaded();

  for (const [threadId, entry] of pendingEntries) {
    const stored: StoredEntry = {
      width: entry.width,
      expansionSig: entry.expansionSig,
      rows: Array.from(entry.rows.entries()),
    };
    if (!writeItem(entryKey(threadId), JSON.stringify(stored))) {
      // writeItem already warned/disabled; drop the rest of this batch
      // rather than hammering a storage that just proved unwritable.
      pendingEntries.clear();
      return;
    }
  }
  pendingEntries.clear();

  while (indexOrder.length > MAX_STORED_THREADS) {
    const oldest = indexOrder.shift();
    if (oldest === undefined) break;
    localStorage.removeItem(entryKey(oldest));
    indexDirty = true;
  }

  if (indexDirty && writeItem(INDEX_KEY, JSON.stringify(indexOrder))) {
    indexDirty = false;
  }
}

function flushNow(): void {
  clearTimeout(flushTimer);
  flushTimer = undefined;
  flush();
}

function handleVisibilityChange(): void {
  if (document.visibilityState === 'hidden') flushNow();
}

function validateStoredEntry(parsed: unknown): SizePriorsEntry | undefined {
  if (typeof parsed !== 'object' || parsed === null) return undefined;
  const candidate = parsed as Partial<StoredEntry>;
  if (typeof candidate.width !== 'number' || !Number.isFinite(candidate.width)) return undefined;
  if (typeof candidate.expansionSig !== 'string') return undefined;
  if (!Array.isArray(candidate.rows)) return undefined;
  const rows = new Map<string, number>();
  for (const pair of candidate.rows) {
    if (!Array.isArray(pair) || pair.length !== 2) return undefined;
    const [sig, height] = pair as [unknown, unknown];
    // `height < 0` is corrupt data (capture filters UNMEASURED/negatives
    // before persisting — see maybePersistSizePriors). Storage is the one
    // untrusted source feeding RowEstimate.at, and a negative estimate
    // would poison the size store's offsets — reject the whole entry.
    if (typeof sig !== 'string' || typeof height !== 'number' || !Number.isFinite(height) || height < 0) {
      return undefined;
    }
    rows.set(sig, height);
  }
  return { width: candidate.width, expansionSig: candidate.expansionSig, rows };
}

function load(threadId: string): SizePriorsEntry | undefined {
  if (!hasLocalStorage()) return undefined;
  ensureIndexLoaded();
  const key = entryKey(threadId);
  const raw = localStorage.getItem(key);
  if (!raw) return undefined;

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    warnAndDrop(key);
    return undefined;
  }
  const entry = validateStoredEntry(parsed);
  if (!entry) {
    warnAndDrop(key);
    return undefined;
  }

  // Bump recency but fold the write into the next debounced flush rather
  // than writing the index synchronously on every load — a thread-switch
  // hot path should not force a synchronous localStorage write.
  bumpRecency(threadId);
  scheduleFlush();
  return entry;
}

function persist(threadId: string, entry: SizePriorsEntry): void {
  if (disabled || !hasLocalStorage()) return;
  ensureIndexLoaded();
  pendingEntries.set(threadId, entry);
  bumpRecency(threadId);
  scheduleFlush();
}

function remove(threadId: string): void {
  pendingEntries.delete(threadId);
  if (!hasLocalStorage()) return;
  ensureIndexLoaded();
  localStorage.removeItem(entryKey(threadId));
  const i = indexOrder.indexOf(threadId);
  if (i !== -1) {
    indexOrder.splice(i, 1);
    indexDirty = true;
    scheduleFlush();
  }
}

function sweepStaleVersions(): void {
  if (!hasLocalStorage()) return;
  const stale: string[] = [];
  for (let i = 0; i < localStorage.length; i++) {
    const key = localStorage.key(i);
    if (key && key.startsWith(PREFIX) && !key.startsWith(VERSION_PREFIX)) {
      stale.push(key);
    }
  }
  for (const key of stale) localStorage.removeItem(key);
}

const adapter: SizePriorsStorageAdapter = { load, persist, remove };

/**
 * Wires the localStorage adapter into priors.ts and sweeps stale-version
 * keys. Naturally idempotent (re-running the sweep is a no-op once
 * stale keys are gone, re-registering the same listener references is a
 * no-op per DOM semantics, and re-installing the same adapter object is
 * harmless) — call it eagerly and as often as needed rather than guard
 * it with an install flag. Called at MODULE SCOPE of
 * `components/chat/timelineSizePriors.svelte.ts` so persistence is
 * active before any pane mounts, in both the embedded webview and
 * `agent-overflow --connect` browser mode.
 */
export function installSizePriorsPersistence(): void {
  sweepStaleVersions();
  setSizePriorsStorageAdapter(adapter);
  if (!hasLocalStorage()) return;
  window.addEventListener('pagehide', flushNow);
  document.addEventListener('visibilitychange', handleVisibilityChange);
}

/** Test-only: resets every module-level flush/dirty/disabled state and
 * clears storage back to empty, so suites don't leak a pending debounce
 * timer or a disabled-persistence flag into the next test. */
export function __resetSizePriorsStorageForTest(): void {
  clearTimeout(flushTimer);
  flushTimer = undefined;
  pendingEntries.clear();
  indexOrder = [];
  indexLoaded = false;
  indexDirty = false;
  disabled = false;
  warnedQuota = false;
  if (!hasLocalStorage()) return;
  const stale: string[] = [];
  for (let i = 0; i < localStorage.length; i++) {
    const key = localStorage.key(i);
    if (key && key.startsWith(VERSION_PREFIX)) stale.push(key);
  }
  for (const key of stale) localStorage.removeItem(key);
}
