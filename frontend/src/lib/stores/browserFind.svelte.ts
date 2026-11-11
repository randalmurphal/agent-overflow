// Find in the browser pane (docs/architecture/browser-tools.md, "Find"). The
// backend's engine searches the page the pane shows and the backend owns the
// page's find: AO's bar open on it, the search, and when they end. This
// provider drives it from the pane's find bar, or for an engine with its own
// find UI opens that instead. An end the backend decides on its own (the page
// committed another document, the tab changed, the page closed, the pane
// unmounted) arrives as a `find` companion event and closes the bar.
//
// A find's identity is its page and its open: the id this provider makes for
// each explicit open (the bar opened, the engine's find opened) and carries
// on every action of that open. The backend stores it with the pane mount as
// the find's owner and echoes it on every answer and end, and every
// acceptance here compares it: an answer or an end for an earlier open
// changes nothing of a later one, and a close for an earlier open never
// closes a later one. An open's find, once ended, never comes back, so an
// end for the held open is taken whenever it arrives.
//
// An answer for an action that changed nothing because its find is gone or
// another open's is marked Foreign: it acknowledges nothing of the asking
// open's.
//
// Every request is an intent object made when the reader acts and carried to
// its answer: open, start (its query), step (its direction), stop (the
// search) and close (the bar). Each records the page, mount and open it is
// for and the display generation (`seq`) it was made under. Intents run one
// at a time, in order, through the pane's `paneFindReads` slot; an answer is
// shown only if its intent's generation is still current. A queued close is
// cleanup the backend still needs, so nothing but its own run removes it.
//
// An action whose outcome is unknown (the call timed out, or the connection
// went while it was out) may have left the backend holding a bar or a search
// this provider does not know of: the open is ended here and its close is
// owed, retried with backoff until an answer arrives, through dispose too.
//
// State: `seq` (display generation), `queue` (intents), `held` (the open this
// provider is bound to: its page, mount and id, and whether the bar and a
// search are open there), and the debounce timer.
//
// | Event                          | Effect                                                    |
// |--------------------------------|-----------------------------------------------------------|
// | bar opened (`opened`)          | seq++; a find held on another page is closed first; a new |
// |                                | open on the shown page and mount; queue open; held.bar    |
// | query typed                    | seq++; drop queued starts and steps; with a bar held,     |
// |                                | debounce, then queue start(query) for it; held.search     |
// | query emptied                  | seq++; drop queued starts and steps; queue stop if a      |
// |                                | search is held                                            |
// | Enter / Shift+Enter            | queue a step, or cancel a queued opposite one             |
// | bar closed (`clear`, dispose)  | seq++; drop the queue but its closes; queue close if      |
// |                                | anything is held                                          |
// | engine find opened             | as bar opened, queueing start(seed) and holding a search  |
// | answer (not foreign)           | shown if its seq is current                               |
// | foreign answer to open, start, | the open's find is gone: taken as its end (bar closes)    |
// | step or stop of the held open  |                                                           |
// | foreign answer to a close      | nothing: the bar is closed already                        |
// | failed open of the held open   | shown as an error if its seq is current; bar closes       |
// | other failure                  | shown as an error if its seq is current; the backend's    |
// |                                | bar stays, and so does this one                           |
// | unknown outcome                | shown as an error if its seq is current; the held open    |
// |                                | ends here (bar closes); its close is owed and retried     |
// | end event for the held open    | seq++, drop the queue but its closes, nothing held,       |
// |                                | onEnded (the bar closes)                                  |

import {
  BrowserCompanionFind,
  BrowserFindAction,
  type BrowserCompanionEvent,
  type BrowserFindState,
} from './bindings';
import { paneFindReads, type PaneFindProvider, type PaneFindResult } from './paneFind.svelte';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { errString } from '../utils/errors';
import { randomId } from '../utils/randomId';

export const BROWSER_FIND_DEBOUNCE_MS = 150;
/** The first retry of an owed close; each next one waits twice as long. */
export const BROWSER_FIND_CLOSE_RETRY_MS = 500;
/** The longest wait between retries of an owed close. */
export const BROWSER_FIND_CLOSE_RETRY_MAX_MS = 15_000;

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
  /** The explicit open the intent belongs to. */
  open: string;
  query: string;
  /** The display generation the intent was made under. */
  seq: number;
  /** How many times an owed close was sent without an answer. */
  attempt: number;
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

/**
 * Whether a call's failure leaves unknown whether the backend ran it: a
 * timeout or a lost connection may come after the backend acted, and an
 * error that is not the transport's says nothing either. Any other
 * transport error is the backend's answer, or a refusal before it ran.
 */
function outcomeUnknown(err: unknown): boolean {
  if (err instanceof DisconnectedError) return true;
  if (err instanceof TransportError) return err.code === 'timeout';
  return true;
}

export type BrowserFind = PaneFindProvider & { dispose(): void };

export function createBrowserFind(options: BrowserFindOptions): BrowserFind {
  const { threadId, mode } = options;
  const reads = paneFindReads(options.paneId);
  const view = $state({ count: 0 as number | null, current: -1, pending: false, capped: false });
  let seq = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  const queue: Intent[] = [];
  const held = { pageId: '', mount: '', open: '', bar: false, search: false };
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
    queue.push({ kind, pageId: held.pageId, mount: held.mount, open: held.open, query, seq, attempt: 0 });
    reads.request(drain);
  }

  /** Queues the close of an open, unless one for it is queued already. */
  function enqueueClose(of: { pageId: string; mount: string; open: string }): void {
    if (queue.some((intent) => intent.kind === 'close' && intent.open === of.open)) return;
    queue.push({ kind: 'close', pageId: of.pageId, mount: of.mount, open: of.open, query: '', seq, attempt: 0 });
    reads.request(drain);
  }

  /** Sends an owed close again after its backoff. */
  function retryClose(intent: Intent): void {
    const wait = Math.min(BROWSER_FIND_CLOSE_RETRY_MS * 2 ** intent.attempt, BROWSER_FIND_CLOSE_RETRY_MAX_MS);
    setTimeout(() => {
      if (queue.some((queued) => queued.kind === 'close' && queued.open === intent.open)) return;
      queue.push({ ...intent, attempt: intent.attempt + 1 });
      reads.request(drain);
    }, wait);
  }

  /**
   * Binds the provider to a new open on the shown page and the pane's mount.
   * A find held on another page is closed first; one on this page the new
   * open takes over on the backend.
   */
  function holdShown(): void {
    const pageId = options.pageId();
    if ((held.bar || held.search) && held.pageId !== pageId) enqueueClose(held);
    held.bar = false;
    held.search = false;
    held.pageId = pageId;
    held.mount = options.mount() ?? '';
    held.open = randomId();
  }

  /** Whether an intent belongs to the open the provider holds a find of. */
  function holds(intent: Intent): boolean {
    return intent.open === held.open && (held.bar || held.search);
  }

  function call(intent: Intent): Promise<BrowserFindState> {
    return BrowserCompanionFind(threadId, new BrowserFindAction({
      kind: intent.kind, pageId: intent.pageId, query: intent.query, mount: intent.mount, open: intent.open,
    }));
  }

  async function drain(): Promise<void> {
    while (queue.length > 0) {
      const intent = queue.shift()!;
      try {
        const answer = await call(intent);
        if (answer.foreign) {
          // The open's find is gone, or another open's: nothing of it is
          // left to show, and a close needs nothing more.
          if (intent.kind !== 'close' && holds(intent)) accept();
        } else if (intent.seq === seq && (intent.kind === 'start' || intent.kind === 'next' || intent.kind === 'previous')) {
          show(answer);
        }
      } catch (err) {
        if (intent.seq === seq) {
          reset();
          options.onError(errString(err));
        }
        if (outcomeUnknown(err)) {
          if (intent.kind === 'close') {
            retryClose(intent);
          } else {
            if (holds(intent)) accept();
            enqueueClose(intent);
          }
        } else if (intent.kind === 'open' && holds(intent)) {
          // A bar the backend would not open is not shown.
          accept();
        }
      }
    }
  }

  function ended(state: BrowserFindState): void {
    if ((held.bar || held.search) && state.open === held.open) accept();
  }

  /** The held open's find ended: nothing is held, and nothing to stop. */
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
    reset();
    if (held.bar || held.search) enqueueClose(held);
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
      // A query searches in the bar of the held open, and nowhere else.
      if (!held.bar) {
        reset();
        return;
      }
      view.pending = true;
      timer = setTimeout(() => {
        timer = null;
        if (!held.bar) return;
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
