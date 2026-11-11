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
}

function state(partial: Partial<BrowserFindState>): BrowserFindState {
  return new BrowserFindState({ pageId: 'page-1', session: 1, active: true, current: -1, ...partial });
}

let calls: Call[];
let answer: (call: Call) => Promise<BrowserFindState>;
let pageId: string;

function setup(mode: BrowserFindMode = 'bar') {
  const onEnded = vi.fn();
  const onError = vi.fn();
  const find = createBrowserFind({ threadId: 'thread', paneId: 'browser-main', mode, pageId: () => pageId, onEnded, onError });
  return { find, onEnded, onError };
}

function ended(session: number, page = 'page-1'): void {
  applyBrowserFindEvent(
    new BrowserCompanionEvent({ kind: 'find', threadId: 'thread', pageId: page, find: state({ pageId: page, session, active: false }) }),
  );
}

async function settle(ms = BROWSER_FIND_DEBOUNCE_MS): Promise<void> {
  await vi.advanceTimersByTimeAsync(ms);
}

beforeEach(() => {
  vi.useFakeTimers();
  resetBrowserFindForTest();
  resetPaneFindForTest();
  calls = [];
  pageId = 'page-1';
  answer = async (call) =>
    call.kind === 'stop' ? state({ active: false, session: 9 }) : state({ query: call.query, found: true, matches: 3, current: 0 });
  setBindingMock('BrowserCompanionFind', async (threadId: string, action: { kind: string; pageId: string; query?: string }) => {
    const call = { threadId, kind: action.kind, pageId: action.pageId, query: action.query ?? '' };
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
  it('debounces the query to one start on the shown page and shows the answer', async () => {
    const { find } = setup();
    find.search('al');
    find.search('alpha');
    expect(find.result.pending).toBe(true);
    await settle(BROWSER_FIND_DEBOUNCE_MS - 1);
    expect(calls).toEqual([]);
    await settle(1);
    expect(calls).toEqual([{ threadId: 'thread', kind: 'start', pageId: 'page-1', query: 'alpha' }]);
    expect(find.result).toMatchObject({ count: 3, current: 0, pending: false, capped: false });
  });

  it('drops an answer a newer query superseded', async () => {
    const { find } = setup();
    let release!: () => void;
    answer = (call) =>
      call.query === 'slow'
        ? new Promise((resolve) => { release = () => resolve(state({ query: 'slow', found: true, matches: 9, current: 0 })); })
        : Promise.resolve(state({ session: 2, query: call.query, found: true, matches: 1, current: 0 }));
    find.search('slow');
    await settle();
    find.search('fast');
    await settle();
    release();
    await settle(0);
    expect(find.result).toMatchObject({ count: 1, current: 0 });
  });

  it('stops a session a superseded answer opened after the reader closed', async () => {
    const { find } = setup();
    let release!: () => void;
    answer = (call) =>
      call.kind === 'start'
        ? new Promise((resolve) => { release = () => resolve(state({ query: call.query, found: true, matches: 2, current: 0 })); })
        : Promise.resolve(state({ active: false, session: 2 }));
    find.search('late');
    await settle();
    find.clear();
    await settle(0);
    // The close's own stop can reach the backend before the start it raced.
    const stopsBefore = calls.filter((call) => call.kind === 'stop').length;
    release();
    await settle(0);
    expect(calls.filter((call) => call.kind === 'stop').length).toBe(stopsBefore + 1);
    expect(find.result).toMatchObject({ count: 0, current: -1, pending: false });
  });

  it('steps with next and previous only while a session has matches', async () => {
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
    expect(calls.map((call) => call.kind)).toEqual(['start', 'next', 'previous']);
    expect(find.result.current).toBe(0);

    answer = async () => state({ session: 2, found: false, matches: 0 });
    find.search('absent');
    await settle();
    expect(find.result.count).toBe(0);
    find.next?.();
    expect(calls.at(-1)?.kind).toBe('start');
  });

  it('shows an engine without a count as found with no numbers', async () => {
    const { find } = setup();
    answer = async () => state({ found: true, matches: -1, current: -1 });
    find.search('alpha');
    await settle();
    expect(find.result).toMatchObject({ count: null, current: -1 });
  });

  it('an empty query and a close stop the session, and nothing is sent without one', async () => {
    const { find } = setup();
    find.clear();
    find.search('');
    expect(calls).toEqual([]);
    find.search('alpha');
    await settle();
    find.search('  ');
    await settle(0);
    expect(calls.map((call) => call.kind)).toEqual(['start', 'stop']);
    expect(find.result).toMatchObject({ count: 0, pending: false });
    find.search('alpha');
    await settle();
    find.clear();
    await settle(0);
    expect(calls.map((call) => call.kind)).toEqual(['start', 'stop', 'start', 'stop']);
  });

  it('closes its bar when the backend ends its session, not an older or another page', async () => {
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

  it('an end that arrives while a newer start is in flight does not close the new session', async () => {
    const { find, onEnded } = setup();
    answer = async () => state({ session: 1, found: true, matches: 3, current: 0 });
    find.search('alpha');
    await settle();
    let release!: () => void;
    answer = () => new Promise((resolve) => { release = () => resolve(state({ session: 3, found: true, matches: 1, current: 0 })); });
    find.search('beta');
    await settle();
    ended(2);
    release();
    await settle(0);
    expect(onEnded).not.toHaveBeenCalled();
    expect(find.result.count).toBe(1);
  });

  it('reports a failed request and clears the result', async () => {
    const { find, onError } = setup();
    answer = async () => { throw new Error('find runs on the page the browser pane shows'); };
    find.search('alpha');
    await settle();
    expect(onError).toHaveBeenCalledWith('find runs on the page the browser pane shows');
    expect(find.result).toMatchObject({ count: 0, pending: false });
  });

  it('in engine mode opens the engine find with the seed and never drives a bar', async () => {
    const { find, onError } = setup('engine');
    expect(find.openNative).toBeTypeOf('function');
    find.openNative?.('needle');
    find.search('ignored');
    await settle();
    expect(calls).toEqual([{ threadId: 'thread', kind: 'start', pageId: 'page-1', query: 'needle' }]);
    answer = async () => { throw new Error('no engine find'); };
    find.openNative?.('');
    await settle(0);
    expect(onError).toHaveBeenCalledWith('no engine find');
  });

  it('makes every engine call in order, one at a time, and counts each step', async () => {
    const { find } = setup();
    const releases: Array<() => void> = [];
    answer = (call) => new Promise((resolve) => {
      releases.push(() => resolve(state({ found: true, matches: 3, current: call.kind === 'start' ? 0 : 1 })));
    });
    find.search('alpha');
    await settle();
    expect(calls.map((call) => call.kind)).toEqual(['start']);
    releases.shift()!();
    await settle(0);
    find.next?.();
    find.next?.();
    find.next?.();
    find.previous?.();
    find.next?.();
    await settle(0);
    // One in flight; the presses wait, net of each other, and none is lost.
    expect(calls.map((call) => call.kind)).toEqual(['start', 'next']);
    while (releases.length) {
      releases.shift()!();
      await settle(0);
    }
    expect(calls.map((call) => call.kind)).toEqual(['start', 'next', 'next', 'next']);
  });

  it('opens and closes twice, each close stopping its own session', async () => {
    const { find } = setup();
    let session = 0;
    answer = async (call) => call.kind === 'stop'
      ? state({ active: false, session: ++session })
      : state({ session: ++session, found: true, matches: 2, current: 0 });
    for (let round = 0; round < 2; round++) {
      find.search('alpha');
      await settle();
      expect(find.result.count).toBe(2);
      find.clear();
      await settle(0);
      expect(find.result).toMatchObject({ count: 0, current: -1, pending: false });
    }
    expect(calls.map((call) => call.kind)).toEqual(['start', 'stop', 'start', 'stop']);
  });

  it('reads as open while the engine find UI is, and find.close stops it', async () => {
    const { find, onEnded } = setup('engine');
    registerPaneFindProvider('browser-main', find);
    expect(isPaneFindOpen('browser-main')).toBe(false);
    for (let round = 0; round < 2; round++) {
      openPaneFind('browser-main', { seed: '' });
      await settle(0);
      expect(find.nativeOpen).toBe(true);
      expect(isPaneFindOpen('browser-main')).toBe(true);
      closePaneFind('browser-main');
      await settle(0);
      expect(isPaneFindOpen('browser-main')).toBe(false);
    }
    expect(calls.map((call) => call.kind)).toEqual(['start', 'stop', 'start', 'stop']);
    // The backend's own end closes it too.
    answer = async () => state({ session: 7, found: false });
    openPaneFind('browser-main', { seed: '' });
    await settle(0);
    ended(8);
    expect(isPaneFindOpen('browser-main')).toBe(false);
    expect(onEnded).toHaveBeenCalledTimes(1);
  });

  it('a disposed provider hears no end and stops its session', async () => {
    const { find, onEnded } = setup();
    find.search('alpha');
    await settle();
    find.dispose();
    await settle(0);
    ended(5);
    expect(onEnded).not.toHaveBeenCalled();
    expect(calls.at(-1)?.kind).toBe('stop');
  });
});
