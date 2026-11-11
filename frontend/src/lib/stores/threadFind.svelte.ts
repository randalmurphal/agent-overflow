// The find provider of every pane that renders a thread timeline: a thread
// pane, a side chat, and an agent pane over a scoped transcript.
//
// Hits come from the store's indexed read of the transcript the pane shows,
// in document order. Typing is debounced and a late answer for an older query
// is dropped. A new query starts at the first hit at or below the reading
// position (the last hit when none is below), so the view moves as little as
// possible; stepping wraps. Every move goes through the pane's scroll-to-item
// request, the reveal search navigation uses, which pages the item in when it
// is outside the loaded window and marks the landing. DOM work (reading
// position, highlights) belongs to the host.

import { SearchThreadItems, type ThreadMessageHit } from './bindings';
import type { PaneFindProvider, PaneFindResult } from './paneFind.svelte';
import { addErrorToast } from './toast.svelte';
import { createRefreshScheduler } from '../utils/refreshScheduler';
import { errString } from '../utils/errors';

export const THREAD_FIND_DEBOUNCE_MS = 150;
export const THREAD_FIND_LIMIT = 1000;
const REFRESH_DELAY_MS = 500;
const REFRESH_MAX_WAIT_MS = 2000;

export interface TimelineCoordinate {
  turnIndex: number;
  itemIndex: number;
}

type Hit = Pick<ThreadMessageHit, 'itemId' | 'turnIndex' | 'itemIndex'>;

export interface ThreadFindScope {
  threadId: string;
  /** The agent transcript root; absent for the main transcript. */
  scopeRootId?: string;
}

export interface ThreadFindHost {
  /** The transcript searched; null while the pane has no thread. */
  scope(): ThreadFindScope | null;
  /** The pane (or scoped view) whose timeline takes the reveal. */
  pane(): { requestScrollToItem(itemId: string, options?: { mark?: boolean }): void } | null;
  /** The coordinate of the first row at or below the viewport top. */
  readingPosition(): TimelineCoordinate | null;
  /** True when the item's row is mounted and in the viewport. */
  isOnScreen(itemId: string): boolean;
  /** Paint the query's matches, with the current hit's row distinguished. */
  paint(query: string, currentItemId: string | null): void;
}

export interface ThreadFind extends PaneFindProvider {
  next(): void;
  previous(): void;
  /** Re-read the current query (the transcript changed); keeps the current hit. */
  refresh(): void;
}

function compare(a: TimelineCoordinate, b: TimelineCoordinate): number {
  return a.turnIndex - b.turnIndex || a.itemIndex - b.itemIndex;
}

/** First hit at or below `position`, else the last; -1 with no hits. */
export function currentHitIndex(hits: readonly Hit[], position: TimelineCoordinate | null): number {
  if (hits.length === 0) return -1;
  if (!position) return 0;
  const below = hits.findIndex((h) => compare(h, position) >= 0);
  return below >= 0 ? below : hits.length - 1;
}

function scopeKey(scope: ThreadFindScope | null): string {
  return scope ? `${scope.threadId}\u0000${scope.scopeRootId ?? ''}` : '';
}

export function createThreadFind(host: ThreadFindHost, noun: () => string = () => 'thread'): ThreadFind {
  let query = '';
  let hits = $state.raw<Hit[]>([]);
  let current = $state(-1);
  let pending = $state(false);
  let seq = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;

  const result: PaneFindResult = {
    get count() { return hits.length; },
    get current() { return current; },
    get pending() { return pending; },
    get capped() { return hits.length >= THREAD_FIND_LIMIT; },
  };

  function currentId(): string | null {
    return hits[current]?.itemId ?? null;
  }

  function paint(): void {
    host.paint(query, currentId());
  }

  function reveal(force: boolean): void {
    const id = currentId();
    if (!id) return;
    if (!force && host.isOnScreen(id)) return;
    host.pane()?.requestScrollToItem(id, { mark: true });
  }

  function cancelTimer(): void {
    if (timer !== null) clearTimeout(timer);
    timer = null;
  }

  async function read(mode: 'query' | 'refresh'): Promise<void> {
    const mine = ++seq;
    const asked = query;
    const scope = host.scope();
    const key = scopeKey(scope);
    if (!scope || !asked) {
      hits = [];
      current = -1;
      pending = false;
      paint();
      return;
    }
    let answer: Hit[];
    try {
      const selection = scope.scopeRootId ? { scopeRootId: scope.scopeRootId } : undefined;
      answer = (await SearchThreadItems(scope.threadId, asked, THREAD_FIND_LIMIT, selection)) ?? [];
    } catch (err) {
      if (mine !== seq) return;
      hits = [];
      current = -1;
      pending = false;
      paint();
      addErrorToast(`Find failed: ${errString(err)}`, err);
      return;
    }
    if (mine !== seq || asked !== query || key !== scopeKey(host.scope())) return;
    const previous = hits[current];
    hits = answer;
    pending = false;
    if (mode === 'refresh' && previous) {
      const kept = answer.findIndex((h) => h.itemId === previous.itemId);
      current = kept >= 0 ? kept : currentHitIndex(answer, previous);
      paint();
      return;
    }
    current = currentHitIndex(answer, host.readingPosition());
    paint();
    reveal(false);
  }

  const refreshes = createRefreshScheduler({
    name: 'thread find',
    delayMs: REFRESH_DELAY_MS,
    maxWaitMs: REFRESH_MAX_WAIT_MS,
    run: (token) => {
      if (!token.isCurrent() || !query || pending) return;
      return read('refresh');
    },
  });

  function step(delta: 1 | -1): void {
    if (hits.length === 0 || pending) return;
    current = current < 0 ? 0 : (current + delta + hits.length) % hits.length;
    paint();
    reveal(true);
  }

  return {
    get noun() { return noun(); },
    result,
    search(next: string): void {
      cancelTimer();
      seq += 1;
      refreshes.reset();
      query = next.trim();
      if (!query) {
        hits = [];
        current = -1;
        pending = false;
        paint();
        return;
      }
      pending = true;
      host.paint(query, null);
      timer = setTimeout(() => {
        timer = null;
        void read('query');
      }, THREAD_FIND_DEBOUNCE_MS);
    },
    next: () => step(1),
    previous: () => step(-1),
    refresh(): void {
      if (query) refreshes.request();
    },
    clear(): void {
      cancelTimer();
      refreshes.reset();
      seq += 1;
      query = '';
      hits = [];
      current = -1;
      pending = false;
      host.paint('', null);
    },
  };
}
