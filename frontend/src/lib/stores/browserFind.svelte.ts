// Find in the browser pane (docs/architecture/browser-tools.md, "Find"). The
// backend's engine searches the page the pane shows; the backend holds what
// the engine holds (whether a search runs, and its owner) and decides when it
// ends. Whether the bar is open is the pane-find store's alone. This provider
// drives the search from the pane's find bar, or for an engine with its own
// find UI opens that instead. An end the backend decides on its own (the page
// committed another document, the tab changed, another bar's start took the
// page over, the pane unmounted) arrives as a `find` companion event and
// closes the bar. A bare Escape pressed in the page stays the page's; the
// backend announces it as an `escape` companion event, and the provider
// closes its bar if it is open on that page.
//
// A find's identity is its page, its pane mount and its find id: the id this
// provider makes the first time a bar's query searches (after the bar opened,
// or the engine's find opened) and carries on every action of that bar. The
// backend echoes all three on every answer and end. An end applies to the
// held search only if it names its page, mount and id and is newer (its
// `session`) than every answer taken for it, so a late end of an earlier
// search never closes the bar over a later one.
//
// An answer for an action that changed nothing because its search is gone or
// another find's is marked Foreign: it acknowledges nothing of the asking
// find's.
//
// Every request is an intent made when the reader acts and carried to its
// answer: start (its query), next, previous or stop. Each records the page,
// mount and find id it is for and the display generation (`seq`) it was made
// under. Intents run one at a time, in order, on this provider's own queue;
// an answer is shown only if its intent's generation is still current. A
// queued stop is cleanup the backend still needs, so nothing but its own run
// removes it.
//
// A stop that fails is reported and sent again on this provider's own timer,
// at most BROWSER_FIND_STOP_ATTEMPTS times in all, until one answers; a new
// start of that find clears the retry waiting for it. A stop that never
// succeeds leaves the page's highlights until the next start, navigation,
// page switch or pane unmount, each of which ends the engine's search.
//
// State: `seq` (display generation), `queue` (intents), `held` (the search
// this provider is bound to: its page, mount, id, whether a search is held,
// and the newest session answered for it), the debounce timer and the stop
// retry timers.
//
// | Event                          | Effect                                                    |
// |--------------------------------|-----------------------------------------------------------|
// | query typed                    | seq++; drop queued starts and steps; debounce, then bind  |
// |                                | the shown page and mount, a find id if none, and queue    |
// |                                | start(query); held.search                                 |
// | query emptied                  | seq++; drop queued starts and steps; queue stop if a      |
// |                                | search is held                                            |
// | Enter / Shift+Enter            | queue a step, or cancel a queued opposite one             |
// | bar closed (`clear`, dispose)  | seq++; drop queued starts and steps; queue stop if a      |
// |                                | search is held; the find id goes                          |
// | engine find opened             | a new find id; start(seed) as for a typed query           |
// | answer for the held search     | shown if its seq is current                               |
// | active answer for a search no  | queue its stop: nothing shows it                          |
// | longer held                    |                                                           |
// | foreign answer to start/step   | the search is gone: taken as its end (bar closes)         |
// | of the held search             |                                                           |
// | foreign answer to a stop       | nothing: the search is gone already                       |
// | failed start or step           | shown as an error if its seq is current, when the held    |
// |                                | search ends here; an unknown outcome queues its stop      |
// | failed stop                    | shown as an error; sent again after the retry wait while  |
// |                                | attempts remain                                           |
// | end event for the held search  | seq++, drop queued starts and steps, nothing held, the    |
// |                                | bar closes                                                |
// | escape event for the shown     | the bar closes, if it is open                             |
// | page                           |                                                           |

import {
  BrowserCompanionFind,
  BrowserFindAction,
  type BrowserCompanionEvent,
  type BrowserFindState,
} from './bindings';
import { isPaneFindOpen, type PaneFindProvider, type PaneFindResult } from './paneFind.svelte';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { errString } from '../utils/errors';
import { randomId } from '../utils/randomId';

export const BROWSER_FIND_DEBOUNCE_MS = 150;
/** The wait before a failed stop is sent again. */
export const BROWSER_FIND_STOP_RETRY_MS = 2_000;
/** How many times a stop is sent at most, the first time included. */
export const BROWSER_FIND_STOP_ATTEMPTS = 3;

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
  /** Closes the pane's bar, as `find.close` does. */
  closeBar: () => void;
  /** A find request failed; the pane shows why. */
  onError: (message: string) => void;
}

type IntentKind = 'start' | 'next' | 'previous' | 'stop';

interface Intent {
  kind: IntentKind;
  pageId: string;
  /** The pane mount the find belongs to. */
  mount: string;
  /** The find id the intent belongs to. */
  id: string;
  query: string;
  /** The display generation the intent was made under. */
  seq: number;
  /** How many times this stop was sent before. */
  attempt: number;
}

interface LiveFind {
  ended(state: BrowserFindState): void;
  escaped(pageId: string): void;
}

const live = new Map<string, Set<LiveFind>>();

/** Routes a `find` or `escape` companion event to the thread's providers. */
export function applyBrowserFindEvent(event: BrowserCompanionEvent): void {
  const finds = live.get(event.threadId);
  if (!finds) return;
  if (event.kind === 'escape') {
    for (const find of finds) find.escaped(event.pageId ?? '');
    return;
  }
  const state = event.find;
  if (!state || state.active) return;
  for (const find of finds) find.ended(state);
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

function retryKey(of: { pageId: string; id: string }): string {
  return `${of.pageId}\n${of.id}`;
}

export type BrowserFind = PaneFindProvider & { dispose(): void };

export function createBrowserFind(options: BrowserFindOptions): BrowserFind {
  const { threadId, mode } = options;
  const view = $state({ count: 0 as number | null, current: -1, pending: false, capped: false });
  let seq = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  const queue: Intent[] = [];
  let running = false;
  const held = { pageId: '', mount: '', id: '', search: false, session: 0 };
  const retries = new Map<string, ReturnType<typeof setTimeout>>();
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

  function cancelRetry(of: { pageId: string; id: string }): void {
    const key = retryKey(of);
    const waiting = retries.get(key);
    if (waiting === undefined) return;
    clearTimeout(waiting);
    retries.delete(key);
  }

  /** Drops queued intents of the given kinds; the one in flight runs on. */
  function drop(kinds: readonly IntentKind[]): void {
    for (let i = queue.length - 1; i >= 0; i--) {
      if (kinds.includes(queue[i].kind)) queue.splice(i, 1);
    }
  }

  function enqueue(intent: Intent): void {
    queue.push(intent);
    if (!running) void drain();
  }

  function heldIntent(kind: IntentKind, query = ''): Intent {
    return { kind, pageId: held.pageId, mount: held.mount, id: held.id, query, seq, attempt: 0 };
  }

  /** Queues the stop of a find, unless one for it is queued already. */
  function enqueueStop(of: Intent): void {
    if (queue.some((intent) => intent.kind === 'stop' && intent.pageId === of.pageId && intent.id === of.id)) return;
    enqueue({ ...of, kind: 'stop', query: '', seq, attempt: 0 });
  }

  /** Sends a failed stop again after the retry wait, while attempts remain. */
  function retryStop(intent: Intent): void {
    cancelRetry(intent);
    if (intent.attempt + 1 >= BROWSER_FIND_STOP_ATTEMPTS) return;
    const key = retryKey(intent);
    retries.set(key, setTimeout(() => {
      retries.delete(key);
      enqueue({ ...intent, seq, attempt: intent.attempt + 1 });
    }, BROWSER_FIND_STOP_RETRY_MS));
  }

  /** Whether an intent belongs to the search the provider holds. */
  function isHeld(intent: Intent): boolean {
    return held.search && intent.pageId === held.pageId && intent.mount === held.mount && intent.id === held.id;
  }

  function call(intent: Intent): Promise<BrowserFindState> {
    return BrowserCompanionFind(threadId, new BrowserFindAction({
      kind: intent.kind, pageId: intent.pageId, query: intent.query, mount: intent.mount, findId: intent.id,
    }));
  }

  async function drain(): Promise<void> {
    running = true;
    try {
      while (queue.length > 0) {
        const intent = queue.shift()!;
        try {
          const answer = await call(intent);
          if (intent.kind === 'stop') {
            // Answered: no retry follows, and a foreign answer is a search
            // already gone.
          } else if (answer.foreign) {
            // The search is gone, or another find's: nothing of it is left
            // to show.
            if (isHeld(intent)) accept();
          } else if (!isHeld(intent)) {
            // A search nothing here shows any more.
            if (answer.active) enqueueStop(intent);
          } else {
            held.session = Math.max(held.session, answer.session);
            if (intent.seq === seq) show(answer);
          }
        } catch (err) {
          if (intent.seq === seq) reset();
          if (intent.kind === 'stop') {
            options.onError(errString(err));
            retryStop(intent);
            continue;
          }
          if (intent.seq === seq) {
            options.onError(errString(err));
            // The backend ended its search, or may have: nothing is held.
            if (isHeld(intent)) {
              held.search = false;
              drop(['next', 'previous']);
            }
          }
          if (outcomeUnknown(err) && !isHeld(intent)) enqueueStop(intent);
        }
      }
    } finally {
      running = false;
    }
  }

  function ended(state: BrowserFindState): void {
    if (!held.search || state.pageId !== held.pageId || state.mount !== held.mount || state.findId !== held.id) return;
    if (state.session <= held.session) return;
    accept();
  }

  /**
   * A bare Escape in a page: the bar closes. Only a "bar" provider has one;
   * an engine's own find UI keeps the Escape, whatever the store kept open
   * from a provider before it.
   */
  function escaped(pageId: string): void {
    if (mode !== 'bar' || pageId !== options.pageId()) return;
    if (isPaneFindOpen(options.paneId)) options.closeBar();
  }

  /** The held search ended: nothing is held, nothing to stop, the bar closes. */
  function accept(): void {
    seq += 1;
    cancelTimer();
    drop(['start', 'next', 'previous']);
    held.search = false;
    held.id = '';
    reset();
    options.closeBar();
  }

  /** The bar closed: the backend's search goes with it. */
  function close(): void {
    seq += 1;
    cancelTimer();
    drop(['start', 'next', 'previous']);
    reset();
    if (held.search) enqueueStop(heldIntent('stop'));
    held.search = false;
    held.id = '';
  }

  /** Binds the shown page and mount and starts the query there. */
  function begin(query: string): void {
    held.pageId = options.pageId();
    held.mount = options.mount() ?? '';
    if (!held.id) {
      held.id = randomId();
      held.session = 0;
    }
    held.search = true;
    cancelRetry(held);
    enqueue(heldIntent('start', query));
  }

  const handle: LiveFind = { ended, escaped };
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
    enqueue(heldIntent(kind));
  }

  const provider: BrowserFind = {
    noun: 'page',
    result,
    search(query: string): void {
      if (mode === 'engine' || disposed) return;
      cancelTimer();
      seq += 1;
      drop(['start', 'next', 'previous']);
      if (!query.trim()) {
        reset();
        if (held.search) enqueueStop(heldIntent('stop'));
        held.search = false;
        return;
      }
      view.pending = true;
      timer = setTimeout(() => {
        timer = null;
        begin(query);
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
      // Each open of the engine's find is a find of its own.
      held.id = '';
      begin(seed);
    };
  }
  return provider;
}

export function resetBrowserFindForTest(): void {
  live.clear();
}
