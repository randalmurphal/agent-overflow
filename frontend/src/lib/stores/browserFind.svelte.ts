// Find in the browser pane (docs/architecture/browser-tools.md, "Find"). The
// backend's engine searches the page the pane shows and owns the session; this
// provider drives it from the pane's find bar, or for an engine with its own
// find UI opens that instead. A session the backend ends on its own (the page
// navigated, the tab changed, the page closed) arrives as a `find` companion
// event and closes the bar.

import {
  BrowserCompanionFind,
  BrowserFindAction,
  type BrowserCompanionEvent,
  type BrowserFindState,
} from './bindings';
import type { PaneFindProvider, PaneFindResult } from './paneFind.svelte';
import { errString } from '../utils/errors';

export const BROWSER_FIND_DEBOUNCE_MS = 150;

/** "bar": the shared find bar drives the engine. "engine": its own find UI. */
export type BrowserFindMode = 'bar' | 'engine';

export interface BrowserFindOptions {
  threadId: string;
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

export function createBrowserFind(options: BrowserFindOptions): PaneFindProvider & { dispose(): void } {
  const { threadId } = options;
  const view = $state({ count: 0 as number | null, current: -1, pending: false, capped: false });
  let seq = 0;
  let inflight = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  // wanted is whether the reader still wants a session: a superseded answer
  // that opened one after the reader moved on is stopped.
  let wanted = false;
  let session: Session | null = null;
  let endedWhileInflight: BrowserFindState | null = null;

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

  function endsSession(state: BrowserFindState): boolean {
    return session !== null && state.pageId === session.pageId && state.session > session.session;
  }

  function ended(state: BrowserFindState): void {
    if (inflight > 0) {
      if (!endedWhileInflight || state.session > endedWhileInflight.session) endedWhileInflight = state;
      return;
    }
    if (!endsSession(state)) return;
    seq += 1;
    cancelTimer();
    wanted = false;
    session = null;
    reset();
    options.onEnded();
  }

  function settled(): void {
    if (inflight > 0 || !endedWhileInflight) return;
    const state = endedWhileInflight;
    endedWhileInflight = null;
    ended(state);
  }

  async function request(kind: string, query = ''): Promise<BrowserFindState> {
    inflight += 1;
    try {
      return await BrowserCompanionFind(threadId, new BrowserFindAction({ kind, pageId: options.pageId(), query }));
    } finally {
      inflight -= 1;
    }
  }

  async function run(kind: 'start' | 'next' | 'previous', query = ''): Promise<void> {
    const mine = ++seq;
    let answer: BrowserFindState;
    try {
      answer = await request(kind, query);
    } catch (err) {
      if (mine === seq) {
        session = null;
        reset();
        options.onError(errString(err));
      }
      settled();
      return;
    }
    if (mine === seq) show(answer);
    else if (answer.active && !wanted) void stop();
    settled();
  }

  async function stop(): Promise<void> {
    try {
      await request('stop');
    } catch (err) {
      options.onError(errString(err));
    }
    settled();
  }

  const handle: LiveFind = { ended };
  let threadFinds = live.get(threadId);
  if (!threadFinds) {
    threadFinds = new Set();
    live.set(threadId, threadFinds);
  }
  threadFinds.add(handle);

  function step(kind: 'next' | 'previous'): void {
    if (!session || view.pending || view.count === 0) return;
    void run(kind);
  }

  const provider: PaneFindProvider & { dispose(): void } = {
    noun: 'page',
    result,
    search(query: string): void {
      if (options.mode === 'engine') return;
      cancelTimer();
      seq += 1;
      if (!query.trim()) {
        wanted = false;
        const had = session !== null || inflight > 0;
        session = null;
        reset();
        if (had) void stop();
        return;
      }
      wanted = true;
      view.pending = true;
      timer = setTimeout(() => {
        timer = null;
        void run('start', query);
      }, BROWSER_FIND_DEBOUNCE_MS);
    },
    next: () => step('next'),
    previous: () => step('previous'),
    clear(): void {
      if (options.mode === 'engine') return;
      cancelTimer();
      seq += 1;
      wanted = false;
      const had = session !== null || inflight > 0;
      session = null;
      reset();
      if (had) void stop();
    },
    dispose(): void {
      provider.clear();
      threadFinds?.delete(handle);
      if (threadFinds?.size === 0) live.delete(threadId);
    },
  };
  if (options.mode === 'engine') {
    provider.openNative = (seed: string) => {
      void request('start', seed).catch((err: unknown) => options.onError(errString(err)));
    };
  }
  return provider;
}

export function resetBrowserFindForTest(): void {
  live.clear();
}
