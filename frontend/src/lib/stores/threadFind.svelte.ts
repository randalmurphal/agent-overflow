// The find provider of every pane that renders a thread timeline: a thread
// pane, a side chat, and an agent pane over a scoped transcript.
//
// Hits come from the store's indexed read of the transcript the pane shows,
// in document order. Typing is debounced. One read is in flight at a time:
// a newer query or an invalidation waits, replacing any other waiting read,
// and a close drops the waiting one. An answer applies only while its
// query, its transcript, the thread's owner and the timeline's generation
// are all still the ones it was asked for.
//
// A new query starts at the first hit at or below the reading position (the
// last hit when none is below), so the view moves as little as possible;
// stepping wraps. Only a new query and a step move the view, through the
// pane's scroll-to-item request, owned by this find so a replaced query or
// a close withdraws a jump the timeline is still loading. A refresh (the
// transcript changed) updates the count and the current hit in place. DOM
// work (reading position, highlights) belongs to the host.

import { SearchThreadItems, type ThreadMessageHit } from './bindings';
import type { PaneFindProvider, PaneFindResult } from './paneFind.svelte';
import type { ScrollToItemOptions } from './threadPaneShared';
import type { BackendKey } from '../transport/backends';
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
  /** The computer that owns the thread. */
  backend: BackendKey;
  /** The timeline's switch generation: a switch, reload or clear moves it. */
  generation: number;
}

export interface ThreadFindHost {
  /** The transcript searched; null while the pane has no thread. */
  scope(): ThreadFindScope | null;
  /** The pane (or scoped view) whose timeline takes the reveal. */
  pane(): {
    requestScrollToItem(itemId: string, options?: ScrollToItemOptions): void;
    withdrawScrollToItem(owner: object): void;
  } | null;
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
  /**
   * The transcript, its owner or its generation changed: drop the hits and
   * search the current query again.
   */
  rescope(): void;
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

export function threadFindScopeKey(scope: ThreadFindScope | null): string {
  return scope
    ? `${scope.threadId}\u0000${scope.scopeRootId ?? ''}\u0000${scope.backend}\u0000${scope.generation}`
    : '';
}

export function createThreadFind(host: ThreadFindHost, noun: () => string = () => 'thread'): ThreadFind {
  let query = '';
  let hits = $state.raw<Hit[]>([]);
  let current = $state(-1);
  let pending = $state(false);
  // Moves on every query change, rescope and close; an answer must match it.
  let seq = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let inFlight = false;
  let waiting: 'query' | 'refresh' | null = null;
  // Identifies this find's scroll-to-item requests for withdrawal.
  const owner = {};

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
    host.pane()?.requestScrollToItem(id, { mark: true, owner });
  }

  function cancelTimer(): void {
    if (timer !== null) clearTimeout(timer);
    timer = null;
  }

  function dropHits(): void {
    hits = [];
    current = -1;
  }

  /** Ends the session's reads and its reveal; the next read starts fresh. */
  function supersede(): void {
    cancelTimer();
    refreshes.reset();
    waiting = null;
    seq += 1;
    host.pane()?.withdrawScrollToItem(owner);
  }

  function enqueue(mode: 'query' | 'refresh'): void {
    if (inFlight) {
      if (waiting !== 'query') waiting = mode;
      return;
    }
    inFlight = true;
    void read(mode).finally(() => {
      inFlight = false;
      const next = waiting;
      waiting = null;
      if (next && query) enqueue(next);
    });
  }

  async function read(mode: 'query' | 'refresh'): Promise<void> {
    const mine = seq;
    const asked = query;
    const scope = host.scope();
    const key = threadFindScopeKey(scope);
    const stale = (): boolean => mine !== seq || asked !== query || key !== threadFindScopeKey(host.scope());
    if (!scope || !asked) {
      dropHits();
      pending = false;
      paint();
      return;
    }
    let answer: Hit[];
    try {
      const selection = scope.scopeRootId ? { scopeRootId: scope.scopeRootId } : undefined;
      answer = (await SearchThreadItems(scope.threadId, asked, THREAD_FIND_LIMIT, selection)) ?? [];
    } catch (err) {
      if (stale()) return;
      dropHits();
      pending = false;
      paint();
      addErrorToast(`Find failed: ${errString(err)}`, err);
      return;
    }
    if (stale()) return;
    if (mode === 'refresh') {
      const previous = hits[current];
      hits = answer;
      const kept = previous ? answer.findIndex((h) => h.itemId === previous.itemId) : -1;
      current = kept >= 0 ? kept : currentHitIndex(answer, previous ?? host.readingPosition());
      paint();
      return;
    }
    hits = answer;
    pending = false;
    current = currentHitIndex(answer, host.readingPosition());
    paint();
    reveal(false);
  }

  // An invalidation while a query is still debouncing needs nothing: that
  // query reads the changed transcript. One while a read is out waits.
  const refreshes = createRefreshScheduler({
    name: 'thread find',
    delayMs: REFRESH_DELAY_MS,
    maxWaitMs: REFRESH_MAX_WAIT_MS,
    run: (token) => {
      if (!token.isCurrent() || !query || (pending && !inFlight)) return;
      enqueue('refresh');
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
      supersede();
      query = next.trim();
      if (!query) {
        dropHits();
        pending = false;
        paint();
        return;
      }
      pending = true;
      host.paint(query, null);
      timer = setTimeout(() => {
        timer = null;
        enqueue('query');
      }, THREAD_FIND_DEBOUNCE_MS);
    },
    next: () => step(1),
    previous: () => step(-1),
    refresh(): void {
      if (query) refreshes.request();
    },
    rescope(): void {
      supersede();
      dropHits();
      pending = query !== '';
      host.paint(query, null);
      if (query) enqueue('query');
    },
    clear(): void {
      supersede();
      query = '';
      dropHits();
      pending = false;
      host.paint('', null);
    },
  };
}
