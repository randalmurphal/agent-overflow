// Find in the browser pane (docs/architecture/browser-tools.md, "Find"). The
// backend's engine searches the page the pane shows and owns the session; this
// provider drives it from the pane's find bar, or for an engine with its own
// find UI opens that instead. A session the backend ends on its own (the page
// navigated, the tab changed, the page closed) arrives as a `find` companion
// event and closes the bar.
//
// Engine calls run through the pane's `paneFindReads` slot, one at a time in
// the order asked. A call is not a read of a result to apply later: the
// engine holds the session, so every call is made, and only its answer is
// dropped when a newer query superseded it. What a sync makes is the
// provider's current intent (start, steps, stop), read when it runs.

import {
  BrowserCompanionFind,
  BrowserFindAction,
  type BrowserCompanionEvent,
  type BrowserFindState,
} from './bindings';
import { paneFindReads, type PaneFindProvider, type PaneFindResult } from './paneFind.svelte';
import { errString } from '../utils/errors';

export const BROWSER_FIND_DEBOUNCE_MS = 150;

/** "bar": the shared find bar drives the engine. "engine": its own find UI. */
export type BrowserFindMode = 'bar' | 'engine';

export interface BrowserFindOptions {
  threadId: string;
  /** The browser companion's layout pane, which owns the find. */
  paneId: string;
  mode: BrowserFindMode;
  /** The page the pane shows now. */
  pageId: () => string;
  /** The backend ended the session; the pane closes its bar. */
  onEnded: () => void;
  /** A find request failed; the pane shows why. */
  onError: (message: string) => void;
}

interface Session {
  pageId: string;
  session: number;
}

interface LiveFind {
  ended(state: BrowserFindState): void;
}

const live = new Map<string, Set<LiveFind>>();

/** Routes a `find` companion event to the thread's providers. */
export function applyBrowserFindEvent(event: BrowserCompanionEvent): void {
  const state = event.find;
  if (!state || state.active) return;
  for (const find of live.get(event.threadId) ?? []) find.ended(state);
}

export type BrowserFind = PaneFindProvider & { dispose(): void };

export function createBrowserFind(options: BrowserFindOptions): BrowserFind {
  const { threadId, mode } = options;
  const reads = paneFindReads(options.paneId);
  const view = $state({ count: 0 as number | null, current: -1, pending: false, capped: false });
  // Moves on every query change and close; an answer must match it.
  let seq = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  // The intent a sync carries out: a start, then steps, then a stop.
  let startQuery: string | null = null;
  let steps = 0;
  let stopNeeded = false;
  // wanted is whether the reader still wants a session.
  let wanted = false;
  let syncing = false;
  // held is whether the engine may hold a session of this provider's: a
  // start was made on heldPage and no stop since. Steps and the stop go to
  // that page, which the pane may no longer show (a closed pane shows none).
  let held = false;
  let heldPage = '';
  let session: Session | null = null;
  let endedWhileSyncing: BrowserFindState | null = null;

  const result: PaneFindResult = {
    get count() { return view.count; },
    get current() { return view.current; },
    get pending() { return view.pending; },
    get capped() { return view.capped; },
  };

  function reset(): void {
    view.count = 0;
    view.current = -1;
    view.pending = false;
    view.capped = false;
  }

  function show(state: BrowserFindState): void {
    if (!state.active) {
      session = null;
      reset();
      return;
    }
    session = { pageId: state.pageId, session: state.session };
    view.count = !state.found ? 0 : state.matches < 0 ? null : state.matches;
    view.current = state.current;
    view.capped = state.capped;
    view.pending = false;
  }

  function cancelTimer(): void {
    if (timer !== null) clearTimeout(timer);
    timer = null;
  }

  function call(kind: string, pageId: string, query = ''): Promise<BrowserFindState> {
    return BrowserCompanionFind(threadId, new BrowserFindAction({ kind, pageId, query }));
  }

  function fail(mine: number, err: unknown): void {
    if (mine !== seq) return;
    session = null;
    reset();
    options.onError(errString(err));
  }

  async function sync(): Promise<void> {
    syncing = true;
    try {
      if (startQuery !== null) {
        const query = startQuery;
        const mine = seq;
        startQuery = null;
        steps = 0;
        held = true;
        heldPage = options.pageId();
        try {
          const answer = await call('start', heldPage, query);
          if (!answer.active && startQuery === null) held = false;
          if (mine === seq) show(answer);
          // A start the reader left before it answered opened a session
          // nobody wants.
          else if (answer.active && !wanted) stopNeeded = true;
        } catch (err) {
          fail(mine, err);
        }
      }
      while (steps !== 0 && session && wanted && !stopNeeded) {
        const back = steps < 0;
        steps += back ? 1 : -1;
        const mine = seq;
        try {
          const answer = await call(back ? 'previous' : 'next', heldPage);
          if (mine === seq) show(answer);
        } catch (err) {
          fail(mine, err);
        }
      }
      if (stopNeeded) {
        stopNeeded = false;
        held = false;
        try {
          await call('stop', heldPage);
        } catch (err) {
          options.onError(errString(err));
        }
      }
    } finally {
      syncing = false;
      settleEnded();
    }
  }

  function request(): void {
    reads.request(sync);
  }

  function endsSession(state: BrowserFindState): boolean {
    return session !== null && state.pageId === session.pageId && state.session > session.session;
  }

  function ended(state: BrowserFindState): void {
    if (syncing || startQuery !== null) {
      if (!endedWhileSyncing || state.session > endedWhileSyncing.session) endedWhileSyncing = state;
      return;
    }
    if (!endsSession(state)) return;
    seq += 1;
    cancelTimer();
    wanted = false;
    steps = 0;
    session = null;
    reset();
    options.onEnded();
  }

  function settleEnded(): void {
    if (syncing || startQuery !== null || !endedWhileSyncing) return;
    const state = endedWhileSyncing;
    endedWhileSyncing = null;
    ended(state);
  }

  /** Ends the session: the engine's highlight goes with it. */
  function end(): void {
    cancelTimer();
    seq += 1;
    wanted = false;
    steps = 0;
    // A start not made yet is dropped; one made is stopped.
    startQuery = null;
    session = null;
    reset();
    if (held) {
      stopNeeded = true;
      request();
    }
  }

  const handle: LiveFind = { ended };
  let threadFinds = live.get(threadId);
  if (!threadFinds) {
    threadFinds = new Set();
    live.set(threadId, threadFinds);
  }
  threadFinds.add(handle);

  function step(delta: 1 | -1): void {
    if (!session || view.pending || view.count === 0) return;
    steps += delta;
    request();
  }

  const provider: BrowserFind = {
    noun: 'page',
    result,
    search(query: string): void {
      if (mode === 'engine') return;
      if (!query.trim()) {
        end();
        return;
      }
      cancelTimer();
      seq += 1;
      wanted = true;
      steps = 0;
      stopNeeded = false;
      view.pending = true;
      timer = setTimeout(() => {
        timer = null;
        startQuery = query;
        request();
      }, BROWSER_FIND_DEBOUNCE_MS);
    },
    next: () => step(1),
    previous: () => step(-1),
    clear: end,
    dispose(): void {
      end();
      threadFinds?.delete(handle);
      if (threadFinds?.size === 0) live.delete(threadId);
    },
  };
  if (mode === 'engine') {
    provider.openNative = (seed: string) => {
      cancelTimer();
      seq += 1;
      wanted = true;
      stopNeeded = false;
      startQuery = seed;
      request();
    };
  }
  return provider;
}

export function resetBrowserFindForTest(): void {
  live.clear();
}
