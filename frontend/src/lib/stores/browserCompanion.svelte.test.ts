import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  addressFocusIntent,
  applyBrowserCompanionState,
  attachBrowserCompanion,
  browserCompanionAct,
  focusBrowserPage,
  noteBrowserActionError,
  openBrowserTab,
  takeAddressFocus,
  canUseNativeBrowser,
  hydrateBrowserCompanionState,
  closeFocusedBrowserTab,
  reconcileBrowserCompanionForPane,
  resetBrowserCompanionForTest,
  setBrowserPaneMountForTest,
} from './browserCompanion.svelte';
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

describe('handing the keyboard to the browser page', () => {
  const THREAD = 'thread-browser';
  const shown = { kind: 'state', threadId: THREAD, activePageId: 'a', visible: true, pages: [page('https://example.test/', 'A')] };
  type Action = { kind: string; pageId?: string; mount?: string; intent?: number };

  let area: HTMLElement;
  let composer: HTMLTextAreaElement;
  let address: HTMLInputElement;
  let posted: string[];
  let pane: ReturnType<typeof attachBrowserCompanion>;
  // Each focus-page call waits until the test lets it answer: the request
  // is still on its way.
  let pending: Array<() => void>;
  let actions: Action[];

  function deferFocusAnswers(): void {
    setBindingMock('BrowserCompanionDo', vi.fn(async (_thread: string, action: Action) => {
      actions.push({ ...action });
      if (action.kind === 'focus-page') await new Promise<void>((resolve) => pending.push(resolve));
      return shown;
    }));
  }

  function answerFocus(): void {
    for (const resolve of pending.splice(0)) resolve();
  }

  const withdrawals = () => actions.filter((action) => action.kind === 'focus-page-withdraw');

  beforeEach(async () => {
    resetBrowserCompanionForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
    installCompanionPanes();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
    createPane('main').replaceThread(makeThread({ id: THREAD }));
    applyBrowserCompanionState(shown);
    posted = [];
    pending = [];
    actions = [];
    vi.stubGlobal('__aoPaneFocusBridge', 1);
    vi.stubGlobal('chrome', { webview: { postMessage: (message: string) => posted.push(message) } });
    area = document.createElement('div');
    area.tabIndex = -1;
    composer = document.createElement('textarea');
    address = document.createElement('input');
    document.body.append(area, composer, address);
    setBindingMock('BrowserCompanionPaneAttach', vi.fn(async () => ({ id: 'mount-1', state: shown })));
    setBindingMock('BrowserCompanionPaneDetach', vi.fn(async () => {}));
    pane = attachBrowserCompanion(THREAD);
    await vi.waitFor(() => expect(pane.current).not.toBeNull());
  });
  afterEach(() => {
    answerFocus();
    pane.release();
    area.remove();
    composer.remove();
    address.remove();
    vi.unstubAllGlobals();
    resetBrowserCompanionForTest();
  });

  it('asks for the page by its mount and a numbered intent, telling the launcher first', async () => {
    deferFocusAnswers();
    const first = focusBrowserPage(THREAD, 'a', area);
    expect(posted).toEqual(['ao:pane-focus:{"mount":"mount-1","intent":1,"withdrawn":false}']);
    expect(actions).toEqual([expect.objectContaining({ kind: 'focus-page', pageId: 'a', mount: 'mount-1', intent: 1 })]);
    answerFocus();
    expect(await first).toBeNull();
    // A second request is the next intent; the first was superseded.
    const second = focusBrowserPage(THREAD, 'a', area);
    expect(actions.at(-1)).toEqual(expect.objectContaining({ kind: 'focus-page', intent: 2 }));
    answerFocus();
    expect(await second).toBeNull();
    expect(withdrawals()).toEqual([]);
  });

  it.each([
    ['the composer focused', () => composer.focus()],
    ['the address bar refocused', () => address.focus()],
    ['a press outside the page area', () => composer.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }))],
    ['a tab switch', () => applyBrowserCompanionState({ ...shown, activePageId: 'b', seq: 99 })],
    ['the pane unmounting', () => pane.release()],
  ])('withdraws a request still on its way when %s', async (_name, choose) => {
    deferFocusAnswers();
    const request = focusBrowserPage(THREAD, 'a', area);
    choose();
    expect(posted.at(-1)).toBe('ao:pane-focus:{"mount":"mount-1","intent":1,"withdrawn":true}');
    await vi.waitFor(() => expect(withdrawals()).toEqual([
      expect.objectContaining({ kind: 'focus-page-withdraw', mount: 'mount-1', intent: 1 }),
    ]));
    answerFocus();
    expect(await request).toBeNull();
    // Withdrawn once: a later choice sends nothing more.
    composer.focus();
    expect(withdrawals()).toHaveLength(1);
  });

  it('keeps the request through a focus or press inside the page area', async () => {
    deferFocusAnswers();
    const request = focusBrowserPage(THREAD, 'a', area);
    area.focus();
    area.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }));
    applyBrowserCompanionState({ ...shown, seq: 99 });
    answerFocus();
    await request;
    expect(withdrawals()).toEqual([]);
    expect(posted).toHaveLength(1);
  });

  it('stops watching once the page took the keyboard', async () => {
    deferFocusAnswers();
    const request = focusBrowserPage(THREAD, 'a', area);
    applyBrowserCompanionState({ kind: 'focus', threadId: THREAD, mount: 'mount-1', focusIntent: 1 });
    answerFocus();
    await request;
    composer.focus();
    expect(withdrawals()).toEqual([]);
  });

  it.each([
    ['before the answer', true],
    ['after the answer', false],
  ])('puts a refusal the launcher reports %s in the banner', async (_name, reportFirst) => {
    deferFocusAnswers();
    const request = focusBrowserPage(THREAD, 'a', area);
    const refusal = { kind: 'focus', threadId: THREAD, mount: 'mount-1', focusIntent: 1, error: 'browser: the page did not take keyboard focus: E_FAIL' };
    if (reportFirst) applyBrowserCompanionState(refusal);
    answerFocus();
    expect(await request).toBeNull();
    if (!reportFirst) applyBrowserCompanionState(refusal);
    expect(pane.current?.actionError).toContain('E_FAIL');
    // The page's state, admitted after, leaves the banner up.
    applyBrowserCompanionState({ ...shown, seq: 100 });
    expect(pane.current?.actionError).toContain('E_FAIL');
    expect(pane.current?.error).toBe('');
  });

  it('never lets an obsolete refusal overwrite a newer banner', async () => {
    deferFocusAnswers();
    const refusal = (intent: number) => ({ kind: 'focus', threadId: THREAD, mount: 'mount-1', focusIntent: intent, error: `refused ${intent}` });

    // A withdrawn intent's refusal.
    const first = focusBrowserPage(THREAD, 'a', area);
    composer.focus();
    applyBrowserCompanionState(refusal(1));
    expect(pane.current?.actionError).toBe('');

    // A superseded intent's refusal.
    const second = focusBrowserPage(THREAD, 'a', area);
    const third = focusBrowserPage(THREAD, 'a', area);
    applyBrowserCompanionState(refusal(2));
    expect(pane.current?.actionError).toBe('');

    // A navigation started after the request failed: its banner stays.
    setBindingMock('BrowserCompanionDo', vi.fn(async () => {
      throw new Error('navigation refused');
    }));
    await browserCompanionAct(THREAD, { kind: 'navigate', pageId: 'a', address: 'https://example.test/x' });
    applyBrowserCompanionState(refusal(3));
    expect(pane.current?.actionError).toBe('navigation refused');
    answerFocus();
    await Promise.all([first, second, third]);

    // One that succeeded leaves no banner, so the refusal shows.
    deferFocusAnswers();
    const fourth = focusBrowserPage(THREAD, 'a', area);
    setBindingMock('BrowserCompanionDo', vi.fn(async () => shown));
    await browserCompanionAct(THREAD, { kind: 'navigate', pageId: 'a', address: 'https://example.test/y' });
    applyBrowserCompanionState(refusal(4));
    expect(pane.current?.actionError).toBe('refused 4');
    answerFocus();
    await fourth;
  });

  it('puts a refused request in the banner, and leaves a banner already up when it succeeds', async () => {
    setBindingMock('BrowserCompanionDo', vi.fn(async () => {
      throw new Error('browser: pane mount not found');
    }));
    const failure = await focusBrowserPage(THREAD, 'a', area);
    expect(failure?.error).toContain('pane mount not found');
    expect(pane.current?.actionError).toContain('pane mount not found');

    // The navigation the focus follows failed: its banner stays.
    noteBrowserActionError(THREAD, 'navigation refused');
    setBindingMock('BrowserCompanionDo', vi.fn(async () => shown));
    expect(await focusBrowserPage(THREAD, 'a', area)).toBeNull();
    expect(pane.current?.actionError).toBe('navigation refused');
  });

  it('asks nothing outside the host window, where there is no native view', async () => {
    setHostWindowFromBootstrap(false);
    try {
      const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => shown));
      expect(await focusBrowserPage(THREAD, 'a', area)).toBeNull();
      expect(act).not.toHaveBeenCalled();
      expect(posted).toEqual([]);
    } finally {
      setHostWindowFromBootstrap(true);
    }
  });
});

