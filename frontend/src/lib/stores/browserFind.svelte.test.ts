import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  applyBrowserFindEvent,
  BROWSER_FIND_DEBOUNCE_MS,
  createBrowserFind,
  resetBrowserFindForTest,
  type BrowserFindMode,
} from './browserFind.svelte';
import { BrowserCompanionEvent, BrowserFindState } from './bindings';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { closePaneFind, isPaneFindOpen, openPaneFind, registerPaneFindProvider, resetPaneFindForTest } from './paneFind.svelte';

interface Call {
  threadId: string;
  kind: string;
  pageId: string;
  query: string;
  mount: string;
}

function state(partial: Partial<BrowserFindState>): BrowserFindState {
  return new BrowserFindState({
    pageId: 'page-1', incarnation: 1, mount: 'mount-1', document: 1, session: 1, active: true, current: -1, ...partial,
  });
}

let calls: Call[];
let answer: (call: Call) => Promise<BrowserFindState>;
let pageId: string;
let mount: string | null;
// The backend's generation: every action takes the next one. A restored
// page is a new incarnation whose generations may start over.
let gen: number;
let incarnation: number;

/** The page is suspended and restored: a new incarnation, numbered afresh. */
function restore(): void {
  incarnation += 1;
  gen = 0;
}

function setup(mode: BrowserFindMode = 'bar') {
  const onEnded = vi.fn();
  const onError = vi.fn();
  const find = createBrowserFind({
    threadId: 'thread', paneId: 'browser-main', mode, pageId: () => pageId, mount: () => mount, onEnded, onError,
  });
  return { find, onEnded, onError };
}

function ended(session: number, page = 'page-1', identity: Partial<BrowserFindState> = {}): void {
  applyBrowserFindEvent(new BrowserCompanionEvent({
    kind: 'find', threadId: 'thread', pageId: page,
    find: state({ pageId: page, incarnation, session, active: false, ...identity }),
  }));
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
  incarnation = 1;
  answer = async (call) => {
    gen += 1;
    const identity = { pageId: call.pageId, mount: call.mount, incarnation, session: gen };
    if (call.kind === 'start' || call.kind === 'next' || call.kind === 'previous') {
      return state({ ...identity, query: call.query, found: true, matches: 3, current: 0 });
    }
    return state({ ...identity, active: false });
  };
  setBindingMock('BrowserCompanionFind', async (threadId: string, action: { kind: string; pageId: string; query?: string; mount?: string }) => {
    const call = { threadId, kind: action.kind, pageId: action.pageId, query: action.query ?? '', mount: action.mount ?? '' };
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
  // Row: bar opened. The backend learns the bar is open for this mount, so
  // the page's Escape is claimed even with no query.
  it('tells the backend the bar opened in its pane mount, with or without a query', async () => {
    const { find } = setup();
    find.opened?.();
    await settle(0);
    expect(calls).toEqual([{ threadId: 'thread', kind: 'open', pageId: 'page-1', query: '', mount: 'mount-1' }]);
  });

  // Row: query typed.
  it('debounces the query to one start on the shown page and shows the answer', async () => {
    const { find } = setup();
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
    answer = (call) => call.query === 'slow' ? slow.promise : Promise.resolve(state({ session: 2, query: call.query, found: true, matches: 1, current: 0 }));
    find.search('slow');
    await settle();
    find.search('fast');
    await settle();
    slow.release(state({ query: 'slow', found: true, matches: 9, current: 0 }));
    await settle(0);
    expect(find.result).toMatchObject({ count: 1, current: 0 });
  });

  // Reported order: a start in flight, a second query's start queued, a
  // third query typed before the first answers. The queued start is dropped
  // the moment the query changes, so its answer can never show under the
  // third query.
  it('drops a queued start the moment the query changes', async () => {
    const { find } = setup();
    const alpha = held();
    answer = (call) => call.query === 'alpha'
      ? alpha.promise
      : Promise.resolve(state({ session: 5, query: call.query, found: true, matches: call.query === 'gamma' ? 2 : 7, current: 0 }));
    find.search('alpha');
    await settle();
    find.search('beta');
    await settle();
    find.search('gamma');
    alpha.release(state({ session: 1, query: 'alpha', found: true, matches: 4, current: 0 }));
    await settle(0);
    expect(find.result.pending).toBe(true);
    await settle();
    expect(calls.map((call) => call.query)).toEqual(['alpha', 'gamma']);
    expect(find.result).toMatchObject({ count: 2, pending: false });
  });

  // Row: query emptied. The search ends; the bar, and its claim, stay.
  it('an emptied query stops the search and leaves the bar open', async () => {
    const { find } = setup();
    find.opened?.();
    find.search('');
    await settle(0);
    expect(kinds()).toEqual(['open']);
    find.search('alpha');
    await settle();
    find.search('  ');
    await settle(0);
    expect(kinds()).toEqual(['open', 'start', 'stop']);
    expect(find.result).toMatchObject({ count: 0, pending: false });
    // Typed and emptied before the debounce: nothing to stop.
    find.search('al');
    find.search('');
    await settle();
    expect(kinds()).toEqual(['open', 'start', 'stop']);
  });

  // Row: bar closed.
  it('a close ends the bar and its search, twice, and sends nothing with none held', async () => {
    const { find } = setup();
    find.clear();
    await settle(0);
    expect(calls).toEqual([]);
    for (let round = 0; round < 2; round++) {
      find.opened?.();
      find.search('alpha');
      await settle();
      expect(find.result.count).toBe(3);
      find.clear();
      await settle(0);
      expect(find.result).toMatchObject({ count: 0, current: -1, pending: false });
    }
    expect(kinds()).toEqual(['open', 'start', 'close', 'open', 'start', 'close']);
  });

  it('a close made while the open is in flight follows it', async () => {
    const { find } = setup();
    const open = held();
    answer = (call) => call.kind === 'open' ? open.promise : Promise.resolve(state({ session: 9, active: false }));
    find.opened?.();
    await settle(0);
    find.clear();
    await settle(0);
    expect(kinds()).toEqual(['open']);
    open.release(state({ session: 1, active: false }));
    await settle(0);
    expect(kinds()).toEqual(['open', 'close']);
  });

  it('a start the reader closed on before it answered is closed after it', async () => {
    const { find } = setup();
    const start = held();
    answer = (call) => call.kind === 'start' ? start.promise : Promise.resolve(state({ session: 2, active: false }));
    find.search('late');
    await settle();
    find.clear();
    await settle(0);
    start.release(state({ query: 'late', found: true, matches: 2, current: 0 }));
    await settle(0);
    expect(kinds()).toEqual(['start', 'close']);
    expect(find.result).toMatchObject({ count: 0, current: -1, pending: false });
  });

  // Row: Enter / Shift+Enter.
  it('steps with next and previous only while a search has matches', async () => {
    const { find } = setup();
    find.next?.();
    expect(calls).toEqual([]);
    find.search('alpha');
    await settle();
    answer = async (call) => state({ found: true, matches: 3, current: call.kind === 'next' ? 1 : 0 });
    find.next?.();
    await settle(0);
    expect(find.result.current).toBe(1);
    find.previous?.();
    await settle(0);
    expect(kinds()).toEqual(['start', 'next', 'previous']);
    expect(find.result.current).toBe(0);

    answer = async () => state({ session: 2, found: false, matches: 0 });
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
      releases.push(() => resolve(state({ found: true, matches: 3, current: call.kind === 'start' ? 0 : 1 })));
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

  it('shows an engine without a count as found with no numbers', async () => {
    const { find } = setup();
    answer = async () => state({ found: true, matches: -1, current: -1 });
    find.search('alpha');
    await settle();
    expect(find.result).toMatchObject({ count: null, current: -1 });
  });

  // Row: failure.
  it('reports a failed request and clears the result', async () => {
    const { find, onError } = setup();
    answer = async () => { throw new Error('find runs on the page the browser pane shows'); };
    find.search('alpha');
    await settle();
    expect(onError).toHaveBeenCalledWith('find runs on the page the browser pane shows');
    expect(find.result).toMatchObject({ count: 0, pending: false });
  });

  // Row: end event, newer than known, for the held page.
  it('closes its bar when the backend ends its find, not on an older end or another page', async () => {
    const { find, onEnded } = setup();
    answer = async () => state({ session: 4, found: true, matches: 3, current: 0 });
    find.search('alpha');
    await settle();
    ended(3);
    ended(5, 'page-2');
    expect(onEnded).not.toHaveBeenCalled();
    ended(5);
    expect(onEnded).toHaveBeenCalledTimes(1);
    expect(find.result).toMatchObject({ count: 0, current: -1 });
  });

  it('an end for a bar that is not open, or already ended, changes nothing', async () => {
    const { find, onEnded } = setup();
    ended(5);
    expect(onEnded).not.toHaveBeenCalled();
    find.opened?.();
    await settle(0);
    ended(9);
    ended(10);
    expect(onEnded).toHaveBeenCalledTimes(1);
  });

  // Reported order: a step in flight when the backend ends the find (a tab
  // switch). The end waits for the step, whose inactive answer clears the
  // shown result; the end still closes the bar.
  it('closes the bar on an end that lands while a step is in flight', async () => {
    const { find, onEnded } = setup();
    find.opened?.();
    find.search('alpha');
    await settle();
    const step = held();
    answer = () => step.promise;
    find.next?.();
    await settle(0);
    ended(gen + 1);
    expect(onEnded).not.toHaveBeenCalled();
    step.release(state({ session: 2, active: false }));
    await settle(0);
    expect(onEnded).toHaveBeenCalledTimes(1);
  });

  it('an end older than the answer in flight does not close the bar it opened', async () => {
    const { find, onEnded } = setup();
    answer = async () => state({ session: 1, found: true, matches: 3, current: 0 });
    find.search('alpha');
    await settle();
    const start = held();
    answer = () => start.promise;
    find.search('beta');
    await settle();
    ended(2);
    start.release(state({ session: 3, found: true, matches: 1, current: 0 }));
    await settle(0);
    expect(onEnded).not.toHaveBeenCalled();
    expect(find.result.count).toBe(1);
  });

  // Reported order: the page closes with its bar open while another tab
  // stays. The backend ended the find; the bar's close that follows sends
  // nothing to the gone page, so no error reaches the pane.
  it('sends nothing for a find the backend ended, and reports no error', async () => {
    const { find, onEnded, onError } = setup();
    registerPaneFindProvider('browser-main', find);
    onEnded.mockImplementation(() => closePaneFind('browser-main'));
    openPaneFind('browser-main', { seed: 'alpha' });
    await settle();
    expect(kinds()).toEqual(['open', 'start']);
    ended(gen + 1);
    expect(onEnded).toHaveBeenCalledTimes(1);
    expect(isPaneFindOpen('browser-main')).toBe(false);
    await settle(0);
    expect(kinds()).toEqual(['open', 'start']);
    expect(onError).not.toHaveBeenCalled();
    // A reopen on the tab now shown starts afresh there.
    pageId = 'page-2';
    openPaneFind('browser-main');
    await settle();
    expect(calls.slice(2)).toMatchObject([{ kind: 'open', pageId: 'page-2' }, { kind: 'start', pageId: 'page-2', query: 'alpha' }]);
  });

  it('a provider registered under an open bar opens it on the backend', async () => {
    const first = setup();
    const release = registerPaneFindProvider('browser-main', first.find);
    openPaneFind('browser-main', { seed: 'alpha' });
    await settle();
    release();
    first.find.dispose();
    await settle(0);
    const second = setup();
    openPaneFind('browser-main');
    registerPaneFindProvider('browser-main', second.find);
    await settle();
    expect(kinds()).toEqual(['open', 'start', 'close', 'open', 'start']);
  });

  // Row: engine find opened.
  it('in engine mode opens the engine find with the seed and never opens a bar', async () => {
    const { find, onError } = setup('engine');
    expect(find.openNative).toBeTypeOf('function');
    find.opened?.();
    find.openNative?.('needle');
    find.search('ignored');
    await settle();
    expect(calls).toMatchObject([{ kind: 'start', pageId: 'page-1', query: 'needle' }]);
    answer = async () => { throw new Error('no engine find'); };
    find.openNative?.('');
    await settle(0);
    expect(onError).toHaveBeenCalledWith('no engine find');
  });

  // The engine's UI closes without telling AO, so it never reads as open:
  // find.close (Escape) is never the pane's for it, and each open seeds it
  // again. The engine's search is still closed when the provider goes.
  it('never reads the engine find UI as open, and closes it on dispose', async () => {
    const { find } = setup('engine');
    registerPaneFindProvider('browser-main', find);
    for (let round = 0; round < 2; round++) {
      openPaneFind('browser-main', { seed: 'alpha' });
      await settle(0);
      expect(isPaneFindOpen('browser-main')).toBe(false);
      closePaneFind('browser-main');
      await settle(0);
    }
    expect(kinds()).toEqual(['start', 'start']);
    find.dispose();
    await settle(0);
    expect(kinds()).toEqual(['start', 'start', 'close']);
  });

  // Reported order: the pane unmounts while a start is in flight. The
  // release clears the bar and queues its close; the dispose that follows
  // must not drop it, or the engine's search stays open.
  for (const mode of ['bar', 'engine'] as const) {
    it(`keeps the close a clear queued through a dispose, with a start in flight (${mode})`, async () => {
      const { find } = setup(mode);
      const start = held();
      answer = (call) => call.kind === 'start' ? start.promise : Promise.resolve(state({ session: 9, active: false }));
      if (mode === 'bar') {
        find.opened?.();
        find.search('alpha');
      } else {
        find.openNative?.('alpha');
      }
      await settle();
      find.clear();
      find.dispose();
      find.clear();
      start.release(state({ session: 2, query: 'alpha', found: true, matches: 2, current: 0 }));
      await settle(0);
      expect(kinds()).toEqual(mode === 'bar' ? ['open', 'start', 'close'] : ['start', 'close']);
      expect(calls.at(-1)).toMatchObject({ kind: 'close', pageId: 'page-1', mount: 'mount-1' });
    });
  }

  // A close queued for one page survives the backend ending the find the
  // provider then held on another: the first page's search still needs it.
  it('keeps a queued close through an end of a later find', async () => {
    const { find, onEnded } = setup();
    const start = held();
    answer = (call) => call.kind === 'start' ? start.promise : Promise.resolve(state({ pageId: call.pageId, session: 9, active: false }));
    find.search('alpha');
    await settle();
    find.clear();
    pageId = 'page-2';
    find.opened?.();
    ended(20, 'page-2');
    start.release(state({ session: 2, query: 'alpha', found: true, matches: 2, current: 0 }));
    await settle(0);
    expect(onEnded).toHaveBeenCalledTimes(1);
    expect(calls).toMatchObject([{ kind: 'start', pageId: 'page-1' }, { kind: 'close', pageId: 'page-1' }]);
    expect(calls).toHaveLength(2);
  });

  // Reported order: a page searched up to generation 10 is suspended and
  // restored, and the restored page numbers its finds afresh. Its answers
  // reset what the provider knows, so its end closes the bar, and a late end
  // of the old incarnation does not.
  it('follows a restored page whose generations started over', async () => {
    const { find, onEnded } = setup();
    find.opened?.();
    for (const query of ['a', 'al', 'alp', 'alph', 'alpha']) {
      find.search(query);
      await settle();
    }
    gen = 10;
    find.next?.();
    await settle(0);
    find.clear();
    await settle(0);
    restore();
    find.opened?.();
    find.search('alpha');
    await settle();
    expect(gen).toBe(2);
    ended(11, 'page-1', { incarnation: 1 });
    expect(onEnded).not.toHaveBeenCalled();
    ended(3);
    expect(onEnded).toHaveBeenCalledTimes(1);
  });

  // Another mount opening the page takes its find over: the backend ends
  // this mount's find, and this bar closes. An end of another mount's find
  // leaves this one alone.
  it('closes only on an end of its own mount', async () => {
    const { find, onEnded } = setup();
    find.opened?.();
    find.search('alpha');
    await settle();
    ended(gen + 5, 'page-1', { mount: 'mount-2' });
    expect(onEnded).not.toHaveBeenCalled();
    ended(gen + 1);
    expect(onEnded).toHaveBeenCalledTimes(1);
  });

  // The close carries the mount the find was opened for, not whichever the
  // pane has when it runs, and steps go to that mount too.
  it('sends every action to the mount that holds the find', async () => {
    const { find } = setup();
    find.opened?.();
    find.search('alpha');
    await settle();
    mount = 'mount-2';
    find.next?.();
    find.clear();
    await settle(0);
    expect(calls.map((call) => call.mount)).toEqual(['mount-1', 'mount-1', 'mount-1', 'mount-1']);
    expect(kinds()).toEqual(['open', 'start', 'next', 'close']);
  });

  it('a disposed provider hears no end and closes what it held', async () => {
    const { find, onEnded } = setup();
    find.opened?.();
    find.search('alpha');
    await settle();
    find.dispose();
    await settle(0);
    ended(50);
    expect(onEnded).not.toHaveBeenCalled();
    expect(calls.at(-1)?.kind).toBe('close');
  });
});
