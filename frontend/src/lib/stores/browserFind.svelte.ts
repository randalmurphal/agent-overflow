// Find in the browser pane (docs/architecture/browser-tools.md, "Find"). The
// backend's engine searches the page the pane shows and the backend owns the
// page's find: AO's bar open on it, the search, and when they end. This
// provider drives it from the pane's find bar, or for an engine with its own
// find UI opens that instead. An end the backend decides on its own (the page
// committed another document, the tab changed, the page closed, the pane
// unmounted) arrives as a `find` companion event and closes the bar.
//
// Every request is an intent object made when the reader acts and carried to
// its answer: open, start (its query), step (its direction), stop (the
// search) and close (the bar). Each records the page it is for and the
// display generation (`seq`) it was made under. Intents run one at a time,
// in order, through the pane's `paneFindReads` slot; an answer is shown only
// if its intent's generation is still current.
//
// State: `seq` (display generation), `queue` and `inFlight` (intents),
// `held` (what the backend holds for this provider: its page, whether the
// bar and a search are open there, and `known`, the newest backend
// generation any answer reported), `deferredEnd`, and the debounce timer.
//
// | Event                          | Effect                                                    |
// |--------------------------------|-----------------------------------------------------------|
// | bar opened (`opened`)          | seq++; queue open for the shown page; held.bar            |
// | query typed                    | seq++; drop queued starts and steps; debounce, then queue |
// |                                | start(query) for the shown page; held.search              |
// | query emptied                  | seq++; drop queued starts and steps; queue stop if a      |
// |                                | search is held                                            |
// | Enter / Shift+Enter            | queue a step, or cancel a queued opposite one             |
// | bar closed (`clear`, dispose)  | seq++; drop the queue; queue close if anything is held    |
// | engine find opened             | seq++; drop the queue; queue start(seed); held.search     |
// | answer                         | known = max(known, session); shown if its seq is current  |
// | failure                        | shown as an error if its seq is current                   |
// | end event, newer than `known`  | in flight: kept as deferredEnd; else seq++, drop the      |
// | for the held page              | queue, nothing held, onEnded (the bar closes)             |
// | in-flight intent completes     | a deferredEnd newer than `known` is accepted as above     |

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
  /** The backend's id of the pane mount the bar shows in, null before it has one. */
  mount: () => string | null;
  /** The backend ended the find; the pane closes its bar. */
  onEnded: () => void;
  /** A find request failed; the pane shows why. */
  onError: (message: string) => void;
}

type IntentKind = 'open' | 'start' | 'next' | 'previous' | 'stop' | 'close';

interface Intent {
  kind: IntentKind;
  pageId: string;
  query: string;
  /** The display generation the intent was made under. */
  seq: number;
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
  let seq = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  const queue: Intent[] = [];
  let inFlight: Intent | null = null;
  const held = { pageId: '', bar: false, search: false, known: 0 };
  let deferredEnd: BrowserFindState | null = null;
  let disposed = false;

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
      reset();
      return;
    }
    view.count = !state.found ? 0 : state.matches < 0 ? null : state.matches;
    view.current = state.current;
    view.capped = state.capped;
    view.pending = false;
  }

  function cancelTimer(): void {
    if (timer !== null) clearTimeout(timer);
    timer = null;
  }

  /** Drops queued intents of the given kinds; the one in flight runs on. */
  function drop(kinds: readonly IntentKind[]): void {
    for (let i = queue.length - 1; i >= 0; i--) {
      if (kinds.includes(queue[i].kind)) queue.splice(i, 1);
    }
  }

  function enqueue(kind: IntentKind, pageId: string, query = ''): void {
    queue.push({ kind, pageId, query, seq });
    reads.request(drain);
  }

  function holdPage(pageId: string): void {
    if (held.pageId === pageId) return;
    held.pageId = pageId;
    held.known = 0;
  }

  function call(intent: Intent): Promise<BrowserFindState> {
    const mount = intent.kind === 'open' ? options.mount() ?? '' : '';
    return BrowserCompanionFind(threadId, new BrowserFindAction({
      kind: intent.kind, pageId: intent.pageId, query: intent.query, mount,
    }));
  }

  async function drain(): Promise<void> {
    while (queue.length > 0) {
      const intent = queue.shift()!;
      inFlight = intent;
      try {
        const answer = await call(intent);
        if (answer.pageId === held.pageId) held.known = Math.max(held.known, answer.session);
        if (intent.seq === seq && (intent.kind === 'start' || intent.kind === 'next' || intent.kind === 'previous')) {
          show(answer);
        }
      } catch (err) {
        if (intent.seq === seq) {
          reset();
          options.onError(errString(err));
        }
      } finally {
        inFlight = null;
      }
      settleDeferredEnd();
    }
  }

  function endsHeld(state: BrowserFindState): boolean {
    return (held.bar || held.search) && state.pageId === held.pageId && state.session > held.known;
  }

  function ended(state: BrowserFindState): void {
    if (!endsHeld(state)) return;
    if (inFlight) {
      if (!deferredEnd || state.session > deferredEnd.session) deferredEnd = state;
      return;
    }
    accept();
  }

  function settleDeferredEnd(): void {
    const state = deferredEnd;
    deferredEnd = null;
    if (state && endsHeld(state)) accept();
  }

  /** The backend ended the find: nothing is held, and nothing to stop. */
  function accept(): void {
    seq += 1;
    cancelTimer();
    queue.length = 0;
    held.bar = false;
    held.search = false;
    reset();
    options.onEnded();
  }

  /** The bar closed: the backend's bar and search go with it. */
  function close(): void {
    seq += 1;
    cancelTimer();
    queue.length = 0;
    deferredEnd = null;
    reset();
    if (held.bar || held.search) enqueue('close', held.pageId);
    held.bar = false;
    held.search = false;
  }

  const handle: LiveFind = { ended };
  let threadFinds = live.get(threadId);
  if (!threadFinds) {
    threadFinds = new Set();
    live.set(threadId, threadFinds);
  }
  threadFinds.add(handle);

  function step(kind: 'next' | 'previous'): void {
    if (!held.search || view.pending || view.count === 0) return;
    const opposite = kind === 'next' ? 'previous' : 'next';
    const last = queue[queue.length - 1];
    if (last?.kind === opposite) {
      queue.pop();
      return;
    }
    enqueue(kind, held.pageId);
  }

  const provider: BrowserFind = {
    noun: 'page',
    result,
    opened(): void {
      if (mode === 'engine' || disposed) return;
      seq += 1;
      reset();
      const pageId = options.pageId();
      holdPage(pageId);
      held.bar = true;
      enqueue('open', pageId);
    },
    search(query: string): void {
      if (mode === 'engine' || disposed) return;
      cancelTimer();
      seq += 1;
      drop(['start', 'next', 'previous']);
      if (!query.trim()) {
        reset();
        if (held.search) enqueue('stop', held.pageId);
        held.search = false;
        return;
      }
      view.pending = true;
      timer = setTimeout(() => {
        timer = null;
        const pageId = options.pageId();
        holdPage(pageId);
        held.search = true;
        enqueue('start', pageId, query);
      }, BROWSER_FIND_DEBOUNCE_MS);
    },
    next: () => step('next'),
    previous: () => step('previous'),
    clear: close,
    dispose(): void {
      close();
      disposed = true;
      threadFinds?.delete(handle);
      if (threadFinds?.size === 0) live.delete(threadId);
    },
  };
  if (mode === 'engine') {
    provider.openNative = (seed: string) => {
      if (disposed) return;
      cancelTimer();
      seq += 1;
      drop(['start']);
      const pageId = options.pageId();
      holdPage(pageId);
      held.search = true;
      enqueue('start', pageId, seed);
    };
  }
  return provider;
}

export function resetBrowserFindForTest(): void {
  live.clear();
}
