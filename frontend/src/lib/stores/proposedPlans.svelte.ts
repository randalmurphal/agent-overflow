import type { Item, ProposedPlanComment } from '../types/models';
import { comparePlanItemPosition, latestProposedPlanItem } from '../utils/proposedPlan';
import { ListProposedPlanComments, ListThreadProposedPlans } from './bindings';
import { onItemUpsert } from './events';
import { itemsAreEqual } from './threadItems';

const REFRESH_DEBOUNCE_MS = 100;
const MAX_CACHED_THREADS = 64;
const MAX_CACHED_PLAN_COMMENTS = 32;

interface ProposedPlanCacheEntry {
  items: Item[];
  fetchSeq: number;
  // Increments on every sync upsert. Compared in refreshThreadProposedPlans
  // to detect upserts that arrived while the RPC was in flight; if any did,
  // the server response is considered stale-relative-to-local and is not
  // allowed to wipe the locally-observed items.
  upsertSeq: number;
  loaded: boolean;
}

interface ProposedPlanCommentCacheEntry {
  comments: ProposedPlanComment[];
  fetchSeq: number;
  loaded: boolean;
}

const cache = $state<Record<string, ProposedPlanCacheEntry>>({});
const cacheAccess = new Map<string, number>();
const refreshTimers = new Map<string, ReturnType<typeof setTimeout>>();
let listenerRefCount = 0;
let cancelItemUpsert: (() => void) | null = null;
const listenerThreadScopes = new Set<() => string | null | undefined>();

const commentCache = $state<Record<string, ProposedPlanCommentCacheEntry>>({});
const commentCacheAccess = new Map<string, number>();

function entryForThread(threadId: string): ProposedPlanCacheEntry {
  cache[threadId] ??= {
    items: [],
    fetchSeq: 0,
    upsertSeq: 0,
    loaded: false,
  };
  cacheAccess.set(threadId, Date.now());
  return cache[threadId];
}

function commentCacheKey(threadId: string, planItemId: string): string {
  return `${threadId}:${planItemId}`;
}

function entryForPlanComments(threadId: string, planItemId: string): ProposedPlanCommentCacheEntry {
  const key = commentCacheKey(threadId, planItemId);
  commentCache[key] ??= {
    comments: [],
    fetchSeq: 0,
    loaded: false,
  };
  commentCacheAccess.set(key, Date.now());
  return commentCache[key];
}

function itemListsAreEqual(left: readonly Item[], right: readonly Item[]): boolean {
  if (left.length !== right.length) return false;
  for (let i = 0; i < left.length; i += 1) {
    const a = left[i];
    const b = right[i];
    if (!a || !b || !itemsAreEqual(a, b)) return false;
  }
  return true;
}

export function getThreadProposedPlans(threadId: string | null | undefined): Item[] {
  if (!threadId) return [];
  const entry = cache[threadId];
  if (!entry) return [];
  cacheAccess.set(threadId, Date.now());
  return entry.items;
}

export function getThreadCurrentProposedPlan(
  threadId: string | null | undefined,
): Item | null {
  return latestProposedPlanItem(threadId, getThreadProposedPlans(threadId));
}

export function hasLoadedThreadProposedPlans(threadId: string | null | undefined): boolean {
  if (!threadId) return false;
  return cache[threadId]?.loaded ?? false;
}

export function getPlanComments(
  threadId: string | null | undefined,
  planItemId: string | null | undefined,
): ProposedPlanComment[] {
  if (!threadId || !planItemId) return [];
  const key = commentCacheKey(threadId, planItemId);
  const entry = commentCache[key];
  if (!entry) return [];
  commentCacheAccess.set(key, Date.now());
  return entry.comments;
}

// Each call starts its own fetch and uses `fetchSeq` to discard stale
// resolutions. With both Composer and PlanSidebar registering refresh on
// thread-switch, the worst case is one redundant fetch per switch — the
// `fetchSeq` guard ensures the latest result wins.
export async function refreshThreadProposedPlans(threadId: string | null | undefined): Promise<void> {
  if (!threadId) return;
  const entry = entryForThread(threadId);
  const seq = entry.fetchSeq + 1;
  entry.fetchSeq = seq;
  const upsertSeqAtFetchStart = entry.upsertSeq;
  try {
    const items = ((await ListThreadProposedPlans(threadId)) as Item[] | null) ?? [];
    if (entry.fetchSeq !== seq) return;
    // If a sync upsert landed during the fetch, the local cache holds items
    // the server response may not yet reflect (e.g. server-side eventual
    // consistency between the upsert event and the list query). Skip the
    // wholesale replace — the upserted items stay, and a subsequent
    // refresh (debounced after the latest upsert) will reconcile.
    if (entry.upsertSeq !== upsertSeqAtFetchStart) {
      cacheAccess.set(threadId, Date.now());
      return;
    }
    const nextItems = items.filter((item) => item.threadId === threadId);
    if (!itemListsAreEqual(entry.items, nextItems)) {
      entry.items = nextItems;
    }
    entry.loaded = true;
    cacheAccess.set(threadId, Date.now());
    evictOldPlanCacheEntries();
  } catch (err) {
    if (entry.fetchSeq !== seq) return;
    console.error('proposedPlans: ListThreadProposedPlans failed:', err);
    // Same upsert-during-fetch guard: don't blank locally-observed items
    // because the RPC happened to fail.
    if (entry.upsertSeq !== upsertSeqAtFetchStart) {
      cacheAccess.set(threadId, Date.now());
      return;
    }
    if (entry.items.length > 0) {
      entry.items = [];
    }
    entry.loaded = true;
    cacheAccess.set(threadId, Date.now());
  }
}

export async function refreshPlanComments(
  threadId: string | null | undefined,
  planItemId: string | null | undefined,
): Promise<void> {
  if (!threadId || !planItemId) return;
  const key = commentCacheKey(threadId, planItemId);
  const entry = entryForPlanComments(threadId, planItemId);
  const seq = entry.fetchSeq + 1;
  entry.fetchSeq = seq;
  try {
    const next = ((await ListProposedPlanComments(threadId, planItemId)) as ProposedPlanComment[] | null) ?? [];
    if (entry.fetchSeq !== seq) return;
    entry.comments = next;
    entry.loaded = true;
    commentCacheAccess.set(key, Date.now());
    evictOldPlanCommentCacheEntries();
  } catch (err) {
    if (entry.fetchSeq !== seq) return;
    console.error('proposedPlans: ListProposedPlanComments failed:', err);
    entry.comments = [];
    entry.loaded = true;
    commentCacheAccess.set(key, Date.now());
  }
}

function evictOldPlanCacheEntries(): void {
  const entries = Object.entries(cache);
  if (entries.length <= MAX_CACHED_THREADS) return;
  const retainedThreadIds = new Set<string>();
  for (const scope of listenerThreadScopes) {
    const threadId = scope();
    if (threadId) retainedThreadIds.add(threadId);
  }
  const evictionCandidates = entries
    .filter(([threadId]) => !retainedThreadIds.has(threadId))
    .sort(([a], [b]) => (cacheAccess.get(a) ?? 0) - (cacheAccess.get(b) ?? 0));
  for (const [threadId] of evictionCandidates.slice(0, entries.length - MAX_CACHED_THREADS)) {
    delete cache[threadId];
    cacheAccess.delete(threadId);
  }
}

function evictOldPlanCommentCacheEntries(): void {
  const entries = Object.entries(commentCache);
  if (entries.length <= MAX_CACHED_PLAN_COMMENTS) return;
  const evictionCandidates = entries
    .sort(([a], [b]) => (commentCacheAccess.get(a) ?? 0) - (commentCacheAccess.get(b) ?? 0));
  for (const [key] of evictionCandidates.slice(0, entries.length - MAX_CACHED_PLAN_COMMENTS)) {
    delete commentCache[key];
    commentCacheAccess.delete(key);
  }
}

function upsertItemIntoCache(item: Item): boolean {
  const wasNewEntry = cache[item.threadId] === undefined;
  const entry = entryForThread(item.threadId);
  const existingIdx = entry.items.findIndex((e) => e.id === item.id);
  if (existingIdx >= 0) {
    const existing = entry.items[existingIdx];
    if (existing && itemsAreEqual(existing, item)) {
      entry.loaded = true;
      return false;
    }
    const replaced = entry.items.slice();
    replaced[existingIdx] = item;
    replaced.sort(comparePlanItemPosition);
    entry.items = replaced;
  } else {
    entry.items = [...entry.items, item].sort(comparePlanItemPosition);
  }
  entry.loaded = true;
  entry.upsertSeq += 1;
  // Sync upserts can land for any thread the backend pushes events for,
  // including threads with no observer. Without this, the cache could grow
  // past MAX_CACHED_THREADS in a long-running session because eviction
  // otherwise only runs in refreshThreadProposedPlans's success branch.
  if (wasNewEntry) evictOldPlanCacheEntries();
  return true;
}

export function scheduleThreadProposedPlansRefresh(threadId: string | null | undefined): void {
  if (!threadId) return;
  const existing = refreshTimers.get(threadId);
  if (existing) clearTimeout(existing);
  const timer = setTimeout(() => {
    refreshTimers.delete(threadId);
    void refreshThreadProposedPlans(threadId);
  }, REFRESH_DEBOUNCE_MS);
  refreshTimers.set(threadId, timer);
}

// Module-level handler, NOT defined inside retainProposedPlanEventListener:
// the registration is a refcounted singleton that outlives individual
// retainers, and a callback created inside the retain call would close over
// the FIRST caller's `threadIdScope` argument forever — pinning that
// caller's component scope chain (and through it the pane's entire DOM
// tree) until the last release, long after that pane closed. Found via the
// DOM-retention probe (chatview-dom-retention.test.ts).
function handlePlanItemUpsert(item: Item): void {
  if (item.payloadKind !== 'proposed_plan') return;
  // Synchronous insert ensures observers (PlanSidebar, Composer) see the
  // new plan immediately — without this, getThreadCurrentProposedPlan
  // returns stale data for ~100ms until the debounced refresh resolves.
  // That window forced PlanSidebar to read pane.items as a fallback,
  // which coupled it to chat streaming. See Composer.svelte / PlanSidebar.svelte.
  const changed = upsertItemIntoCache(item);
  if (!changed) return;
  if (listenerThreadScopes.size > 0) {
    let matched = false;
    for (const scope of listenerThreadScopes) {
      if (scope() === item.threadId) {
        matched = true;
        break;
      }
    }
    if (!matched) return;
  }
  scheduleThreadProposedPlansRefresh(item.threadId);
}

export function retainProposedPlanEventListener(threadIdScope?: () => string | null | undefined): () => void {
  listenerRefCount += 1;
  if (threadIdScope) {
    listenerThreadScopes.add(threadIdScope);
  }
  cancelItemUpsert ??= onItemUpsert(handlePlanItemUpsert);

  let released = false;
  return () => {
    if (released) return;
    released = true;
    if (threadIdScope) {
      listenerThreadScopes.delete(threadIdScope);
    }
    listenerRefCount -= 1;
    if (listenerRefCount > 0) return;
    cancelItemUpsert?.();
    cancelItemUpsert = null;
  };
}

/**
 * Test-only entry point that exercises the same sync-insert path used by the
 * onItemUpsert listener, without requiring the test to drive the event bus.
 * Production code MUST go through retainProposedPlanEventListener so the
 * cache stays the single source of truth.
 */
export function upsertProposedPlanForTests(item: Item): void {
  if (item.payloadKind !== 'proposed_plan') return;
  upsertItemIntoCache(item);
}

export function resetProposedPlanCacheForTests(): void {
  for (const timer of refreshTimers.values()) {
    clearTimeout(timer);
  }
  refreshTimers.clear();
  for (const key of Object.keys(cache)) {
    delete cache[key];
  }
  for (const key of Object.keys(commentCache)) {
    delete commentCache[key];
  }
  cacheAccess.clear();
  commentCacheAccess.clear();
  cancelItemUpsert?.();
  cancelItemUpsert = null;
  listenerThreadScopes.clear();
  listenerRefCount = 0;
}
