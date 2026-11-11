// Find in the browser pane (docs/architecture/browser-tools.md, "Find"). The
// backend's engine searches the page the pane shows; the backend holds what
// the engine holds (whether a search runs, and its owner) and decides when it
// ends. Whether the bar is open is the pane-find store's alone. This provider
// drives the search from the pane's find bar, or for an engine with its own
// find UI opens that instead. An end the backend decides on its own (the page
// committed another document, the tab changed, another pane's start took the
// page over, the pane unmounted) arrives as a `find` companion event and
// closes the bar. A bare Escape pressed in the page stays the page's; the
// backend announces it as an `escape` companion event, and the provider
// closes its bar if it is open on that page.
//
// A search's identity is its pane mount and its sequence: every start takes
// the next number of one counter for the frontend's lifetime, so a mount's
// starts only ever increase, and the backend refuses one that does not. A
// step names the sequence it steps. A stop names the highest sequence issued
// when it is made, whether that start was sent, answered, refused or dropped,
// and the backend ends the mount's search exactly when its sequence is at or
// below it. So a stop never ends a later search, and closing the bar always
// ends whatever the mount searched before, without the provider knowing
// which search the backend holds.
//
// The bar shows one search: `showing`, its sequence, 0 for none. An answer or
// failure changes what the bar shows only if it is for that sequence. A
// failed start or step of it is its result: the bar shows the error
// (`result.error`) until the next query or the bar's close. An answer for an
// action that changed nothing because its search is gone or another find's
// is marked Foreign: for the shown search it is that search's end.
//
// Every request is an intent made when the reader acts and carried to its
// answer: start (its query), next, previous or stop. Each records the page
// and mount it is for and its sequence. Intents run one at a time, in order,
// on this provider's own queue. A queued stop is cleanup the backend still
// needs, so nothing but its own run removes it.
//
// A stop that fails never changes what the bar shows, and is sent again on
// this provider's own timer, at most BROWSER_FIND_STOP_ATTEMPTS times in
// all, until the backend answers it, or a later stop through it or further
// is answered. Its failure outlives the bar, since the page may keep its
// highlights, so it goes to the pane (`onCleanupError`) and is withdrawn
// once a stop through it or further is answered. A stop that never succeeds
// leaves the page's highlights until the next start, navigation, page switch
// or pane unmount, each of which ends the engine's search; an unmounted pane
// shows nothing, so a dispose withdraws the failure.
//
// State: `showing`, `issued` (the highest sequence this provider made),
// `stoppedThrough` (the highest a stop was made through), `bound` (the page
// and mount of the newest start), `queue` (intents), `undelivered` (per
// mount, the highest bound of a stop that failed and is not yet covered by
// an answered one), the debounce timer and the stop retry timers.
//
// | Event                          | Effect                                                    |
// |--------------------------------|-----------------------------------------------------------|
// | query typed                    | showing = 0; drop queued starts and steps; debounce, then |
// |                                | bind the shown page and mount, issue the next sequence,   |
// |                                | show it, and queue start(query)                           |
// | query emptied, bar closed      | showing = 0; drop queued starts and steps; queue stop     |
// | (`clear`, dispose)             | through `issued` unless one was made since                |
// | Enter / Shift+Enter            | queue a step of `showing`, or cancel a queued opposite one|
// | engine find opened             | start(seed) as for a typed query                          |
// | answer for `showing`           | shown; Foreign: the search ended, the bar closes          |
// | answer for another sequence    | nothing                                                   |
// | failed start or step           | for `showing`: the bar shows its error (engine mode: a    |
// |                                | toast); always: queue a stop through its sequence         |
// | answered stop                  | retries through it or below are dropped; the pane's       |
// |                                | cleanup error goes once nothing undelivered is left       |
// | failed stop                    | the pane shows the cleanup error; sent again after the    |
// |                                | retry wait while attempts remain                          |
// | end event for `showing`        | showing = 0, drop queued starts and steps, the bar closes |
// | escape event for the shown     | the bar closes, if it is open                             |
// | page                           |                                                           |

import {
  BrowserCompanionFind,
  BrowserFindAction,
  type BrowserCompanionEvent,
  type BrowserFindState,
} from './bindings';
import { isPaneFindOpen, type PaneFindProvider, type PaneFindResult } from './paneFind.svelte';
import { errString } from '../utils/errors';
import { addErrorToast } from './toast.svelte';

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
  /**
   * A stop could not be delivered, so the page may keep its highlights: the
   * pane shows why. null withdraws it, once a later stop is answered.
   */
  onCleanupError: (message: string | null) => void;
}

type IntentKind = 'start' | 'next' | 'previous' | 'stop';

interface Intent {
  kind: IntentKind;
  pageId: string;
  /** The pane mount the find belongs to. */
  mount: string;
  /** A start's sequence, the sequence a step steps, or a stop's bound. */
  seq: number;
  query: string;
  /** How many times this stop was sent before. */
  attempt: number;
}

interface LiveFind {
  ended(state: BrowserFindState): void;
  escaped(pageId: string): void;
}

const live = new Map<string, Set<LiveFind>>();

/**
 * The sequence of the newest start any provider made. One counter for the
 * frontend's lifetime, so a pane mount's starts increase across the
 * providers it has.
 */
let lastSequence = 0;

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

export type BrowserFind = PaneFindProvider & { dispose(): void };

export function createBrowserFind(options: BrowserFindOptions): BrowserFind {
  const { threadId, mode } = options;
  const view = $state({ count: 0 as number | null, current: -1, pending: false, capped: false, error: null as string | null });
  let showing = 0;
  let issued = 0;
  let stoppedThrough = 0;
  const bound = { pageId: '', mount: '' };
  let timer: ReturnType<typeof setTimeout> | null = null;
  const queue: Intent[] = [];
  let running = false;
  const retries: Array<{ intent: Intent; timer: ReturnType<typeof setTimeout> }> = [];
  const undelivered = new Map<string, number>();
  let disposed = false;

  const result: PaneFindResult = {
    get count() { return view.count; },
    get current() { return view.current; },
    get pending() { return view.pending; },
    get capped() { return view.capped; },
    get error() { return view.error; },
  };

  function reset(): void {
    view.count = 0;
    view.current = -1;
    view.pending = false;
    view.capped = false;
    view.error = null;
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

  function enqueue(intent: Intent): void {
    queue.push(intent);
    if (!running) void drain();
  }

  /**
   * Queues a stop of the mount's search through a bound, unless one through
   * it or further is queued already. A find with no mount started nothing.
   */
  function enqueueStop(of: { pageId: string; mount: string }, through: number): void {
    stoppedThrough = Math.max(stoppedThrough, through);
    if (!of.mount) return;
    if (queue.some((intent) => intent.kind === 'stop' && intent.mount === of.mount && intent.seq >= through)) return;
    enqueue({ kind: 'stop', pageId: of.pageId, mount: of.mount, seq: through, query: '', attempt: 0 });
  }

  /** Stops every search this provider started, unless a stop was made since. */
  function stopIssued(): void {
    if (issued > stoppedThrough) enqueueStop(bound, issued);
  }

  /** Sends a failed stop again after the retry wait, while attempts remain. */
  function retryStop(intent: Intent): void {
    if (intent.attempt + 1 >= BROWSER_FIND_STOP_ATTEMPTS) return;
    const entry = {
      intent,
      timer: setTimeout(() => {
        retries.splice(retries.indexOf(entry), 1);
        enqueue({ ...intent, attempt: intent.attempt + 1 });
      }, BROWSER_FIND_STOP_RETRY_MS),
    };
    retries.push(entry);
  }

  /** A stop failed: its bound stays undelivered until one through it answers. */
  function noteUndelivered(stop: Intent, err: unknown): void {
    undelivered.set(stop.mount, Math.max(undelivered.get(stop.mount) ?? 0, stop.seq));
    if (!disposed) options.onCleanupError(errString(err));
  }

  /** An answered stop delivers every failed one through it or below. */
  function settleUndelivered(stop: Intent): void {
    const owed = undelivered.get(stop.mount);
    if (owed === undefined || owed > stop.seq) return;
    undelivered.delete(stop.mount);
    if (undelivered.size === 0 && !disposed) options.onCleanupError(null);
  }

  /** An answered stop: retries it makes needless, through it or below, go. */
  function settleRetries(stop: Intent): void {
    for (let i = retries.length - 1; i >= 0; i--) {
      const { intent, timer: waiting } = retries[i];
      if (intent.mount !== stop.mount || intent.seq > stop.seq) continue;
      clearTimeout(waiting);
      retries.splice(i, 1);
    }
  }

  function call(intent: Intent): Promise<BrowserFindState> {
    return BrowserCompanionFind(threadId, new BrowserFindAction({
      kind: intent.kind, pageId: intent.pageId, query: intent.query, mount: intent.mount, seq: intent.seq,
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
            settleRetries(intent);
            settleUndelivered(intent);
          } else if (intent.seq !== showing) {
            // A search the bar no longer shows.
          } else if (answer.foreign) {
            // The shown search is gone: nothing of it is left to show.
            accept();
          } else {
            show(answer);
          }
        } catch (err) {
          if (intent.kind === 'stop') {
            noteUndelivered(intent, err);
            retryStop(intent);
            continue;
          }
          if (intent.seq === showing) {
            reset();
            showing = 0;
            // The engine's own UI has no place for it: a toast says it.
            if (mode === 'engine') addErrorToast('Find could not open in this page', err);
            else view.error = errString(err);
            drop(['next', 'previous']);
          }
          // The backend ended the search, refused it, or may hold it: the
          // stop through it settles which.
          enqueueStop(intent, intent.seq);
        }
      }
    } finally {
      running = false;
    }
  }

  function ended(state: BrowserFindState): void {
    if (showing === 0 || state.pageId !== bound.pageId || state.mount !== bound.mount || state.seq !== showing) return;
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

  /**
   * The shown search ended on the backend, the newest this provider issued:
   * nothing is left to stop, and the bar closes.
   */
  function accept(): void {
    stoppedThrough = Math.max(stoppedThrough, issued);
    showing = 0;
    cancelTimer();
    drop(['start', 'next', 'previous']);
    reset();
    options.closeBar();
  }

  /** The bar closed: the backend's search goes with it. */
  function close(): void {
    showing = 0;
    cancelTimer();
    drop(['start', 'next', 'previous']);
    reset();
    stopIssued();
  }

  /** Binds the shown page and mount and starts the query there. */
  function begin(query: string): void {
    bound.pageId = options.pageId();
    bound.mount = options.mount() ?? '';
    lastSequence += 1;
    issued = lastSequence;
    showing = issued;
    enqueue({ kind: 'start', pageId: bound.pageId, mount: bound.mount, seq: issued, query, attempt: 0 });
  }

  const handle: LiveFind = { ended, escaped };
  let threadFinds = live.get(threadId);
  if (!threadFinds) {
    threadFinds = new Set();
    live.set(threadId, threadFinds);
  }
  threadFinds.add(handle);

  function step(kind: 'next' | 'previous'): void {
    if (showing === 0 || view.pending || view.count === 0) return;
    const opposite = kind === 'next' ? 'previous' : 'next';
    const last = queue[queue.length - 1];
    if (last?.kind === opposite) {
      queue.pop();
      return;
    }
    enqueue({ kind, pageId: bound.pageId, mount: bound.mount, seq: showing, query: '', attempt: 0 });
  }

  const provider: BrowserFind = {
    noun: 'page',
    result,
    search(query: string): void {
      if (mode === 'engine' || disposed) return;
      cancelTimer();
      showing = 0;
      drop(['start', 'next', 'previous']);
      if (!query.trim()) {
        reset();
        stopIssued();
        return;
      }
      view.pending = true;
      view.error = null;
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
      // The pane goes, and with its mount the backend ends its search and
      // stops the engine's: nothing is left to show the failure on.
      if (undelivered.size > 0) options.onCleanupError(null);
      disposed = true;
      threadFinds?.delete(handle);
      if (threadFinds?.size === 0) live.delete(threadId);
    },
  };
  if (mode === 'engine') {
    provider.openNative = (seed: string) => {
      if (disposed) return;
      cancelTimer();
      drop(['start']);
      begin(seed);
    };
  }
  return provider;
}

export function resetBrowserFindForTest(): void {
  live.clear();
  lastSequence = 0;
}
