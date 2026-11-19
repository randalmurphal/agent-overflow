import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  applyBrowserCompanionState,
  attachBrowserCompanion,
  browserCompanionAct,
  browserPaneMountId,
  openBrowserTab,
  canUseNativeBrowser,
  hydrateBrowserCompanionState,
  closeFocusedBrowserTab,
  reconcileBrowserCompanionForPane,
  refreshBrowserCompanionStates,
  resetBrowserCompanionForTest,
  setBrowserPaneMountForTest,
  toggleBrowserFromHeader,
} from './browserCompanion.svelte';
import { addressFocusIntent, takeAddressFocus } from './browserAddressFocus.svelte';
import { BROWSER_FIND_DEBOUNCE_MS, createBrowserFind } from './browserFind.svelte';
import { closeCompanion, companionForSource, installCompanionPanes, resetCompanionPanesForTest } from './companionPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { createPane, focusPane, getFocusedPaneId, getPane, resetPanesForTest } from './panes.svelte';
import { makeCommandContext } from './builtinCommands.svelte';
import { notifyTerminalFocus } from '../components/terminal/terminalStore.svelte';
import { makeThread } from '../../test/helpers/chat';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { flushSync } from 'svelte';
import { __resetScopesForTest, setPageGrantsFromBootstrap } from '../transport/scopes';
import { noteThread, forgetBackendEntities } from '../transport/entityIndex';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import * as runMode from '../transport/runMode';
import { setCompactLayoutForTest } from './layoutMode.svelte';
import { setHostWindowFromBootstrap } from '../transport/pageHost';
import { getToasts, resetToastsForTest } from './toast.svelte';
import { browserCompanionState } from './browserCompanion.svelte';

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

  it('reopens the pane in the host window when the thread is shown again after a switch away', () => {
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: true,
      pages: [page('file:///repo/demo.html', 'Demo')],
    });
    expect(companionForSource('main', 'browser')?.kind).toBe('browser');
    closeCompanion('browser-main');
    getPane('main')!.replaceThread(makeThread({ id: 'other-thread' }));
    reconcileBrowserCompanionForPane('main', 'other-thread');
    expect(companionForSource('main', 'browser')).toBeNull();
    getPane('main')!.replaceThread(makeThread({ id: 'thread-browser' }));
    reconcileBrowserCompanionForPane('main', 'thread-browser');
    expect(companionForSource('main', 'browser')?.kind).toBe('browser');
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

  it('restores a live companion when its thread is mounted again', () => {
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'returning-thread',
      visible: true,
      pages: [page('https://example.com', 'Example')],
    });
    createPane('main').replaceThread(makeThread({ id: 'returning-thread' }));
    reconcileBrowserCompanionForPane('main', 'returning-thread');
    expect(companionForSource('main', 'browser')?.kind).toBe('browser');
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
      await browserCompanionAct('thread-browser', { kind: 'reload', pageId: 'page-1' });
      expect(companionForSource('main', 'browser')).toBeNull();
    });

    it('applies an action reply, then a newer frame after it', async () => {
      applyBrowserCompanionState(open(1));
      setBindingMock('BrowserCompanionDo', async () => open(2));
      await browserCompanionAct('thread-browser', { kind: 'reload', pageId: 'page-1' });
      expect(companionForSource('main', 'browser')).not.toBeNull();
      applyBrowserCompanionState(closed(3));
      expect(companionForSource('main', 'browser')).toBeNull();
    });

    it('keeps a newer zero-page frame that arrived during the hydrate read', async () => {
      let answer: (value: unknown) => void = () => {};
      setBindingMock('BrowserCompanionThreadState', () => new Promise((resolve) => { answer = resolve; }));
      hydrateBrowserCompanionState('thread-browser');
      applyBrowserCompanionState(closed(5));
      answer(open(4));
      await vi.waitFor(() => expect(answer).toBeTruthy());
      await Promise.resolve();
      await Promise.resolve();
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
      expect(attachment.current?.state.pages).toEqual([]);
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

    it('takes the attach answer when no newer frame arrived', async () => {
      setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: 'mount-1', state: open(6) }));
      setBindingMock('BrowserCompanionPaneDetach', async () => {});
      const attachment = attachBrowserCompanion('thread-browser');
      await vi.waitFor(() => expect(attachment.current).not.toBeNull());
      expect(attachment.current?.state.pages).toHaveLength(1);
      applyBrowserCompanionState(closed(8));
      expect(attachment.current?.state.pages).toEqual([]);
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

    it('the header button reads a state it does not know before it opens a tab', async () => {
      let answer: (state: unknown) => void = () => {};
      setBindingMock('BrowserCompanionThreadState', () => new Promise((resolve) => { answer = resolve; }));
      const act = setBindingMock('BrowserCompanionDo', vi.fn(async (_thread: string, action: { kind: string }) => (
        action.kind === 'show' ? { ...hidden(3), visible: true } : none(3)
      )));
      const toggled = toggleBrowserFromHeader('thread-browser');
      await Promise.resolve();
      expect(act).not.toHaveBeenCalled();
      answer(hidden(2));
      await toggled;
      expect(act).toHaveBeenCalledTimes(1);
      expect(act.mock.calls[0][1]).toMatchObject({ kind: 'show', pageId: 'page-1' });
    });

    it('the header button opens a tab once the state is known to have none', async () => {
      setBindingMock('BrowserCompanionThreadState', async () => none(1));
      const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => hidden(2)));
      await toggleBrowserFromHeader('thread-browser');
      expect(act).toHaveBeenCalledTimes(1);
      expect(act.mock.calls[0][1]).toMatchObject({ kind: 'new' });
    });

    it('the header button opens nothing when the state cannot be read, and says so', async () => {
      resetToastsForTest();
      setBindingMock('BrowserCompanionThreadState', async () => { throw new Error('backend restarting'); });
      const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => hidden(2)));
      await toggleBrowserFromHeader('thread-browser');
      expect(act).not.toHaveBeenCalled();
      expect(getToasts().map((toast) => toast.message)).toContain("The browser's tabs could not be read: backend restarting");
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

describe('pane.close on a browser companion', () => {
  beforeEach(() => {
    resetBrowserCompanionForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
    installCompanionPanes();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
    createPane('main').replaceThread(makeThread({ id: 'thread-browser' }));
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: true,
      pages: [{ id: 'page-1', url: 'https://example.com', title: 'Example', canGoBack: false, canGoForward: false }],
    });
  });

  it('closes the active tab, and the pane only once no tab remains', async () => {
    const calls: unknown[][] = [];
    setBindingMock('BrowserCompanionDo', async (threadId: string, action: { kind: string; pageId: string }) => {
      calls.push([threadId, action.kind, action.pageId]);
      return { kind: 'state', threadId, activePageId: '', visible: true, pages: [] };
    });
    focusPane('browser-main');

    expect(closeFocusedBrowserTab()).toBe(true);
    expect(calls).toEqual([['thread-browser', 'close', 'page-1']]);
    // Still open: the pane follows the backend's answer, not the keystroke.
    expect(companionForSource('main', 'browser')).not.toBeNull();
    await vi.waitFor(() => expect(companionForSource('main', 'browser')).toBeNull());
  });

  // The backend moved to another tab while this client was away: the
  // reattach's answer is what the pane shows and what the chord closes.
  it('closes the tab the reattached pane shows after the backend changed tabs', async () => {
    const tab = (id: string) => ({ id, url: `https://example.com/${id}`, title: id, canGoBack: false, canGoForward: false });
    applyBrowserCompanionState({
      kind: 'state', threadId: 'thread-browser', activePageId: 'page-1', visible: true, seq: 1,
      pages: [tab('page-1'), tab('page-2')],
    });
    setBindingMock('BrowserCompanionPaneAttach', async () => ({
      id: 'mount-1',
      state: {
        kind: 'state', threadId: 'thread-browser', activePageId: 'page-2', visible: true, seq: 2,
        pages: [tab('page-1'), tab('page-2')],
      },
    }));
    setBindingMock('BrowserCompanionPaneDetach', async () => {});
    const closes: string[] = [];
    setBindingMock('BrowserCompanionDo', async (threadId: string, action: { kind: string; pageId: string }) => {
      closes.push(action.pageId);
      return { kind: 'state', threadId, activePageId: 'page-1', visible: true, seq: 3, pages: [tab('page-1')] };
    });
    const attachment = attachBrowserCompanion('thread-browser');
    await vi.waitFor(() => expect(attachment.current?.state.activePageId).toBe('page-2'));
    focusPane('browser-main');
    expect(closeFocusedBrowserTab()).toBe(true);
    await vi.waitFor(() => expect(closes).toEqual(['page-2']));
    attachment.release();
  });

  describe('right after a tab selection or another close', () => {
    const tab = (id: string) => ({ id, url: `https://example.com/${id}`, title: id, canGoBack: false, canGoForward: false });
    const state = (active: string, ids: string[], seq: number) => ({
      kind: 'state', threadId: 'thread-browser', activePageId: active, visible: true, seq, pages: ids.map(tab),
    });
    let sent: Array<{ kind: string; pageId: string }>;
    let answers: Array<() => void>;

    beforeEach(() => {
      applyBrowserCompanionState(state('a', ['a', 'b', 'c'], 1));
      setBrowserPaneMountForTest('thread-browser', 'mount-1');
      sent = [];
      answers = [];
      // The backend applies each action as it arrives and answers when the
      // test lets it.
      let current = state('a', ['a', 'b', 'c'], 1);
      setBindingMock('BrowserCompanionDo', vi.fn(async (_thread: string, action: { kind: string; pageId: string }) => {
        sent.push({ kind: action.kind, pageId: action.pageId });
        const ids = current.pages.map((page) => page.id);
        if (action.kind === 'activate') current = state(action.pageId, ids, current.seq + 1);
        if (action.kind === 'close') {
          const left = ids.filter((id) => id !== action.pageId);
          current = state(current.activePageId === action.pageId ? left[0] ?? '' : current.activePageId, left, current.seq + 1);
        }
        const answer = current;
        await new Promise<void>((resolve) => answers.push(resolve));
        return answer;
      }));
      focusPane('browser-main');
    });
    afterEach(() => {
      for (const answer of answers.splice(0)) answer();
    });

    const answerAll = async (): Promise<void> => {
      while (answers.length > 0) {
        for (const answer of answers.splice(0)) answer();
        await new Promise((resolve) => setTimeout(resolve, 0));
      }
    };

    it('closes the tab the user just selected, not the one the last state named', async () => {
      void browserCompanionAct('thread-browser', { kind: 'activate', pageId: 'b' });
      expect(closeFocusedBrowserTab()).toBe(true);
      // Nothing is closed before the selection has answered.
      expect(sent).toEqual([{ kind: 'activate', pageId: 'b' }]);
      await answerAll();
      expect(sent).toEqual([{ kind: 'activate', pageId: 'b' }, { kind: 'close', pageId: 'b' }]);
    });

    it('closes one tab per press when presses come faster than the answers', async () => {
      expect(closeFocusedBrowserTab()).toBe(true);
      expect(closeFocusedBrowserTab()).toBe(true);
      await answerAll();
      expect(sent).toEqual([{ kind: 'close', pageId: 'a' }, { kind: 'close', pageId: 'b' }]);
    });

    it('leaves a close from the tab menu on the tab it names, at once', async () => {
      void browserCompanionAct('thread-browser', { kind: 'activate', pageId: 'b' });
      void browserCompanionAct('thread-browser', { kind: 'close', pageId: 'c' });
      expect(sent).toEqual([{ kind: 'activate', pageId: 'b' }, { kind: 'close', pageId: 'c' }]);
      await answerAll();
    });
  });

  it('falls through when the focused pane is not a browser companion', () => {
    focusPane('main');
    expect(closeFocusedBrowserTab()).toBe(false);
  });

  it('returns a refused action with the error behind it', async () => {
    const failure = new Error('page is gone');
    setBindingMock('BrowserCompanionDo', async () => {
      throw failure;
    });
    expect(await browserCompanionAct('thread-browser', { kind: 'close', pageId: 'page-1' }))
      .toEqual({ error: 'page is gone', cause: failure });
  });
});

describe('accelerators from the native page view', () => {
  beforeEach(() => {
    resetBrowserCompanionForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
    installCompanionPanes();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
    createPane('main').replaceThread(makeThread({ id: 'thread-browser' }));
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: true,
      pages: [{ id: 'page-1', url: 'https://example.com', title: 'Example', canGoBack: false, canGoForward: false }],
    });
    setBrowserPaneMountForTest('thread-browser', 'mount-1');
  });

  it('focuses the companion and replays the chord as a window keydown', () => {
    focusPane('main');
    const seen: KeyboardEvent[] = [];
    const listener = (event: KeyboardEvent) => seen.push(event);
    window.addEventListener('keydown', listener);
    try {
      applyBrowserCompanionState({
        kind: 'accelerator',
        threadId: 'thread-browser',
        mount: 'mount-1',
        accelerator: { key: 'w', meta: true },
      });
    } finally {
      window.removeEventListener('keydown', listener);
    }
    expect(getFocusedPaneId()).toBe('browser-main');
    expect(seen).toHaveLength(1);
    expect(seen[0].key).toBe('w');
    expect(seen[0].metaKey).toBe(true);
    expect(seen[0].ctrlKey).toBe(false);
  });

  // DOM focus left in the SPA (a composer or terminal caret) is where the
  // user returns to; a replayed chord that moves no focus leaves it there.
  it('leaves DOM focus where it was across a replayed chord', () => {
    const composer = document.createElement('textarea');
    document.body.appendChild(composer);
    composer.focus();
    const blurred = vi.fn();
    composer.addEventListener('blur', blurred);
    let focusedAtReplay: Element | null = null;
    const listener = () => { focusedAtReplay = document.activeElement; };
    window.addEventListener('keydown', listener);
    try {
      applyBrowserCompanionState({
        kind: 'accelerator',
        threadId: 'thread-browser',
        mount: 'mount-1',
        accelerator: { key: 'f', ctrl: true },
      });
      expect(focusedAtReplay).toBe(composer);
      expect(document.activeElement).toBe(composer);
      expect(blurred).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener('keydown', listener);
      composer.remove();
    }
  });

  // The engine hands keyboard focus back to the SPA when it claims a chord,
  // so a terminal caret the SPA kept may be focused again by the time the
  // chord arrives: the chord was typed in the page, not the terminal.
  it('resolves a replayed chord outside a terminal the SPA kept focus in', () => {
    notifyTerminalFocus('main', true);
    const hasFocus = vi.spyOn(document, 'hasFocus').mockReturnValue(true);
    let during: boolean | null = null;
    const listener = () => { during = makeCommandContext(getPane('main') ?? null, {}).flags.terminalFocus; };
    window.addEventListener('keydown', listener);
    try {
      expect(makeCommandContext(getPane('main') ?? null, {}).flags.terminalFocus).toBe(true);
      applyBrowserCompanionState({
        kind: 'accelerator',
        threadId: 'thread-browser',
        mount: 'mount-1',
        accelerator: { key: 'w', ctrl: true },
      });
      expect(during).toBe(false);
      expect(makeCommandContext(getPane('main') ?? null, {}).flags.terminalFocus).toBe(true);
    } finally {
      window.removeEventListener('keydown', listener);
      hasFocus.mockRestore();
      notifyTerminalFocus('main', false);
    }
  });

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

  // Every host client receives the event; a client whose pane mount did not
  // show the page, or that holds none, replays nothing.
  it.each([
    ['another client\'s mount', 'mount-2'],
    ['no mount named', undefined],
  ])('does not replay a chord routed to %s', (_name, mount) => {
    const seen: KeyboardEvent[] = [];
    const listener = (event: KeyboardEvent) => seen.push(event);
    window.addEventListener('keydown', listener);
    try {
      applyBrowserCompanionState({ kind: 'accelerator', threadId: 'thread-browser', mount, accelerator: { key: 'w', ctrl: true } });
    } finally {
      window.removeEventListener('keydown', listener);
    }
    expect(seen).toEqual([]);
    expect(getFocusedPaneId()).not.toBe('browser-main');
  });

  it('does not replay a chord in a client with no pane mounted', () => {
    resetBrowserCompanionForTest();
    const seen: KeyboardEvent[] = [];
    const listener = (event: KeyboardEvent) => seen.push(event);
    window.addEventListener('keydown', listener);
    try {
      applyBrowserCompanionState({ kind: 'accelerator', threadId: 'thread-browser', mount: 'mount-1', accelerator: { key: 'w', ctrl: true } });
      applyBrowserCompanionState({ kind: 'accelerator', threadId: 'thread-browser', accelerator: { key: 'w', ctrl: true } });
    } finally {
      window.removeEventListener('keydown', listener);
    }
    expect(seen).toEqual([]);
  });

  it('is inert without a chord', () => {
    const seen: KeyboardEvent[] = [];
    const listener = (event: KeyboardEvent) => seen.push(event);
    window.addEventListener('keydown', listener);
    try {
      applyBrowserCompanionState({ kind: 'accelerator', threadId: 'thread-browser', mount: 'mount-1' });
    } finally {
      window.removeEventListener('keydown', listener);
    }
    expect(seen).toEqual([]);
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

  it('opens the pane from no tab with the creating tab, then names the tab for focus', async () => {
    let answer!: () => void;
    setBindingMock('BrowserCompanionDo', vi.fn(async (_thread: string, action: { kind: string }) => {
      expect(action.kind).toBe('new');
      // The backend pushes the creating tab, shown, before it answers.
      applyBrowserCompanionState({ kind: 'state', threadId: THREAD, seq: 1, activePageId: 'n', visible: true, pages: [tab('n', { creating: true })] });
      await new Promise<void>((resolve) => { answer = resolve; });
      return { kind: 'state', threadId: THREAD, seq: 2, activePageId: 'n', visible: true, pages: [tab('n')], pageId: 'n' };
    }));
    const opened = openBrowserTab(THREAD, { focusAddress: true });
    await vi.waitFor(() => expect(companionForSource('main', 'browser')).not.toBeNull());
    expect(addressFocusIntent(THREAD)).toBe('');
    // Not while it is created: there is no page to take an address yet.
    expect(takeAddressFocus(THREAD, { id: 'n', creating: true })).toBe(false);
    answer();
    expect(await opened).toBeNull();
    expect(addressFocusIntent(THREAD)).toBe('n');
    expect(takeAddressFocus(THREAD, { id: 'n' })).toBe(true);
    // Once.
    expect(takeAddressFocus(THREAD, { id: 'n' })).toBe(false);
  });

  it('drops the focus when the user moves on before the tab is ready', async () => {
    setBindingMock('BrowserCompanionDo', vi.fn(async () => ({
      kind: 'state', threadId: THREAD, activePageId: 'n', visible: true, pages: [tab('n')], pageId: 'n',
    })));
    const opened = openBrowserTab(THREAD, { focusAddress: true });
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'a' }));
    await opened;
    expect(addressFocusIntent(THREAD)).toBeNull();
    expect(takeAddressFocus(THREAD, { id: 'n' })).toBe(false);
  });

  // A second new tab replaces the first's request: the first answer, ready
  // or refused, arriving after it, neither resolves nor withdraws it.
  for (const firstFails of [false, true]) {
    it(`leaves the newer tab's focus to its own answer when the older ${firstFails ? 'fails' : 'answers'} late`, async () => {
      const answers: Array<(pageId: string) => void> = [];
      setBindingMock('BrowserCompanionDo', vi.fn(() => new Promise((resolve, reject) => {
        answers.push((pageId) => {
          if (!pageId) reject(new Error('refused'));
          else resolve({ kind: 'state', threadId: THREAD, activePageId: pageId, visible: true, pages: [tab(pageId)], pageId });
        });
      })));
      const first = openBrowserTab(THREAD, { focusAddress: true });
      await vi.waitFor(() => expect(answers).toHaveLength(1));
      // The user presses New again.
      window.dispatchEvent(new Event('pointerdown'));
      const second = openBrowserTab(THREAD, { focusAddress: true });
      await vi.waitFor(() => expect(answers).toHaveLength(2));
      answers[0](firstFails ? '' : 'a');
      await first;
      expect(addressFocusIntent(THREAD)).toBe('');
      answers[1]('b');
      await second;
      expect(addressFocusIntent(THREAD)).toBe('b');
      expect(takeAddressFocus(THREAD, { id: 'b' })).toBe(true);
    });
  }

  it('drops the focus when another tab is shown first', async () => {
    setBindingMock('BrowserCompanionDo', vi.fn(async () => ({
      kind: 'state', threadId: THREAD, activePageId: 'n', visible: true, pages: [tab('n')], pageId: 'n',
    })));
    await openBrowserTab(THREAD, { focusAddress: true });
    expect(takeAddressFocus(THREAD, { id: 'other' })).toBe(false);
    expect(addressFocusIntent(THREAD)).toBeNull();
    expect(takeAddressFocus(THREAD, { id: 'n' })).toBe(false);
  });

  it('asks no focus for a tab opened without it, and none for a failed open', async () => {
    setBindingMock('BrowserCompanionDo', vi.fn(async () => ({
      kind: 'state', threadId: THREAD, activePageId: 'n', visible: true, pages: [tab('n')], pageId: 'n',
    })));
    await openBrowserTab(THREAD, { address: 'https://example.com' });
    expect(addressFocusIntent(THREAD)).toBeNull();
    // A failed creation answers no tab: the tab itself shows its error.
    setBindingMock('BrowserCompanionDo', vi.fn(async () => ({
      kind: 'state', threadId: THREAD, activePageId: 'n', visible: true, pages: [tab('n', { error: 'refused' })], pageId: '',
    })));
    await openBrowserTab(THREAD, { focusAddress: true });
    expect(addressFocusIntent(THREAD)).toBeNull();
  });
});

describe('numbering the user\'s tab selections', () => {
  const THREAD = 'thread-browser';
  const shown = { kind: 'state', threadId: THREAD, activePageId: 'a', visible: true, pages: [page('https://example.test/', 'A')] };
  type Action = { kind: string; mount?: string; select?: number };
  let sent: Action[];
  let answers: Array<() => void>;
  let mounts: string[];

  beforeEach(() => {
    resetBrowserCompanionForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
    installCompanionPanes();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
    createPane('main').replaceThread(makeThread({ id: THREAD }));
    applyBrowserCompanionState(shown);
    sent = [];
    answers = [];
    mounts = ['mount-1', 'mount-2'];
    // No action answers until the test lets it: every one is on its way.
    setBindingMock('BrowserCompanionDo', vi.fn(async (_thread: string, action: Action) => {
      sent.push({ ...action });
      await new Promise<void>((resolve) => answers.push(resolve));
      return shown;
    }));
    setBindingMock('BrowserCompanionPaneAttach', vi.fn(async () => ({ id: mounts.shift(), state: shown })));
    setBindingMock('BrowserCompanionPaneDetach', vi.fn(async () => {}));
  });
  afterEach(() => {
    for (const answer of answers.splice(0)) answer();
    resetBrowserCompanionForTest();
  });

  const order = (action: Action) => ({ kind: action.kind, mount: action.mount ?? '', select: action.select ?? 0 });

  it('numbers selections consecutively per mount when the user acts, from 1 again after a reconnect', async () => {
    let pane = attachBrowserCompanion(THREAD);
    await vi.waitFor(() => expect(pane.current).not.toBeNull());
    void browserCompanionAct(THREAD, { kind: 'activate', pageId: 'a' });
    void browserCompanionAct(THREAD, { kind: 'step', index: 1 });
    void browserCompanionAct(THREAD, { kind: 'close', pageId: 'a' });
    void openBrowserTab(THREAD);
    void browserCompanionAct(THREAD, { kind: 'reopen' });
    void browserCompanionAct(THREAD, { kind: 'duplicate', pageId: 'a' });
    void browserCompanionAct(THREAD, { kind: 'download-open', downloadId: 'd' });
    void browserCompanionAct(THREAD, { kind: 'restore', pageId: 'a' });
    expect(sent.map(order)).toEqual([
      { kind: 'activate', mount: 'mount-1', select: 1 },
      { kind: 'step', mount: 'mount-1', select: 2 },
      { kind: 'close', mount: '', select: 0 },
      { kind: 'new', mount: 'mount-1', select: 3 },
      { kind: 'reopen', mount: 'mount-1', select: 4 },
      { kind: 'duplicate', mount: 'mount-1', select: 5 },
      { kind: 'download-open', mount: 'mount-1', select: 6 },
      { kind: 'restore', mount: '', select: 0 },
    ]);

    // The connection drops and the pane attaches again as a new mount, which
    // the backend counts from 1.
    pane.release();
    pane = attachBrowserCompanion(THREAD);
    await vi.waitFor(() => expect(pane.current).not.toBeNull());
    sent = [];
    void browserCompanionAct(THREAD, { kind: 'activate', pageId: 'a' });
    void browserCompanionAct(THREAD, { kind: 'step', index: -1 });
    expect(sent.map(order)).toEqual([
      { kind: 'activate', mount: 'mount-2', select: 1 },
      { kind: 'step', mount: 'mount-2', select: 2 },
    ]);
    pane.release();
  });

  it('sends a selection made with no pane mounted unnumbered', () => {
    void browserCompanionAct(THREAD, { kind: 'activate', pageId: 'a' });
    expect(sent.map(order)).toEqual([{ kind: 'activate', mount: '', select: 0 }]);
  });
});
