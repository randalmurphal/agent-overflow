// The find provider of every pane that renders a thread timeline: a thread
// pane, a side chat, and an agent pane over a scoped transcript.
//
// The store answers candidates: the rows of the transcript the pane shows
// that may hold a match, in document order, as pages serve them. A hit is a
// candidate whose find text (host.findRuns, chat/threadFindText.ts)
// holds a match by the find rule, the text the highlighter paints, so the
// count and the paint read the same text. Hits are re-decided when a row's
// find text can change without a read (host.reevaluate callers: an
// expanded body loading).
//
// Typing is debounced. One read is in flight at a time: a newer query or an
// invalidation waits, replacing any other waiting read, and a close drops
// the waiting one. An answer applies only while its query, its transcript,
// the thread's owner and the timeline's generation are all still the ones
// it was asked for. A refresh passes the last answer's state, so the store
// re-reads only the rows written since (SearchThreadItems); it answers in
// full when it cannot.
//
// A new query starts at the first hit at or below the reading position (the
// last hit when none is below), so the view moves as little as possible;
// stepping wraps. Only a new query and a step move the view, through the
// pane's scroll-to-item request, owned by this find so a replaced query or
// a close withdraws a jump the timeline is still loading. A refresh (the
// transcript changed) updates the count and the current hit in place. DOM
// work (reading position, highlights) belongs to the host.

import { SearchThreadItems, type ThreadFindSince } from './bindings';
import { paneFindReads, type PaneFindProvider, type PaneFindResult } from './paneFind.svelte';
import type { ScrollToItemOptions } from './threadPaneShared';
import type { BackendKey } from '../transport/backends';
import type { Item } from '../types/models';
import { addErrorToast } from './toast.svelte';
import { createRefreshScheduler } from '../utils/refreshScheduler';
import { errString } from '../utils/errors';
import { countFindMatches, findNeedle } from '../utils/findMatch';

export const THREAD_FIND_DEBOUNCE_MS = 150;
export const THREAD_FIND_LIMIT = 1000;
const REFRESH_DELAY_MS = 500;
const REFRESH_MAX_WAIT_MS = 2000;

export interface TimelineCoordinate {
  turnIndex: number;
  itemIndex: number;
}

interface Hit {
  itemId: string;
  turnIndex: number;
  itemIndex: number;
}

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
  /** The layout pane whose read slot this find's reads share. */
  paneId(): string;
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
  /** The runs of text a row's find text shows (threadFindText.ts). */
  findRuns(item: Item): readonly string[];
  /** Whether a row's find text can change with no new read (an expanded body). */
  findRunsLive(item: Item): boolean;
  /** Paint the query's matches, with the current hit's row distinguished. */
  paint(query: string, currentItemId: string | null): void;
}

export interface ThreadFind extends PaneFindProvider {
  next(): void;
  previous(): void;
  /** Re-read the current query (the transcript changed); keeps the current hit. */
  refresh(): void;
  /** Re-decide the hits among the candidates held (a row's find text changed). */
  reevaluate(): void;
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
export function currentHitIndex(hits: readonly TimelineCoordinate[], position: TimelineCoordinate | null): number {
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
  let capped = $state(false);
  // The candidates of the last answer, by id, and the state it read.
  let candidates = new Map<string, Item>();
  let since: ThreadFindSince | null = null;
  // Whether a candidate's find text matches the needle, for rows whose
  // find text only a new read changes. Keyed by the row object an answer gave.
  let decided = new WeakMap<Item, boolean>();
  let decidedNeedle = '';
  // Moves on every query change, rescope and close; an answer must match it.
  let seq = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  // The read this find has asked the pane's slot for and not yet started.
  let queued: 'query' | 'refresh' | null = null;
  // True from a read's start until it settles.
  let reading = false;
  // Identifies this find's scroll-to-item requests for withdrawal.
  const owner = {};

  const result: PaneFindResult = {
    get count() { return hits.length; },
    get current() { return current; },
    get pending() { return pending; },
    get capped() { return capped; },
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
    capped = false;
    candidates = new Map();
    since = null;
  }

  function matches(item: Item, needle: string): boolean {
    if (needle !== decidedNeedle) {
      decided = new WeakMap();
      decidedNeedle = needle;
    }
    const live = host.findRunsLive(item);
    if (!live) {
      const known = decided.get(item);
      if (known !== undefined) return known;
    }
    const hit = host.findRuns(item).some((run) => countFindMatches(run, needle) > 0);
    if (!live) decided.set(item, hit);
    return hit;
  }

  /** The hits among the candidates, in document order. */
  function decide(): Hit[] {
    const needle = findNeedle(query);
    const out: Hit[] = [];
    for (const item of candidates.values()) {
      if (matches(item, needle)) out.push({ itemId: item.id, turnIndex: item.turnIndex, itemIndex: item.itemIndex });
    }
    return out.sort(compare);
  }

  /** Re-decides the hits and keeps the current one where it can. */
  function settle(fallback: TimelineCoordinate | null): boolean {
    const previous = hits[current];
    const next = decide();
    if (next.length === hits.length && next.every((hit, at) => hit.itemId === hits[at].itemId)) return false;
    hits = next;
    const kept = previous ? next.findIndex((h) => h.itemId === previous.itemId) : -1;
    current = kept >= 0 ? kept : currentHitIndex(next, previous ?? fallback);
    return true;
  }

  /** Ends the session's reads and its reveal; the next read starts fresh. */
  function supersede(): void {
    cancelTimer();
    refreshes.reset();
    queued = null;
    seq += 1;
    host.pane()?.withdrawScrollToItem(owner);
  }

  // A waiting query covers a refresh: it reads the changed transcript.
  function enqueue(mode: 'query' | 'refresh'): void {
    if (queued === 'query') return;
    queued = mode;
    paneFindReads(host.paneId()).request(async () => {
      const next = queued;
      queued = null;
      if (!next || !query) return;
      reading = true;
      try {
        await read(next);
      } finally {
        reading = false;
      }
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
    const after = mode === 'refresh' ? since : null;
    let answer;
    try {
      const selection = scope.scopeRootId ? { scopeRootId: scope.scopeRootId } : undefined;
      answer = await SearchThreadItems(scope.threadId, asked, THREAD_FIND_LIMIT, selection, after);
    } catch (err) {
      if (stale()) return;
      dropHits();
      pending = false;
      paint();
      addErrorToast(`Find failed: ${errString(err)}`, err);
      return;
    }
    if (stale()) return;
    const items = (answer?.items ?? []) as unknown as Item[];
    if (answer?.incremental && after) {
      for (const id of answer.changed ?? []) candidates.delete(id);
    } else {
      candidates = new Map();
      capped = answer?.capped === true;
    }
    for (const item of items) candidates.set(item.id, item);
    since = answer?.since ?? null;
    if (mode === 'refresh') {
      settle(host.readingPosition());
      paint();
      return;
    }
    hits = decide();
    pending = false;
    current = currentHitIndex(hits, host.readingPosition());
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
      if (!token.isCurrent() || !query || (pending && !reading && queued === null)) return;
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
      // The held candidates answer another query; the next read is full.
      since = null;
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
    reevaluate(): void {
      if (!query || pending || candidates.size === 0) return;
      if (settle(host.readingPosition())) paint();
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
