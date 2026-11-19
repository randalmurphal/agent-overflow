import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { browserCompanionAct, closeFocusedBrowserTab, openBrowserTab, toggleBrowserFromHeader } from './browserActions';
import {
  applyBrowserCompanionState,
  attachBrowserCompanion,
  resetBrowserCompanionForTest,
  setBrowserPaneMountForTest,
} from './browserCompanion.svelte';
import { addressFocusIntent, takeAddressFocus } from './browserAddressFocus.svelte';
import { companionForSource, installCompanionPanes, resetCompanionPanesForTest } from './companionPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { createPane, focusPane, resetPanesForTest } from './panes.svelte';
import { makeThread } from '../../test/helpers/chat';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { getToasts, resetToastsForTest } from './toast.svelte';

const page = (url: string, title: string) => ({ id: 'page-1', url, title, canGoBack: false, canGoForward: false });

describe('the chat header browser button', () => {
  beforeEach(() => {
    resetBrowserCompanionForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
    installCompanionPanes();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
    createPane('main').replaceThread(makeThread({ id: 'thread-browser' }));
  });

  // The state channel has no replay: tabs opened while this window was
  // away are known only from a read on the new connection.
  const tab = { id: 'page-1', url: 'https://example.com/', title: 'Example', canGoBack: false, canGoForward: false };
  const none = (seq: number) => ({ kind: 'state', threadId: 'thread-browser', pages: [], seq });
  const hidden = (seq: number) => ({ kind: 'state', threadId: 'thread-browser', activePageId: 'page-1', visible: false, pages: [tab], seq });

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

describe('opening a browser tab', () => {
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

  // The numbers belong to the mount: once it ends they are gone, even for an
  // attach answering the same id.
  it('forgets a mount\'s numbers when the mount ends', async () => {
    mounts = ['mount-1', 'mount-1'];
    let pane = attachBrowserCompanion(THREAD);
    await vi.waitFor(() => expect(pane.current).not.toBeNull());
    void browserCompanionAct(THREAD, { kind: 'activate', pageId: 'a' });
    pane.release();
    pane = attachBrowserCompanion(THREAD);
    await vi.waitFor(() => expect(pane.current).not.toBeNull());
    void browserCompanionAct(THREAD, { kind: 'activate', pageId: 'a' });
    expect(sent.map(order)).toEqual([
      { kind: 'activate', mount: 'mount-1', select: 1 },
      { kind: 'activate', mount: 'mount-1', select: 1 },
    ]);
    pane.release();
  });

  it('sends a selection made with no pane mounted unnumbered', () => {
    void browserCompanionAct(THREAD, { kind: 'activate', pageId: 'a' });
    expect(sent.map(order)).toEqual([{ kind: 'activate', mount: '', select: 0 }]);
  });
});
