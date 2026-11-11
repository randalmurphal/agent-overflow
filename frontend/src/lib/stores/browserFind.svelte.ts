// Find in the browser pane (docs/architecture/browser-tools.md, "Find"). The
// backend's engine searches the page the pane shows and the backend owns the
// page's find: AO's bar open on it, the search, and when they end. This
// provider drives it from the pane's find bar, or for an engine with its own
// find UI opens that instead. An end the backend decides on its own (the page
// committed another document, the tab changed, the page closed, the pane
// unmounted) arrives as a `find` companion event and closes the bar.
//
// A find's identity is its page (id and incarnation), the pane mount that
// owns it, the document it was opened on, and a generation. Every action,
// answer and end carries all four, and an end, a close or a claim applies
// only to the find whose identity it names: another mount's close, a
// commit older than the find's document, an answer older than an end, or a
// page that is not the same incarnation leaves it alone.
//
// An answer for an action that changed nothing because its find is gone or
// another mount's is marked Foreign and names the find's real owner, "" for
// none: it acknowledges nothing of the asking find's.
//
// Every request is an intent object made when the reader acts and carried to
// its answer: open, start (its query), step (its direction), stop (the
// search) and close (the bar). Each records the page and the mount it is
// for and the display generation (`seq`) it was made under. Intents run one
// at a time, in order, through the pane's `paneFindReads` slot; an answer is
// shown only if its intent's generation is still current. A queued close is
// cleanup the backend still needs, so nothing but its own run removes it.
//
// State: `seq` (display generation), `queue` and `inFlight` (intents),
// `held` (what the backend holds for this provider: its page, mount and the
// page's incarnation, whether the bar and a search are open there, and
// `known`, the newest backend generation an answer of that incarnation
// reported), `deferredEnd`, and the debounce timer.
//
// | Event                          | Effect                                                    |
// |--------------------------------|-----------------------------------------------------------|
// | bar opened (`opened`)          | seq++; queue open for the shown page; held.bar            |
// | query typed                    | seq++; drop queued starts and steps; debounce, then queue |
// |                                | start(query) for the shown page; held.search              |
// | query emptied                  | seq++; drop queued starts and steps; queue stop if a      |
// |                                | search is held                                            |
// | Enter / Shift+Enter            | queue a step, or cancel a queued opposite one             |
// | bar closed (`clear`, dispose)  | seq++; drop the queue but its closes; queue close if      |
// |                                | anything is held                                          |
// | engine find opened             | seq++; drop the queue; queue start(seed); held.search     |
// | answer for the held mount,     | known = max(known, session), reset by a newer incarnation;|
// | not foreign                    |                                                           |
// |                                | shown if its seq is current                               |
// | failure                        | shown as an error if its seq is current                   |
// | end event for the held page    | in flight: kept as deferredEnd; else seq++, drop the      |
// | and mount, newer than `known`  | queue but its closes, nothing held, onEnded (bar closes)  |
// | or a newer incarnation         |                                                           |
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
  /** The pane mount the find belongs to. */
  mount: string;
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
  const held = { pageId: '', mount: '', incarnation: 0, bar: false, search: false, known: 0 };
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

  function enqueue(kind: IntentKind, query = ''): void {
    queue.push({ kind, pageId: held.pageId, mount: held.mount, query, seq });
    reads.request(drain);
  }

  /** Queues the close of what is held, unless one for it is queued already. */
  function enqueueClose(): void {
    const queued = queue.some((intent) => intent.kind === 'close' && intent.pageId === held.pageId && intent.mount === held.mount);
    if (!queued) enqueue('close');
  }

  /** Holds the shown page for the pane's mount, ahead of an open or a start. */
  function holdShown(): void {
    const pageId = options.pageId();
    const mount = options.mount() ?? '';
    if (held.pageId === pageId && held.mount === mount) return;
    held.pageId = pageId;
    held.mount = mount;
    held.incarnation = 0;
    held.known = 0;
  }

  /** Whether an answer or end is for the held page and mount. */
  function forHeld(state: BrowserFindState): boolean {
    return state.pageId === held.pageId && state.mount === held.mount;
  }

  /** Records the generation an answer reports for the held find. */
  function note(state: BrowserFindState): void {
    if (state.foreign || !forHeld(state) || state.incarnation < held.incarnation) return;
    if (state.incarnation > held.incarnation) {
      held.incarnation = state.incarnation;
      held.known = state.session;
      return;
    }
    held.known = Math.max(held.known, state.session);
  }

  function call(intent: Intent): Promise<BrowserFindState> {
    return BrowserCompanionFind(threadId, new BrowserFindAction({
      kind: intent.kind, pageId: intent.pageId, query: intent.query, mount: intent.mount,
    }));
  }

  async function drain(): Promise<void> {
    while (queue.length > 0) {
      const intent = queue.shift()!;
      inFlight = intent;
      try {
        const answer = await call(intent);
        note(answer);
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
    if (!(held.bar || held.search) || !forHeld(state)) return false;
    if (state.incarnation !== held.incarnation) return state.incarnation > held.incarnation;
    return state.session > held.known;
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
    drop(['open', 'start', 'next', 'previous', 'stop']);
    held.bar = false;
    held.search = false;
    reset();
    options.onEnded();
  }

  /** The bar closed: the backend's bar and search go with it. */
  function close(): void {
    seq += 1;
    cancelTimer();
    drop(['open', 'start', 'next', 'previous', 'stop']);
    deferredEnd = null;
    reset();
    if (held.bar || held.search) enqueueClose();
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
    enqueue(kind);
  }

  const provider: BrowserFind = {
    noun: 'page',
    result,
    opened(): void {
      if (mode === 'engine' || disposed) return;
      seq += 1;
      reset();
      holdShown();
      held.bar = true;
      enqueue('open');
    },
    search(query: string): void {
      if (mode === 'engine' || disposed) return;
      cancelTimer();
      seq += 1;
      drop(['start', 'next', 'previous']);
      if (!query.trim()) {
        reset();
        if (held.search) enqueue('stop');
        held.search = false;
        return;
      }
      view.pending = true;
      timer = setTimeout(() => {
        timer = null;
        holdShown();
        held.search = true;
        enqueue('start', query);
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
      holdShown();
      held.search = true;
      enqueue('start', seed);
    };
  }
  return provider;
}

export function resetBrowserFindForTest(): void {
  live.clear();
}
