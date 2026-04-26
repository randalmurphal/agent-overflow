import type { Item, ProposedPlanComment } from '../types/models';
import { latestProposedPlanItem } from '../utils/proposedPlan';
import { ListProposedPlanComments, ListThreadProposedPlans } from './bindings';
import { onItemUpsert } from './events';

const REFRESH_DEBOUNCE_MS = 100;
const MAX_CACHED_THREADS = 64;
const MAX_CACHED_PLAN_COMMENTS = 32;

interface ProposedPlanCacheEntry {
  items: Item[];
  fetchSeq: number;
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

export function getThreadProposedPlans(threadId: string | null | undefined): Item[] {
  if (!threadId) return [];
  const entry = cache[threadId];
  if (!entry) return [];
  cacheAccess.set(threadId, Date.now());
  return entry.items;
}

export function getThreadCurrentProposedPlan(
  threadId: string | null | undefined,
  visibleItems: readonly Item[] | null | undefined = [],
): Item | null {
  return latestProposedPlanItem(threadId, visibleItems, getThreadProposedPlans(threadId));
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
  try {
    const items = ((await ListThreadProposedPlans(threadId)) as Item[] | null) ?? [];
    if (entry.fetchSeq !== seq) return;
    entry.items = items.filter((item) => item.threadId === threadId);
    entry.loaded = true;
    cacheAccess.set(threadId, Date.now());
    evictOldPlanCacheEntries();
  } catch (err) {
    if (entry.fetchSeq !== seq) return;
    console.error('proposedPlans: ListThreadProposedPlans failed:', err);
    entry.items = [];
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

export function retainProposedPlanEventListener(threadIdScope?: () => string | null | undefined): () => void {
  listenerRefCount += 1;
  if (threadIdScope) {
    listenerThreadScopes.add(threadIdScope);
  }
  if (!cancelItemUpsert) {
    cancelItemUpsert = onItemUpsert((item) => {
      if (item.payloadKind !== 'proposed_plan') return;
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
    });
  }

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
