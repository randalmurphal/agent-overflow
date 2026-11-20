import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { browserActionCountMountsForTest, browserCompanionAct } from './browserActions';
import { browserAction } from './browserActionWire';
import {
  applyBrowserCompanionState,
  attachBrowserCompanion,
  browserThreadSnapshot,
  browserThreadState,
  browserPaneMountId,
  canUseNativeBrowser,
  hydrateBrowserCompanionState,
  readBrowserCompanionState,
  reconcileBrowserCompanionForPane,
  refreshBrowserCompanionStates,
  resetBrowserCompanionForTest,
} from './browserCompanion.svelte';
import { BROWSER_FIND_DEBOUNCE_MS, createBrowserFind } from './browserFind.svelte';
import { closeCompanion, companionForSource, installCompanionPanes, resetCompanionPanesForTest } from './companionPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { createPane, getFocusedPaneId, getPane, resetPanesForTest } from './panes.svelte';
import { makeThread } from '../../test/helpers/chat';
import { attachBrowserPaneForTest, openBrowserPaneForTest } from '../../test/helpers/browserPane';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { flushSync } from 'svelte';
import { __resetScopesForTest, setPageGrantsFromBootstrap } from '../transport/scopes';
import { noteThread, forgetBackendEntities } from '../transport/entityIndex';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import * as runMode from '../transport/runMode';
import { setCompactLayoutForTest } from './layoutMode.svelte';
import { setHostWindowFromBootstrap } from '../transport/pageHost';
import { getToasts, resetToastsForTest } from './toast.svelte';
import { browserCompanionState, browserStateThreadsForTest } from './browserCompanion.svelte';
import { refreshWatchedThreads, registerWatchedThreadSource } from './watchedThreads';
import { installDiagnosticsCapture } from '../../test/helpers/diagnostics';
import { DisconnectedError } from '../transport/wsClient';

const page = (url: string, title: string) => ({ id: 'page-1', url, title, canGoBack: false, canGoForward: false });

describe('browser companion state routing', () => {
  afterEach(() => { forgetBackendEntities('remote'); resetStagedBackends(); vi.restoreAllMocks(); });
  beforeEach(() => {
    resetBrowserCompanionForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
    installCompanionPanes();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
    createPane('main').replaceThread(makeThread({ id: 'thread-browser' }));
  });

  it('opens beside the owning thread and closes when its last page closes', () => {
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: true,
      pages: [page('file:///repo/demo.html', 'Demo')],
    });
    expect(companionForSource('main', 'browser')).toEqual({
      paneId: 'browser-main',
      kind: 'browser',
      sourcePaneId: 'main',
    });

    applyBrowserCompanionState({ kind: 'state', threadId: 'thread-browser', pages: [] });
    expect(companionForSource('main', 'browser')).toBeNull();
  });

  // A browser tab on the host holds the window's grants but not its native
  // view: it offers no pane and opens none from a push, as a connected
  // browser, and its pane source refuses without asking the backend.
  describe('in a page that is not the host window', () => {
    beforeEach(() => setHostWindowFromBootstrap(false));
    afterEach(() => setHostWindowFromBootstrap(true));

    it('offers, opens and attaches no native pane', async () => {
      expect(canUseNativeBrowser('thread-browser')).toBe(false);
      applyBrowserCompanionState({
        kind: 'state',
        threadId: 'thread-browser',
        activePageId: 'page-1',
        visible: true,
        pages: [page('file:///repo/demo.html', 'Demo')],
      });
      expect(companionForSource('main', 'browser')).toBeNull();
      const attach = vi.fn(async () => ({ id: 'mount-1', state: {} }));
      setBindingMock('BrowserCompanionPaneAttach', attach);
      const attachment = attachBrowserCompanion('thread-browser');
      await vi.waitFor(() => expect(attachment.error).toContain('native browser pane is unavailable'));
      attachment.release();
      expect(attach).not.toHaveBeenCalled();
      setHostWindowFromBootstrap(true);
      expect(canUseNativeBrowser('thread-browser')).toBe(true);
    });

    it('opens no pane when the thread is shown again after a switch away', () => {
      applyBrowserCompanionState({
        kind: 'state',
        threadId: 'thread-browser',
        activePageId: 'page-1',
        visible: true,
        pages: [page('file:///repo/demo.html', 'Demo')],
      });
      expect(companionForSource('main', 'browser')).toBeNull();
      for (let round = 0; round < 2; round += 1) {
        getPane('main')!.replaceThread(makeThread({ id: 'other-thread' }));
        reconcileBrowserCompanionForPane('main', 'other-thread');
        getPane('main')!.replaceThread(makeThread({ id: 'thread-browser' }));
        reconcileBrowserCompanionForPane('main', 'thread-browser');
        expect(companionForSource('main', 'browser')).toBeNull();
      }
    });
  });

  // The state of a thread no pane shows is not kept: shown again, it is
  // read again, and the pane opens from that read.
  it('reopens the pane in the host window when the thread is shown again after a switch away', async () => {
    const shown = {
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: true,
      pages: [page('file:///repo/demo.html', 'Demo')],
    };
    applyBrowserCompanionState(shown);
    expect(companionForSource('main', 'browser')?.kind).toBe('browser');
    closeCompanion('browser-main');
    const read = setBindingMock('BrowserCompanionThreadState', vi.fn(async (threadId: string) => (threadId === 'thread-browser' ? shown : { kind: 'state', threadId, pages: [] })));
    getPane('main')!.replaceThread(makeThread({ id: 'other-thread' }));
    reconcileBrowserCompanionForPane('main', 'other-thread');
    expect(companionForSource('main', 'browser')).toBeNull();
    getPane('main')!.replaceThread(makeThread({ id: 'thread-browser' }));
    reconcileBrowserCompanionForPane('main', 'thread-browser');
    await vi.waitFor(() => expect(companionForSource('main', 'browser')?.kind).toBe('browser'));
    expect(read).toHaveBeenCalledWith('thread-browser');
  });

  // The companion is a native view on the host; the compact screen never
  // shows it, and opening it there would reveal the pane and yank the phone
  // to the thread. State is still recorded, so a later desktop layout can
  // restore it; only the open is withheld.
  describe('under compact layout', () => {
    beforeEach(() => setCompactLayoutForTest(true));
    afterEach(() => setCompactLayoutForTest(false));

    it('does not open the companion from a state push', () => {
      applyBrowserCompanionState({
        kind: 'state',
        threadId: 'thread-browser',
        activePageId: 'page-1',
        visible: true,
        pages: [page('file:///repo/demo.html', 'Demo')],
      });
      expect(companionForSource('main', 'browser')).toBeNull();
      expect(getFocusedPaneId()).not.toBe('browser-main');
    });

    it('does not open the companion from the pane reconcile', () => {
      applyBrowserCompanionState({
        kind: 'state',
        threadId: 'returning-thread',
        visible: true,
        pages: [page('https://example.com', 'Example')],
      });
      createPane('main').replaceThread(makeThread({ id: 'returning-thread' }));
      reconcileBrowserCompanionForPane('main', 'returning-thread');
      expect(companionForSource('main', 'browser')).toBeNull();
    });

    it('still closes a companion the desktop layout opened before the resize', () => {
      setCompactLayoutForTest(false);
      applyBrowserCompanionState({
        kind: 'state',
        threadId: 'thread-browser',
        activePageId: 'page-1',
        visible: true,
        pages: [page('file:///repo/demo.html', 'Demo')],
      });
      expect(companionForSource('main', 'browser')?.kind).toBe('browser');
      setCompactLayoutForTest(true);
      applyBrowserCompanionState({ kind: 'state', threadId: 'thread-browser', pages: [] });
      expect(companionForSource('main', 'browser')).toBeNull();
    });
  });

  it('ignores pages owned by an unmounted thread', () => {
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'other-thread',
      pages: [page('https://example.com', 'Example')],
    });
    expect(companionForSource('main', 'browser')).toBeNull();
  });

  it('restores a live companion when its thread is mounted again', async () => {
    const live = {
      kind: 'state',
      threadId: 'returning-thread',
      visible: true,
      pages: [page('https://example.com', 'Example')],
    };
    applyBrowserCompanionState(live);
    setBindingMock('BrowserCompanionThreadState', vi.fn(async () => live));
    createPane('main').replaceThread(makeThread({ id: 'returning-thread' }));
    reconcileBrowserCompanionForPane('main', 'returning-thread');
    await vi.waitFor(() => expect(companionForSource('main', 'browser')?.kind).toBe('browser'));
  });

  it('hides and restores the live companion without closing its pages', () => {
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: true,
      pages: [page('https://example.com', 'Example')],
    });
    expect(companionForSource('main', 'browser')?.kind).toBe('browser');

    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: false,
      pages: [page('https://example.com', 'Example')],
    });
    expect(companionForSource('main', 'browser')).toBeNull();

    // A navigation state update while hidden must not reopen the pane.
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: false,
      pages: [page('https://example.com/next', 'Next')],
    });
    expect(companionForSource('main', 'browser')).toBeNull();

    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: true,
      pages: [page('https://example.com/next', 'Next')],
    });
    expect(companionForSource('main', 'browser')?.kind).toBe('browser');
  });

  it('keeps background pages hidden until explicitly presented', () => {
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: false,
      pages: [page('https://example.com', 'Example')],
    });
    expect(companionForSource('main', 'browser')).toBeNull();

    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: true,
      pages: [page('https://example.com', 'Example')],
    });
    expect(companionForSource('main', 'browser')?.kind).toBe('browser');
  });

  it('hydrates from the mount effect once host presence lands, not never', () => {
    // Pane mount runs before the WS client has fetched the manifest. The
    // effect re-runs when the answer lands; the thread must not have been
    // pinned as hydrated on the placeholder.
    const read = setBindingMock('BrowserCompanionThreadState', vi.fn(async () => ({
      kind: 'state', threadId: 'thread-browser', activePageId: '', visible: false, pages: [],
    })));
    __resetScopesForTest();
    const dispose = $effect.root(() => {
      $effect(() => {
        reconcileBrowserCompanionForPane('main', 'thread-browser');
      });
    });
    flushSync();
    expect(read).not.toHaveBeenCalled();

    setPageGrantsFromBootstrap(false);
    flushSync();
    expect(read).toHaveBeenCalledTimes(1);
    dispose();
  });

  // State frames and call answers carry the backend's Seq; the transport can
  // write a newer frame ahead of a reply.
  describe('ordering answers against frames', () => {
    const open = (seq: number) => ({
      kind: 'state', threadId: 'thread-browser', activePageId: 'page-1', visible: true, seq,
      pages: [page('https://example.com', 'Example')],
    });
    const closed = (seq: number) => ({ kind: 'state', threadId: 'thread-browser', visible: true, pages: [], seq });

    it('keeps a newer frame that arrived before an action reply', async () => {
      applyBrowserCompanionState(open(1));
      setBindingMock('BrowserCompanionDo', async () => {
        applyBrowserCompanionState(closed(3));
        return open(2);
      });
      await browserCompanionAct('thread-browser', browserAction.hide());
      expect(companionForSource('main', 'browser')).toBeNull();
    });

    it('applies an action reply, then a newer frame after it', async () => {
      applyBrowserCompanionState(open(1));
      setBindingMock('BrowserCompanionDo', async () => open(2));
      await browserCompanionAct('thread-browser', browserAction.hide());
      expect(companionForSource('main', 'browser')).not.toBeNull();
      applyBrowserCompanionState(closed(3));
      expect(companionForSource('main', 'browser')).toBeNull();
    });

    it('keeps a newer zero-page frame that arrived during the hydrate read', async () => {
      let answer: (value: unknown) => void = () => {};
      setBindingMock('BrowserCompanionThreadState', () => new Promise((resolve) => { answer = resolve; }));
      const read = readBrowserCompanionState('thread-browser');
      applyBrowserCompanionState(closed(5));
      answer(open(4));
      await read;
      expect(browserThreadState('thread-browser')?.pages).toEqual([]);
      expect(companionForSource('main', 'browser')).toBeNull();
    });

    it('keeps a newer frame that arrived during the pane attach', async () => {
      setBindingMock('BrowserCompanionPaneAttach', async () => {
        applyBrowserCompanionState(closed(7));
        return { id: 'mount-1', state: open(6) };
      });
      setBindingMock('BrowserCompanionPaneDetach', async () => {});
      const attachment = attachBrowserCompanion('thread-browser');
      await vi.waitFor(() => expect(attachment.current).not.toBeNull());
      expect(browserThreadState('thread-browser')?.pages).toEqual([]);
      attachment.release();
    });

    it('keeps the current mount when a superseded attach answers last', async () => {
      const replies: Array<(mount: string) => void> = [];
      setBindingMock('BrowserCompanionPaneAttach', () => new Promise((resolve) => {
        replies.push((mount) => resolve({ id: mount, state: open(6) }));
      }));
      const detach = setBindingMock('BrowserCompanionPaneDetach', vi.fn(async () => {}));
      // The pane closes and opens again while its first attach is pending.
      attachBrowserCompanion('thread-browser').release();
      const second = attachBrowserCompanion('thread-browser');
      await vi.waitFor(() => expect(replies).toHaveLength(2));
      replies[1]('mount-2');
      await vi.waitFor(() => expect(second.current).not.toBeNull());
      replies[0]('mount-1');
      await vi.waitFor(() => expect(detach).toHaveBeenCalledWith('mount-1'));
      expect(browserPaneMountId('thread-browser')).toBe('mount-2');
      expect(detach).toHaveBeenCalledTimes(1);
      second.release();
      await vi.waitFor(() => expect(detach).toHaveBeenLastCalledWith('mount-2'));
      expect(browserPaneMountId('thread-browser')).toBeNull();
    });

    // A mount id addresses every ordered action and the detach: an attach
    // that names none is the pane's failure, never a mount to number.
    it('takes no mount from an attach answer that names none', async () => {
      setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: '', state: open(6) }));
      setBindingMock('BrowserCompanionPaneDetach', async () => {});
      const attachment = attachBrowserCompanion('thread-browser');
      await vi.waitFor(() => expect(attachment.error).toContain('answered with no mount'));
      expect(attachment.current).toBeNull();
      expect(browserPaneMountId('thread-browser')).toBeNull();
      attachment.release();
    });

    it('takes the attach answer when no newer frame arrived', async () => {
      setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: 'mount-1', state: open(6) }));
      setBindingMock('BrowserCompanionPaneDetach', async () => {});
      const attachment = attachBrowserCompanion('thread-browser');
      await vi.waitFor(() => expect(attachment.current).not.toBeNull());
      expect(browserThreadState('thread-browser')?.pages).toHaveLength(1);
      applyBrowserCompanionState(closed(8));
      expect(browserThreadState('thread-browser')?.pages).toEqual([]);
      attachment.release();
    });
  });

  // The state channel has no replay: tabs opened while this window was
  // away are known only from a read on the new connection.
  describe('across a connection edge', () => {
    const tab = { id: 'page-1', url: 'https://example.com/', title: 'Example', canGoBack: false, canGoForward: false };
    const none = (seq: number) => ({ kind: 'state', threadId: 'thread-browser', pages: [], seq });
    const hidden = (seq: number) => ({ kind: 'state', threadId: 'thread-browser', activePageId: 'page-1', visible: false, pages: [tab], seq });

    it('reads a thread with a pane again, and finds the tab an agent opened meanwhile', async () => {
      const answers = [none(1), hidden(2)];
      const read = setBindingMock('BrowserCompanionThreadState', vi.fn(async () => answers.shift()));
      hydrateBrowserCompanionState('thread-browser');
      await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(1));
      hydrateBrowserCompanionState('thread-browser');
      expect(read).toHaveBeenCalledTimes(1);

      refreshBrowserCompanionStates();
      await vi.waitFor(() => expect(browserCompanionState('thread-browser')?.pages).toHaveLength(1));
      expect(read).toHaveBeenCalledTimes(2);
    });

    // A push sent while the window was away is lost, so an answer to a call
    // made before the edge may be older than what the backend holds now.
    it('admits no answer to a call made before the edge', async () => {
      applyBrowserCompanionState(hidden(1));
      const read = setBindingMock('BrowserCompanionThreadState', vi.fn(() => new Promise(() => {})));
      setBindingMock('BrowserCompanionDo', async () => {
        refreshBrowserCompanionStates();
        return none(2);
      });
      await browserCompanionAct('thread-browser', browserAction.hide());
      expect(read).toHaveBeenCalledTimes(1);
      expect(browserThreadState('thread-browser')?.pages).toHaveLength(1);
      expect(browserThreadSnapshot('thread-browser')).toEqual({ status: 'unknown' });
    });
  });

  describe('a failed hydrate read', () => {
    const diagnostics = installDiagnosticsCapture();

    it('is reported, and read again by the next caller', async () => {
      const read = setBindingMock('BrowserCompanionThreadState', vi.fn(async () => {
        throw new Error('the browser host stopped');
      }));
      hydrateBrowserCompanionState('thread-browser');
      await vi.waitFor(async () => expect(await diagnostics.all()).toEqual([
        expect.objectContaining({ message: 'A browser state read failed', detail: expect.stringContaining('browser host stopped') }),
      ]));
      expect(browserThreadSnapshot('thread-browser')).toEqual({ status: 'unknown' });
      hydrateBrowserCompanionState('thread-browser');
      await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    });

    it('reports an answer that carries no state for the thread', async () => {
      setBindingMock('BrowserCompanionThreadState', async () => ({ kind: 'state', threadId: 'another-thread', pages: [] }));
      hydrateBrowserCompanionState('thread-browser');
      await vi.waitFor(async () => expect(await diagnostics.all()).toEqual([
        expect.objectContaining({ message: 'A browser state read failed', detail: expect.stringContaining('no state for this conversation') }),
      ]));
    });

    it('leaves a lost connection to the connection banner', async () => {
      const read = setBindingMock('BrowserCompanionThreadState', vi.fn(async () => {
        throw new DisconnectedError();
      }));
      hydrateBrowserCompanionState('thread-browser');
      await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(1));
      await vi.waitFor(() => expect(browserThreadSnapshot('thread-browser')).toEqual({ status: 'unknown' }));
      // The read's failure has settled once a second caller reads again.
      hydrateBrowserCompanionState('thread-browser');
      await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(2));
      expect(await diagnostics.all()).toEqual([]);
    });
  });

  it('does not ask the local host to hydrate another computer’s native browser', () => {
    const read = setBindingMock('BrowserCompanionThreadState', vi.fn());
    stageBackend({ id: 'remote', backendId: 'remote' });
    noteThread('remote-browser', 'remote');
    hydrateBrowserCompanionState('remote-browser');
    hydrateBrowserCompanionState('unknown-browser');
    expect(read).not.toHaveBeenCalled();
  });

  it('does not treat a frontend controller’s admin grant as a native execution host', () => {
    const read = setBindingMock('BrowserCompanionThreadState', vi.fn());
    vi.spyOn(runMode, 'isFrontendOnly').mockReturnValue(true);
    hydrateBrowserCompanionState('thread-browser');
    expect(read).not.toHaveBeenCalled();
  });
});

describe('find events from the native page view', () => {
  beforeEach(() => openBrowserPaneForTest('thread-browser', 'mount-1'));

  it('routes the end of a find to the thread and leaves the pane state alone', async () => {
    vi.useFakeTimers();
    let seq = 0;
    setBindingMock('BrowserCompanionFind', async (_thread: string, action: { seq: number }) => {
      seq = action.seq;
      return { pageId: 'page-1', incarnation: 1, mount: 'mount-1', seq, document: 1, session: 1, active: true, query: 'alpha', found: true, matches: 2, current: 0, capped: false };
    });
    const closeBar = vi.fn();
    const find = createBrowserFind({
      threadId: 'thread-browser', paneId: 'browser-main', mode: 'bar', pageId: () => 'page-1', mount: () => 'mount-1', closeBar, onCleanupError: vi.fn(),
    });
    try {
      find.search('alpha');
      await vi.advanceTimersByTimeAsync(BROWSER_FIND_DEBOUNCE_MS);
      expect(find.result.count).toBe(2);
      applyBrowserCompanionState({
        kind: 'find',
        threadId: 'thread-browser',
        pageId: 'page-1',
        find: { pageId: 'page-1', incarnation: 1, mount: 'mount-1', seq, document: 1, session: 2, foreign: false, active: false, query: '', found: false, matches: 0, current: -1, capped: false },
      });
      expect(closeBar).toHaveBeenCalledTimes(1);
      expect(companionForSource('main', 'browser')).not.toBeNull();
    } finally {
      find.dispose();
      vi.useRealTimers();
    }
  });
});

describe('tab lifecycle in the browser companion store', () => {
  const THREAD = 'thread-browser';
  const tab = (id: string, extra: Record<string, unknown> = {}) => ({
    id, url: 'about:blank', title: '', canGoBack: false, canGoForward: false, ...extra,
  });

  beforeEach(() => {
    resetBrowserCompanionForTest();
    resetToastsForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
    installCompanionPanes();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
    createPane('main').replaceThread(makeThread({ id: THREAD }));
  });
  afterEach(() => {
    resetBrowserCompanionForTest();
    resetToastsForTest();
  });

  it('raises a teardown failure after the last tab closed as a toast that stays', () => {
    applyBrowserCompanionState({ kind: 'state', threadId: THREAD, activePageId: 'a', visible: true, pages: [tab('a')] });
    applyBrowserCompanionState({ kind: 'state', threadId: THREAD, visible: false, pages: [] });
    expect(companionForSource('main', 'browser')).toBeNull();
    applyBrowserCompanionState({ kind: 'teardown', threadId: THREAD, pageId: 'a', error: 'profile would not stop' });
    const toasts = getToasts();
    expect(toasts).toHaveLength(1);
    expect(toasts[0].message).toContain('profile would not stop');
    expect(toasts[0].duration).toBe(0);
  });
});

// The channel carries every thread's state; this window keeps it only for
// the threads a surface shows (the watched set), so visiting threads leaves
// nothing behind for the ones no surface shows any more.
describe('browser state kept per surface', () => {
  const page = { id: 'p', url: 'https://example.test/', title: 'P', canGoBack: false, canGoForward: false };
  const state = (threadId: string, seq: number) => ({ kind: 'state', threadId, activePageId: 'p', visible: false, seq, pages: [page] });
  let shown: Set<string>;
  let unregister: () => void;
  beforeEach(() => {
    resetBrowserCompanionForTest();
    shown = new Set();
    unregister = registerWatchedThreadSource(() => shown);
  });
  afterEach(() => {
    unregister();
    resetBrowserCompanionForTest();
  });

  function show(...threadIds: string[]): void {
    shown = new Set(threadIds);
    refreshWatchedThreads();
  }

  it('keeps state only for the threads a surface shows, and orders a thread shown again afresh', () => {
    for (let index = 0; index < 20; index += 1) {
      show(`thread-${index}`);
      applyBrowserCompanionState(state(`thread-${index}`, 5));
    }
    applyBrowserCompanionState(state('thread-unseen', 1));
    expect(browserStateThreadsForTest()).toEqual(['thread-19']);

    // Shown again, its next state is admitted whatever its number: the
    // order it had was forgotten with it.
    show('thread-19', 'thread-0');
    applyBrowserCompanionState(state('thread-0', 1));
    expect(browserThreadState('thread-0')?.seq).toBe(1);
  });

  it('drops a read in flight for a thread no surface shows, and reads again once one does', async () => {
    const answers: Array<() => void> = [];
    const read = setBindingMock('BrowserCompanionThreadState', vi.fn(() => new Promise((resolve) => answers.push(() => resolve(state('thread-a', 3))))));
    show('thread-a');
    const first = readBrowserCompanionState('thread-a');
    show();
    expect(browserStateThreadsForTest()).toEqual([]);
    answers[0]();
    await first;
    expect(browserThreadState('thread-a')).toBeNull();

    show('thread-a');
    const second = readBrowserCompanionState('thread-a');
    expect(read).toHaveBeenCalledTimes(2);
    answers[1]();
    await second;
    expect(browserThreadState('thread-a')?.seq).toBe(3);
  });
  // An action's answer outlives the window's
  // observation of its thread. Left and shown again, the thread is observed
  // afresh, and the old answer is not its state: the next read asks.
  it('admits no action answer from before the thread left and came back, and reads it again', async () => {
    let reply: (state: unknown) => void = () => {};
    setBindingMock('BrowserCompanionDo', vi.fn(() => new Promise((resolve) => { reply = resolve; })));
    const read = setBindingMock('BrowserCompanionThreadState', vi.fn(async () => state('thread-a', 7)));
    show('thread-a');
    const acted = browserCompanionAct('thread-a', browserAction.hide());
    show();
    applyBrowserCompanionState(state('thread-a', 6));
    show('thread-a');
    reply(state('thread-a', 4));
    await acted;
    expect(browserThreadState('thread-a')).toBeNull();
    expect(browserThreadSnapshot('thread-a').status).toBe('unknown');
    await readBrowserCompanionState('thread-a');
    expect(read).toHaveBeenCalledTimes(1);
    expect(browserThreadState('thread-a')?.seq).toBe(7);
  });

  // A download toast's action names a thread no surface shows: it keeps
  // nothing for it.
  it('keeps no action count for an action on a thread with no pane', async () => {
    setBindingMock('BrowserCompanionDo', vi.fn(async () => state('thread-gone', 2)));
    show('thread-other');
    await browserCompanionAct('thread-gone', browserAction.hide());
    expect(browserActionCountMountsForTest()).toEqual([]);
    expect(browserStateThreadsForTest()).toEqual([]);
  });

  it('ends a pane\'s action count with its mount', async () => {
    setBindingMock('BrowserCompanionDo', vi.fn(async () => state('thread-a', 2)));
    show('thread-a');
    const attachment = await attachBrowserPaneForTest('thread-a', 'mount-a');
    await browserCompanionAct('thread-a', browserAction.hide());
    expect(browserActionCountMountsForTest()).toEqual(['mount-a']);
    attachment.release();
    await vi.waitFor(() => expect(browserActionCountMountsForTest()).toEqual([]));
  });
});
