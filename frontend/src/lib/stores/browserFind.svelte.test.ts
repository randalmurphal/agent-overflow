import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  applyBrowserFindEvent,
  BROWSER_FIND_DEBOUNCE_MS,
  BROWSER_FIND_STOP_ATTEMPTS,
  BROWSER_FIND_STOP_RETRY_MS,
  createBrowserFind,
  resetBrowserFindForTest,
  type BrowserFindMode,
} from './browserFind.svelte';
import { BrowserCompanionEvent, BrowserFindState } from './bindings';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import {
  closePaneFind,
  isPaneFindOpen,
  openPaneFind,
  paneFindReads,
  registerPaneFindProvider,
  resetPaneFindForTest,
  setPaneFindQuery,
} from './paneFind.svelte';

interface Call {
  threadId: string;
  kind: string;
  pageId: string;
  query: string;
  mount: string;
  findId: string;
}

const PANE = 'browser-main';

function state(partial: Partial<BrowserFindState>): BrowserFindState {
  return new BrowserFindState({
    pageId: 'page-1', incarnation: 1, mount: 'mount-1', document: 1, session: 1, active: true, current: -1, ...partial,
  });
}

let calls: Call[];
let answer: (call: Call) => Promise<BrowserFindState>;
let pageId: string;
let mount: string | null;
// The backend's generation: every action takes the next one.
let gen: number;

/** The find id the provider's newest call named. */
const lastId = (): string => calls.at(-1)?.findId ?? '';

/** A refusal the backend answered: the action did not run. */
const refused = (message: string) => new TransportError('method_error', message);

/**
 * A provider registered with the pane-find store as the browser pane
 * registers it, its bar closing through the store.
 */
function setup(mode: BrowserFindMode = 'bar') {
  const closeBar = vi.fn(() => closePaneFind(PANE));
  const onError = vi.fn();
  const find = createBrowserFind({
    threadId: 'thread', paneId: PANE, mode, pageId: () => pageId, mount: () => mount, closeBar, onError,
  });
  const release = registerPaneFindProvider(PANE, find);
  const unmount = () => {
    release();
    find.dispose();
  };
  return { find, closeBar, onError, unmount };
}

/** A bar-mode provider whose bar is open with a search for `query`. */
async function setupSearching(query = 'alpha') {
  const made = setup();
  openPaneFind(PANE, { seed: query });
  await settle();
  return made;
}

/** The backend ends a search: by default the one of the newest call's find. */
function ended(session: number, page = 'page-1', identity: Partial<BrowserFindState> = {}): void {
  applyBrowserFindEvent(new BrowserCompanionEvent({
    kind: 'find', threadId: 'thread', pageId: page,
    find: state({ pageId: page, session, active: false, findId: lastId(), ...identity }),
  }));
}

function escaped(page = 'page-1', threadId = 'thread'): void {
  applyBrowserFindEvent(new BrowserCompanionEvent({ kind: 'escape', threadId, pageId: page }));
}

async function settle(ms = BROWSER_FIND_DEBOUNCE_MS): Promise<void> {
  await vi.advanceTimersByTimeAsync(ms);
}

/** A promise answer the test resolves later. */
function held(): { promise: Promise<BrowserFindState>; release: (s: BrowserFindState) => void } {
  let release!: (s: BrowserFindState) => void;
  const promise = new Promise<BrowserFindState>((resolve) => { release = resolve; });
  return { promise, release };
}

const kinds = () => calls.map((call) => call.kind);

beforeEach(() => {
  vi.useFakeTimers();
  resetBrowserFindForTest();
  resetPaneFindForTest();
  calls = [];
  pageId = 'page-1';
  mount = 'mount-1';
  gen = 0;
  answer = async (call) => {
    gen += 1;
    const identity = { pageId: call.pageId, mount: call.mount, findId: call.findId, session: gen };
    if (call.kind === 'start' || call.kind === 'next' || call.kind === 'previous') {
      return state({ ...identity, query: call.query, found: true, matches: 3, current: 0 });
    }
    return state({ ...identity, active: false });
  };
  setBindingMock('BrowserCompanionFind', async (threadId: string, action: { kind: string; pageId: string; query?: string; mount?: string; findId?: string }) => {
    const call = { threadId, kind: action.kind, pageId: action.pageId, query: action.query ?? '', mount: action.mount ?? '', findId: action.findId ?? '' };
    calls.push(call);
    return answer(call);
  });
});

afterEach(() => {
  vi.useRealTimers();
  resetBrowserFindForTest();
  resetPaneFindForTest();
});

describe('browser find provider', () => {
  // A bar's open is the store's: nothing reaches the backend until a query
  // searches.
  it('sends nothing when the bar opens with no query', async () => {
    setup();
    openPaneFind(PANE);
    await settle();
    expect(calls).toEqual([]);
    expect(isPaneFindOpen(PANE)).toBe(true);
  });

  // Each bar is a find of its own: every action names the bar's id, and the
  // bar opened again after a close makes another.
  it('names its find id on every action of a bar, and a new one after the bar closes', async () => {
    const { find } = await setupSearching();
    find.next?.();
    setPaneFindQuery(PANE, '');
    setPaneFindQuery(PANE, 'beta');
    await settle();
    closePaneFind(PANE);
    await settle(0);
    openPaneFind(PANE);
    await settle();
    expect(kinds()).toEqual(['start', 'next', 'stop', 'start', 'stop', 'start']);
    const ids = calls.map((call) => call.findId);
    expect(ids[0]).not.toBe('');
    expect(new Set(ids.slice(0, 5)).size).toBe(1);
    expect(ids[5]).not.toBe(ids[0]);
    expect(calls).toMatchObject(Array(6).fill({ threadId: 'thread', pageId: 'page-1', mount: 'mount-1' }));
  });

  // Row: query typed.
  it('debounces the query to one start on the shown page and shows the answer', async () => {
    const { find } = setup();
    openPaneFind(PANE);
    find.search('al');
    find.search('alpha');
    expect(find.result.pending).toBe(true);
    await settle(BROWSER_FIND_DEBOUNCE_MS - 1);
    expect(calls).toEqual([]);
    await settle(1);
    expect(calls).toMatchObject([{ kind: 'start', pageId: 'page-1', query: 'alpha', mount: 'mount-1' }]);
    expect(find.result).toMatchObject({ count: 3, current: 0, pending: false, capped: false });
  });

  it('drops an answer a newer query superseded', async () => {
    const { find } = setup();
    const slow = held();
    answer = (call) => call.query === 'slow'
      ? slow.promise
      : Promise.resolve(state({ findId: call.findId, session: 2, query: call.query, found: true, matches: 1, current: 0 }));
    find.search('slow');
    await settle();
    find.search('fast');
    await settle();
    slow.release(state({ findId: lastId(), query: 'slow', found: true, matches: 9, current: 0 }));
    await settle(0);
    expect(find.result).toMatchObject({ count: 1, current: 0 });
  });

  // A start in flight, a second query's start queued, a third query typed
  // before the first answers: the queued start is dropped the moment the
  // query changes, so its answer can never show under the third query.
  it('drops a queued start the moment the query changes', async () => {
    const { find } = setup();
    const alpha = held();
    answer = (call) => call.query === 'alpha'
      ? alpha.promise
      : Promise.resolve(state({ findId: call.findId, session: 5, query: call.query, found: true, matches: call.query === 'gamma' ? 2 : 7, current: 0 }));
    find.search('alpha');
    await settle();
    find.search('beta');
    await settle();
    find.search('gamma');
    alpha.release(state({ findId: lastId(), session: 1, query: 'alpha', found: true, matches: 4, current: 0 }));
    await settle(0);
    expect(find.result.pending).toBe(true);
    await settle();
    expect(calls.map((call) => call.query)).toEqual(['alpha', 'gamma']);
    expect(find.result).toMatchObject({ count: 2, pending: false });
  });

  // Row: query emptied. The search ends; the bar stays.
  it('an emptied query stops the search and leaves the bar open', async () => {
    const { find } = setup();
    openPaneFind(PANE);
    find.search('');
    await settle(0);
    expect(calls).toEqual([]);
    find.search('alpha');
    await settle();
    find.search('  ');
    await settle(0);
    expect(kinds()).toEqual(['start', 'stop']);
    expect(find.result).toMatchObject({ count: 0, pending: false });
    expect(isPaneFindOpen(PANE)).toBe(true);
    // Typed and emptied before the debounce: nothing to stop.
    find.search('al');
    find.search('');
    await settle();
    expect(kinds()).toEqual(['start', 'stop']);
  });

  // Row: bar closed.
  it('a close stops the search, twice, and sends nothing with none held', async () => {
    const { find } = setup();
    find.clear();
    await settle(0);
    expect(calls).toEqual([]);
    for (let round = 0; round < 2; round++) {
      openPaneFind(PANE, { seed: 'alpha' });
      await settle();
      expect(find.result.count).toBe(3);
      closePaneFind(PANE);
      await settle(0);
      expect(find.result).toMatchObject({ count: 0, current: -1, pending: false });
    }
    expect(kinds()).toEqual(['start', 'stop', 'start', 'stop']);
  });

  it('a start the reader closed on before it answered is stopped after it', async () => {
    const { find } = setup();
    const start = held();
    answer = (call) => call.kind === 'start' ? start.promise : Promise.resolve(state({ session: 2, active: false }));
    openPaneFind(PANE, { seed: 'late' });
    await settle();
    closePaneFind(PANE);
    await settle(0);
    start.release(state({ findId: calls[0].findId, query: 'late', found: true, matches: 2, current: 0 }));
    await settle(0);
    expect(kinds()).toEqual(['start', 'stop']);
    expect(calls[1].findId).toBe(calls[0].findId);
    expect(find.result).toMatchObject({ count: 0, current: -1, pending: false });
  });

  // Row: Enter / Shift+Enter.
  it('steps with next and previous only while a search has matches', async () => {
    const { find } = setup();
    find.next?.();
    expect(calls).toEqual([]);
    find.search('alpha');
    await settle();
    answer = async (call) => state({ findId: call.findId, session: 2, found: true, matches: 3, current: call.kind === 'next' ? 1 : 0 });
    find.next?.();
    await settle(0);
    expect(find.result.current).toBe(1);
    find.previous?.();
    await settle(0);
    expect(kinds()).toEqual(['start', 'next', 'previous']);
    expect(find.result.current).toBe(0);

    answer = async (call) => state({ findId: call.findId, session: 3, found: false, matches: 0 });
    find.search('absent');
    await settle();
    expect(find.result.count).toBe(0);
    find.next?.();
    expect(calls.at(-1)?.kind).toBe('start');
  });

  it('makes every engine call in order, one at a time, and nets opposite steps', async () => {
    const { find } = setup();
    const releases: Array<() => void> = [];
    answer = (call) => new Promise((resolve) => {
      releases.push(() => resolve(state({ findId: call.findId, found: true, matches: 3, current: call.kind === 'start' ? 0 : 1 })));
    });
    find.search('alpha');
    await settle();
    expect(kinds()).toEqual(['start']);
    releases.shift()!();
    await settle(0);
    find.next?.();
    find.next?.();
    find.next?.();
    find.previous?.();
    find.next?.();
    await settle(0);
    expect(kinds()).toEqual(['start', 'next']);
    while (releases.length) {
      releases.shift()!();
      await settle(0);
    }
    expect(kinds()).toEqual(['start', 'next', 'next', 'next']);
  });

  // The provider's calls run on its own queue: a read another provider of
  // the pane left in the pane's read slot holds nothing of the browser
  // find's up.
  it('runs its calls on its own queue, whatever holds the pane\'s read slot', async () => {
    paneFindReads(PANE).request(() => new Promise<void>(() => {}));
    const { find } = setup();
    find.search('alpha');
    await settle();
    find.search('');
    await settle(0);
    expect(kinds()).toEqual(['start', 'stop']);
  });

  it('shows an engine without a count as found with no numbers', async () => {
    const { find } = setup();
    answer = async (call) => state({ findId: call.findId, found: true, matches: -1, current: -1 });
    find.search('alpha');
    await settle();
    expect(find.result).toMatchObject({ count: null, current: -1 });
  });

  // Row: failed start or step.
  it('reports a failed start, clears the result, and holds no search after it', async () => {
    const { find, onError, closeBar } = setup();
    answer = async () => { throw refused('find runs on the page the browser pane shows'); };
    find.search('alpha');
    await settle();
    expect(onError).toHaveBeenCalledWith('find runs on the page the browser pane shows');
    expect(find.result).toMatchObject({ count: 0, pending: false });
    find.search('');
    find.next?.();
    await settle(0);
    // The backend answered the refusal: there is nothing to stop.
    expect(kinds()).toEqual(['start']);
    expect(closeBar).not.toHaveBeenCalled();
  });

  // Row: end event for the held search.
  it('closes its bar on the end of its own search, and on no other', async () => {
    const { find, closeBar } = await setupSearching();
    const session = gen;
    ended(session + 5, 'page-1', { findId: 'another-find' });
    ended(session + 5, 'page-1', { mount: 'mount-2' });
    ended(session + 5, 'page-2');
    // No newer than the answer taken: an end of an earlier search.
    ended(session);
    expect(closeBar).not.toHaveBeenCalled();
    expect(isPaneFindOpen(PANE)).toBe(true);
    ended(session + 1);
    expect(closeBar).toHaveBeenCalledTimes(1);
    expect(isPaneFindOpen(PANE)).toBe(false);
    expect(find.result).toMatchObject({ count: 0, current: -1 });
    await settle(0);
    // The backend ended the search: nothing is stopped.
    expect(kinds()).toEqual(['start']);
  });

  it('an end with no search held changes nothing', async () => {
    const { closeBar } = setup();
    ended(5, 'page-1', { findId: 'x' });
    openPaneFind(PANE);
    ended(9, 'page-1', { findId: 'x' });
    expect(closeBar).not.toHaveBeenCalled();
    expect(isPaneFindOpen(PANE)).toBe(true);
  });

  // A step in flight when the backend ends the search (a tab switch): the
  // end closes the bar at once. The step's late answer shows nothing, and
  // a search it reports running is stopped.
  it('closes the bar on an end that lands while a step is in flight, and stops what the step left', async () => {
    const { find, closeBar } = await setupSearching();
    const step = held();
    answer = (call) => call.kind === 'next' ? step.promise : Promise.resolve(state({ session: 99, active: false }));
    find.next?.();
    await settle(0);
    const id = lastId();
    ended(gen + 1);
    expect(closeBar).toHaveBeenCalledTimes(1);
    step.release(state({ findId: id, session: gen + 2, found: true, matches: 3, current: 1 }));
    await settle(0);
    expect(closeBar).toHaveBeenCalledTimes(1);
    expect(find.result).toMatchObject({ count: 0, current: -1 });
    expect(calls.at(-1)).toMatchObject({ kind: 'stop', findId: id });
  });

  // A late end of an earlier search of the bar, arriving after a later
  // start's answer, is older than it: the bar stays.
  it('keeps the bar over a later search when an earlier search\'s end arrives late', async () => {
    const { find, closeBar } = await setupSearching();
    const early = gen;
    setPaneFindQuery(PANE, '');
    setPaneFindQuery(PANE, 'beta');
    await settle();
    expect(find.result.count).toBe(3);
    expect(early + 1).toBeLessThan(gen);
    ended(early + 1);
    expect(closeBar).not.toHaveBeenCalled();
    expect(find.result.count).toBe(3);
  });

  // Row: foreign answer to a start or step.
  for (const kind of ['start', 'next'] as const) {
    it(`closes the bar on a foreign answer to a ${kind}`, async () => {
      const { find, closeBar } = setup();
      openPaneFind(PANE);
      if (kind !== 'start') {
        find.search('alpha');
        await settle();
      }
      answer = async (call) => state({ pageId: call.pageId, session: 50, mount: 'mount-2', findId: 'other', foreign: true, active: false });
      if (kind === 'start') find.search('alpha');
      else find.next?.();
      await settle();
      expect(closeBar).toHaveBeenCalledTimes(1);
      expect(isPaneFindOpen(PANE)).toBe(false);
      expect(find.result).toMatchObject({ count: 0, pending: false });
    });
  }

  it('a foreign answer to a stop changes nothing more', async () => {
    const { find, closeBar } = await setupSearching();
    answer = async (call) => state({ pageId: call.pageId, session: 50, mount: '', findId: '', foreign: true, active: false });
    find.search('');
    await settle(0);
    expect(closeBar).not.toHaveBeenCalled();
    expect(isPaneFindOpen(PANE)).toBe(true);
    expect(kinds()).toEqual(['start', 'stop']);
  });

  // The stop carries the mount the search was started for, not whichever
  // the pane has when it runs, and steps go to that mount too.
  it('sends every action of a search to the mount that holds it', async () => {
    const { find } = await setupSearching();
    mount = 'mount-2';
    find.next?.();
    closePaneFind(PANE);
    await settle(0);
    expect(calls.map((call) => call.mount)).toEqual(['mount-1', 'mount-1', 'mount-1']);
    expect(kinds()).toEqual(['start', 'next', 'stop']);
  });

  // Row: engine find opened.
  it('in engine mode opens the engine find with the seed, a find of its own each time', async () => {
    const { find, onError } = setup('engine');
    expect(find.openNative).toBeTypeOf('function');
    find.openNative?.('needle');
    find.search('ignored');
    await settle(0);
    find.openNative?.('');
    await settle(0);
    expect(calls).toMatchObject([{ kind: 'start', pageId: 'page-1', query: 'needle' }, { kind: 'start', query: '' }]);
    expect(calls[1].findId).not.toBe(calls[0].findId);
    answer = async () => { throw refused('no engine find'); };
    find.openNative?.('x');
    await settle(0);
    expect(onError).toHaveBeenCalledWith('no engine find');
  });

  // The engine's UI closes without telling AO, so it never reads as open:
  // find.close (Escape) is never the pane's for it. The engine's search is
  // still stopped when the pane goes.
  it('never reads the engine find UI as open, and stops it on unmount', async () => {
    const { unmount } = setup('engine');
    for (let round = 0; round < 2; round++) {
      openPaneFind(PANE, { seed: 'alpha' });
      await settle(0);
      expect(isPaneFindOpen(PANE)).toBe(false);
      closePaneFind(PANE);
      await settle(0);
    }
    expect(kinds()).toEqual(['start', 'start']);
    unmount();
    await settle(0);
    expect(kinds()).toEqual(['start', 'start', 'stop']);
  });

  // The pane unmounts while a start is in flight: the release clears the
  // bar and queues its stop, and the dispose that follows keeps it.
  for (const mode of ['bar', 'engine'] as const) {
    it(`keeps the stop a clear queued through a dispose, with a start in flight (${mode})`, async () => {
      const { find } = setup(mode);
      const start = held();
      answer = (call) => call.kind === 'start' ? start.promise : Promise.resolve(state({ session: 9, active: false }));
      if (mode === 'bar') find.search('alpha');
      else find.openNative?.('alpha');
      await settle();
      find.clear();
      find.dispose();
      find.clear();
      start.release(state({ findId: calls[0].findId, session: 2, query: 'alpha', found: true, matches: 2, current: 0 }));
      await settle(0);
      expect(kinds()).toEqual(['start', 'stop']);
      expect(calls.at(-1)).toMatchObject({ kind: 'stop', pageId: 'page-1', mount: 'mount-1', findId: calls[0].findId });
    });
  }

  it('a disposed provider hears no end and stops what it held', async () => {
    const { closeBar, unmount } = await setupSearching();
    unmount();
    await settle(0);
    ended(50);
    expect(closeBar).not.toHaveBeenCalled();
    expect(calls.at(-1)?.kind).toBe('stop');
  });

  // An unknown outcome may have left a search running: the provider holds
  // none, and stops it.
  const unknowns: Array<[string, () => Error]> = [
    ['a lost connection', () => new DisconnectedError()],
    ['a timeout', () => new TransportError('timeout', 'the call timed out')],
    ['an error that is not the transport\'s', () => new Error('broken')],
  ];
  for (const kind of ['start', 'next'] as const) {
    for (const [why, error] of unknowns) {
      it(`stops a search a ${kind} may have left after ${why}`, async () => {
        const { find, closeBar, onError } = setup();
        const answering = answer;
        if (kind === 'next') {
          find.search('alpha');
          await settle();
        }
        answer = (call) => call.kind === kind ? Promise.reject(error()) : answering(call);
        if (kind === 'start') find.search('alpha');
        else find.next?.();
        await settle();
        expect(onError).toHaveBeenCalledTimes(1);
        expect(closeBar).not.toHaveBeenCalled();
        expect(calls.at(-2)?.kind).toBe(kind);
        expect(calls.at(-1)).toMatchObject({ kind: 'stop', findId: calls.at(-2)?.findId });
        expect(find.result).toMatchObject({ count: 0, pending: false });
      });
    }
  }

  // Row: failed stop. Reported, and sent again on the provider's own timer
  // while attempts remain, through the pane's unmount too; no timer is left
  // once the attempts are spent.
  for (const [why, error] of [...unknowns, ['a refusal', () => refused('fake engine: find stop failed')] as [string, () => Error]]) {
    it(`retries a stop that failed with ${why}, a bounded number of times`, async () => {
      const { onError, unmount } = await setupSearching();
      answer = async (call) => {
        if (call.kind === 'stop') throw error();
        return state({ findId: call.findId, session: 9, active: false });
      };
      unmount();
      await settle(0);
      expect(kinds()).toEqual(['start', 'stop']);
      for (let attempt = 2; attempt <= BROWSER_FIND_STOP_ATTEMPTS; attempt++) {
        await settle(BROWSER_FIND_STOP_RETRY_MS - 1);
        expect(calls, `attempt ${attempt} waits ${BROWSER_FIND_STOP_RETRY_MS} ms`).toHaveLength(attempt);
        await settle(1);
        expect(calls).toHaveLength(attempt + 1);
      }
      expect(vi.getTimerCount()).toBe(0);
      await settle(BROWSER_FIND_STOP_RETRY_MS * 4);
      expect(kinds()).toEqual(['start', ...Array(BROWSER_FIND_STOP_ATTEMPTS).fill('stop')]);
      expect(new Set(calls.slice(1).map((call) => call.findId))).toEqual(new Set([calls[0].findId]));
      expect(onError).toHaveBeenCalledTimes(BROWSER_FIND_STOP_ATTEMPTS);
    });
  }

  it('clears a stop\'s retry once a retry answers', async () => {
    const { find } = await setupSearching();
    let failures = 1;
    answer = async (call) => {
      if (call.kind === 'stop' && failures-- > 0) throw new DisconnectedError();
      return state({ findId: call.findId, session: 9, active: false });
    };
    find.search('');
    await settle(0);
    expect(vi.getTimerCount()).toBe(1);
    await settle(BROWSER_FIND_STOP_RETRY_MS);
    expect(kinds()).toEqual(['start', 'stop', 'stop']);
    expect(vi.getTimerCount()).toBe(0);
  });

  // A start of the same find settles what its stop was owed: the retry
  // waiting would stop the new search, so it goes.
  it('a new start of the find drops the stop retry waiting for it', async () => {
    const { find } = await setupSearching();
    let failures = 1;
    const answering = answer;
    answer = async (call) => {
      if (call.kind === 'stop' && failures-- > 0) throw refused('fake engine: find stop failed');
      return answering(call);
    };
    find.search('');
    await settle(0);
    find.search('beta');
    await settle();
    expect(vi.getTimerCount()).toBe(0);
    await settle(BROWSER_FIND_STOP_RETRY_MS * 2);
    expect(kinds()).toEqual(['start', 'stop', 'start']);
    expect(find.result.count).toBe(3);
  });
});

// Row: escape event. A bare Escape pressed in the page stays the page's; the
// backend announces it, and the pane closes an open bar on the page it
// shows.
describe('browser find escape', () => {
  it('closes an open bar on the shown page, through the find.close path', async () => {
    const { closeBar } = await setupSearching();
    escaped();
    expect(closeBar).toHaveBeenCalledTimes(1);
    expect(isPaneFindOpen(PANE)).toBe(false);
    await settle(0);
    expect(kinds()).toEqual(['start', 'stop']);
  });

  it('closes an open bar with no query', () => {
    const { closeBar } = setup();
    openPaneFind(PANE);
    escaped();
    expect(closeBar).toHaveBeenCalledTimes(1);
    expect(isPaneFindOpen(PANE)).toBe(false);
  });

  it('is ignored with no bar open, for another page or thread, in engine mode, and after unmount', async () => {
    const bar = setup();
    escaped();
    openPaneFind(PANE);
    escaped('page-2');
    escaped('page-1', 'another-thread');
    expect(bar.closeBar).not.toHaveBeenCalled();
    expect(isPaneFindOpen(PANE)).toBe(true);
    bar.unmount();
    escaped();
    expect(bar.closeBar).not.toHaveBeenCalled();

    const engine = setup('engine');
    openPaneFind(PANE, { seed: 'alpha' });
    await settle(0);
    escaped();
    expect(engine.closeBar).not.toHaveBeenCalled();
    expect(calls.at(-1)?.kind).toBe('start');
  });
});
