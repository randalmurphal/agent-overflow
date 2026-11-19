import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  browserAction,
  browserCompanionAct,
  browserHeaderIntent,
  closeFocusedBrowserTab,
  openBrowserTab,
  runBrowserHeaderIntent,
  toDownloadId,
  toPageId,
} from './browserActions';
import {
  applyBrowserCompanionState,
  attachBrowserCompanion,
  browserActionError,
  browserThreadState,
  resetBrowserCompanionForTest,
} from './browserCompanion.svelte';
import { addressFocusIntent, takeAddressFocus } from './browserAddressFocus.svelte';
import { closeCompanion, companionForSource, installCompanionPanes, resetCompanionPanesForTest } from './companionPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { createPane, focusPane, resetPanesForTest } from './panes.svelte';
import { makeThread } from '../../test/helpers/chat';
import { attachBrowserPaneForTest } from '../../test/helpers/browserPane';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { getToasts, resetToastsForTest } from './toast.svelte';
import { canPresentNativeBrowser, canUseNativeBrowser } from './browserCompanion.svelte';
import { forgetBackendEntities, forgetThread, noteThread } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';

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
    const toggled = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    await Promise.resolve();
    expect(act).not.toHaveBeenCalled();
    answer(hidden(2));
    await toggled;
    expect(act).toHaveBeenCalledTimes(1);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'show', pageId: 'page-1' });
  });

  // The press meant "open": a read that finds visible tabs shows the pane,
  // and the press must not then hide it.
  it('the header button pressed to open leaves shown the tabs its read finds visible', async () => {
    let answer: (state: unknown) => void = () => {};
    setBindingMock('BrowserCompanionThreadState', () => new Promise((resolve) => { answer = resolve; }));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => none(9)));
    const toggled = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    await Promise.resolve();
    answer({ ...hidden(2), visible: true });
    await toggled;
    expect(companionForSource('main', 'browser')).not.toBeNull();
    expect(act).not.toHaveBeenCalled();
  });

  // Both presses saw the button closed; each would send `new` once the
  // read found no tab, before either answer arrived.
  it('the header button pressed twice during one open makes one tab', async () => {
    let answer: (state: unknown) => void = () => {};
    setBindingMock('BrowserCompanionThreadState', () => new Promise((resolve) => { answer = resolve; }));
    let reply: (state: unknown) => void = () => {};
    const act = setBindingMock('BrowserCompanionDo', vi.fn(() => new Promise((resolve) => { reply = resolve; })));
    const first = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    const second = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    answer(none(1));
    await vi.waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    expect(runBrowserHeaderIntent(browserHeaderIntent('thread-browser'))).toBe(first);
    reply({ ...hidden(2), visible: true });
    await Promise.all([first, second]);
    expect(act).toHaveBeenCalledTimes(1);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'new' });
    expect(companionForSource('main', 'browser')).not.toBeNull();
  });

  it('the header button pressed while its pane is shown hides it without reading', async () => {
    applyBrowserCompanionState({ ...hidden(1), visible: true } as never);
    expect(companionForSource('main', 'browser')).not.toBeNull();
    const read = setBindingMock('BrowserCompanionThreadState', vi.fn(async () => hidden(2)));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => hidden(3)));
    await runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    expect(read).not.toHaveBeenCalled();
    expect(act).toHaveBeenCalledTimes(1);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'hide' });
    expect(companionForSource('main', 'browser')).toBeNull();
  });

  it('the header button opens a tab once the state is known to have none', async () => {
    setBindingMock('BrowserCompanionThreadState', async () => none(1));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => hidden(2)));
    await runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    expect(act).toHaveBeenCalledTimes(1);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'new' });
  });

  const second = { ...tab, id: 'page-2', url: 'https://example.com/b', title: 'B' };
  const twoTabs = (seq: number, visible: boolean) => ({
    kind: 'state', threadId: 'thread-browser', activePageId: 'page-2', visible, pages: [tab, second], seq,
  });

  it('two presses during a cold read that finds visible tabs show them and send nothing', async () => {
    let answer: (state: unknown) => void = () => {};
    const read = setBindingMock('BrowserCompanionThreadState', vi.fn(() => new Promise((resolve) => { answer = resolve; })));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => none(9)));
    const first = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    const again = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    answer(twoTabs(2, true));
    await Promise.all([first, again]);
    expect(read).toHaveBeenCalledTimes(1);
    expect(act).not.toHaveBeenCalled();
    expect(companionForSource('main', 'browser')).not.toBeNull();
    expect(browserThreadState('thread-browser')?.activePageId).toBe('page-2');
  });

  it('two presses during a cold read that finds hidden tabs send one show of the active tab', async () => {
    let answer: (state: unknown) => void = () => {};
    setBindingMock('BrowserCompanionThreadState', () => new Promise((resolve) => { answer = resolve; }));
    const replies: Array<(state: unknown) => void> = [];
    const act = setBindingMock('BrowserCompanionDo', vi.fn(() => new Promise((resolve) => { replies.push(resolve); })));
    const first = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    const again = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    answer(twoTabs(2, false));
    await vi.waitFor(() => expect(act).toHaveBeenCalled());
    // Both presses have read the state by now; the show is still on its way.
    await new Promise((resolve) => setTimeout(resolve, 20));
    for (const reply of replies) reply(twoTabs(3, true));
    await Promise.all([first, again]);
    expect(act.mock.calls.map((call) => call[1])).toEqual([expect.objectContaining({ kind: 'show', pageId: 'page-2' })]);
    expect(companionForSource('main', 'browser')).not.toBeNull();
    expect(browserThreadState('thread-browser')?.activePageId).toBe('page-2');
  });

  it('a tab an agent opens during the read is shown, with no blank tab beside it', async () => {
    let answer: (state: unknown) => void = () => {};
    setBindingMock('BrowserCompanionThreadState', () => new Promise((resolve) => { answer = resolve; }));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => none(9)));
    const pressed = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    applyBrowserCompanionState({ ...hidden(3), visible: true } as never);
    answer(none(2));
    await pressed;
    expect(act).not.toHaveBeenCalled();
    expect(companionForSource('main', 'browser')).not.toBeNull();
    expect(browserThreadState('thread-browser')?.activePageId).toBe('page-1');
  });

  // The stand-in for a Manager show-or-reserve operation: a tab the agent
  // opens while the header's `new` is on its way still gets the blank tab
  // beside it. The answer, newer than the push, decides the selected tab.
  it('a tab an agent opens while the header asks for a new one leaves one new tab, selected', async () => {
    setBindingMock('BrowserCompanionThreadState', async () => none(1));
    let reply: (state: unknown) => void = () => {};
    const act = setBindingMock('BrowserCompanionDo', vi.fn(() => new Promise((resolve) => { reply = resolve; })));
    const pressed = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    await vi.waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    applyBrowserCompanionState({ ...hidden(2), visible: true } as never);
    reply({ ...twoTabs(3, true), pageId: 'page-2' });
    await pressed;
    expect(act.mock.calls.map((call) => (call[1] as { kind: string }).kind)).toEqual(['new']);
    expect(companionForSource('main', 'browser')).not.toBeNull();
    expect(browserThreadState('thread-browser')?.activePageId).toBe('page-2');
  });

  // No backend change follows a show of a session already shown, so no push
  // would bring the pane; the layout presents it.
  it('a press for a visible session whose pane this layout lacks presents it without asking', async () => {
    applyBrowserCompanionState({ ...hidden(1), visible: true } as never);
    closeCompanion('browser-main');
    const intent = browserHeaderIntent('thread-browser');
    expect(intent.kind).toBe('show');
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ ...hidden(1), visible: true })));
    await runBrowserHeaderIntent(intent);
    expect(act).not.toHaveBeenCalled();
    expect(companionForSource('main', 'browser')).not.toBeNull();
  });

  it('the header button opens nothing when the state cannot be read, and says so', async () => {
    resetToastsForTest();
    setBindingMock('BrowserCompanionThreadState', async () => { throw new Error('backend restarting'); });
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => hidden(2)));
    await runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
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
    await attachBrowserPaneForTest('thread-browser', 'mount-1');
    focusPane('browser-main');

    expect(closeFocusedBrowserTab()).toBe(true);
    // The backend picks the tab: the close names none.
    expect(calls).toEqual([['thread-browser', 'close-active', '']]);
    // Still open: the pane follows the backend's answer, not the keystroke.
    expect(companionForSource('main', 'browser')).not.toBeNull();
    await vi.waitFor(() => expect(companionForSource('main', 'browser')).toBeNull());
  });

  // The backend moved to another tab while this client was away: the chord
  // still names no tab, and goes in the reattached mount's order, so the
  // backend closes the tab active there.
  it('closes through the reattached mount after the backend changed tabs', async () => {
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
    const closes: Array<{ kind: string; pageId: string; mount?: string; select?: number }> = [];
    setBindingMock('BrowserCompanionDo', async (threadId: string, action: { kind: string; pageId: string; mount?: string; select?: number }) => {
      closes.push({ kind: action.kind, pageId: action.pageId, mount: action.mount, select: action.select });
      return { kind: 'state', threadId, activePageId: 'page-1', visible: true, seq: 3, pages: [tab('page-1')] };
    });
    const attachment = attachBrowserCompanion('thread-browser');
    await vi.waitFor(() => expect(browserThreadState('thread-browser')?.activePageId).toBe('page-2'));
    focusPane('browser-main');
    expect(closeFocusedBrowserTab()).toBe(true);
    await vi.waitFor(() => expect(closes).toEqual([{ kind: 'close-active', pageId: '', mount: 'mount-1', select: 1 }]));
    attachment.release();
  });

  // The keyboard's close takes its place in the mount's order at the
  // keypress and is sent at once; the backend closes the tab active at its
  // turn, so nothing here waits for an earlier answer.
  describe('right after a tab selection or another close', () => {
    const tab = (id: string) => ({ id, url: `https://example.com/${id}`, title: id, canGoBack: false, canGoForward: false });
    let sent: Array<{ kind: string; pageId: string; mount?: string; select?: number }>;
    let answers: Array<() => void>;

    beforeEach(async () => {
      applyBrowserCompanionState({
        kind: 'state', threadId: 'thread-browser', activePageId: 'a', visible: true, seq: 1, pages: ['a', 'b', 'c'].map(tab),
      });
      await attachBrowserPaneForTest('thread-browser', 'mount-1');
      sent = [];
      answers = [];
      // The backend answers only when the test lets it.
      setBindingMock('BrowserCompanionDo', vi.fn(async (threadId: string, action: { kind: string; pageId: string; mount?: string; select?: number }) => {
        sent.push({ kind: action.kind, pageId: action.pageId, mount: action.mount, select: action.select });
        await new Promise<void>((resolve) => answers.push(resolve));
        return { kind: 'state', threadId, activePageId: 'a', visible: true, seq: 1, pages: ['a', 'b', 'c'].map(tab) };
      }));
      focusPane('browser-main');
    });
    afterEach(() => {
      for (const answer of answers.splice(0)) answer();
    });

    it('sends the close at once, numbered after the selection the user made first', () => {
      void browserCompanionAct('thread-browser', browserAction.select(toPageId('b')));
      expect(closeFocusedBrowserTab()).toBe(true);
      void browserCompanionAct('thread-browser', browserAction.select(toPageId('c')));
      expect(sent).toEqual([
        { kind: 'activate', pageId: 'b', mount: 'mount-1', select: 1 },
        { kind: 'close-active', pageId: '', mount: 'mount-1', select: 2 },
        { kind: 'activate', pageId: 'c', mount: 'mount-1', select: 3 },
      ]);
    });

    it('sends one numbered close per press when presses come faster than the answers', () => {
      expect(closeFocusedBrowserTab()).toBe(true);
      expect(closeFocusedBrowserTab()).toBe(true);
      expect(sent).toEqual([
        { kind: 'close-active', pageId: '', mount: 'mount-1', select: 1 },
        { kind: 'close-active', pageId: '', mount: 'mount-1', select: 2 },
      ]);
    });

    // The backend refuses a keyboard command while an earlier number of its
    // epoch never arrived; the window starts a new epoch only once every
    // action it sent in the old one answered or failed, so nothing of the
    // old epoch is still meant to act.
    it('starts a new epoch only when none of the mount\'s actions is unanswered', async () => {
      const epochs: Array<{ epoch?: number; select?: number }> = [];
      const failNext: boolean[] = [];
      setBindingMock('BrowserCompanionDo', vi.fn(async (threadId: string, action: { kind: string; epoch?: number; select?: number }) => {
        epochs.push({ epoch: action.epoch, select: action.select });
        await new Promise<void>((resolve) => answers.push(resolve));
        if (failNext.shift()) throw new Error('browser: an earlier tab action from this pane had not arrived');
        return { kind: 'state', threadId, activePageId: 'a', visible: true, seq: 1, pages: ['a', 'b', 'c'].map(tab) };
      }));
      failNext.push(false, true);
      const first = browserCompanionAct('thread-browser', browserAction.select(toPageId('b')));
      expect(closeFocusedBrowserTab()).toBe(true);
      answers.shift()?.();
      await first;
      // The close is still unanswered: the next press stays in epoch 1.
      expect(closeFocusedBrowserTab()).toBe(true);
      expect(epochs).toEqual([{ epoch: 1, select: 1 }, { epoch: 1, select: 2 }, { epoch: 1, select: 3 }]);
      // The close fails and the last press answers: the next starts epoch 2.
      for (const answer of answers.splice(0)) answer();
      await vi.waitFor(() => expect(browserActionError('thread-browser')).not.toBe(''));
      await Promise.resolve();
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(closeFocusedBrowserTab()).toBe(true);
      expect(epochs.at(-1)).toEqual({ epoch: 2, select: 1 });
    });

    it('leaves a close from the tab menu on the tab it names, unnumbered', () => {
      void browserCompanionAct('thread-browser', browserAction.select(toPageId('b')));
      void browserCompanionAct('thread-browser', browserAction.onTab('close', toPageId('c')));
      expect(sent).toEqual([
        { kind: 'activate', pageId: 'b', mount: 'mount-1', select: 1 },
        { kind: 'close', pageId: 'c', mount: undefined, select: undefined },
      ]);
    });
  });

  // A pane not attached yet has no mount, and its close is not one the
  // backend can order: it is taken and nothing is sent.
  it('sends nothing while the pane has no mount', () => {
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ kind: 'state', threadId: 'thread-browser', pages: [] })));
    focusPane('browser-main');
    expect(closeFocusedBrowserTab()).toBe(true);
    expect(act).not.toHaveBeenCalled();
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
    expect(await browserCompanionAct('thread-browser', browserAction.onTab('close', toPageId('page-1'))))
      .toEqual({ error: 'page is gone', cause: failure });
  });
});

// Two computers claiming one ownership epoch leave the owner unknown, even
// when the home computer's claim arrived first: the runtime refuses the
// thread's companion pushes then, and no action goes to the home browser.
describe('a thread whose ownership two computers claim', () => {
  const THREAD = 'thread-contested';
  const contest = (): void => {
    noteThread(THREAD, 'remote', 4);
  };

  beforeEach(() => {
    resetBrowserCompanionForTest();
    resetToastsForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
    installCompanionPanes();
    stageBackend({ id: 'remote', backendId: 'remote' });
    noteThread(THREAD, HOME_BACKEND, 4);
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
    createPane('main').replaceThread(makeThread({ id: THREAD }));
  });
  afterEach(() => {
    forgetThread(THREAD);
    forgetBackendEntities('remote');
    resetStagedBackends();
    resetBrowserCompanionForTest();
    resetToastsForTest();
  });

  it('offers no browser and sends no action, even with the home claim first', async () => {
    expect(canUseNativeBrowser(THREAD)).toBe(true);
    contest();
    expect(canUseNativeBrowser(THREAD)).toBe(false);
    expect(canPresentNativeBrowser(THREAD)).toBe(false);
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ kind: 'state', threadId: THREAD, pages: [] })));
    const failure = await browserCompanionAct(THREAD, browserAction.onTab('reload', toPageId('page-1')));
    expect(failure?.error).toContain('Two computers claim this conversation');
    expect(await openBrowserTab(THREAD, { address: 'https://example.com/' })).not.toBeNull();
    expect(act).not.toHaveBeenCalled();
  });

  it('sends nothing for a header press whose read answers after the claim was contested', async () => {
    let answer: (state: unknown) => void = () => {};
    setBindingMock('BrowserCompanionThreadState', () => new Promise((resolve) => { answer = resolve; }));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ kind: 'state', threadId: THREAD, pages: [] })));
    const toggled = runBrowserHeaderIntent(browserHeaderIntent(THREAD));
    await Promise.resolve();
    contest();
    answer({ kind: 'state', threadId: THREAD, pages: [], seq: 1 });
    await toggled;
    expect(act).not.toHaveBeenCalled();
    expect(getToasts().map((toast) => toast.message).join('\n')).toContain('Two computers claim this conversation');
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
    void browserCompanionAct(THREAD, browserAction.select(toPageId('a')));
    void browserCompanionAct(THREAD, browserAction.step(1));
    void browserCompanionAct(THREAD, browserAction.onTab('close', toPageId('a')));
    void openBrowserTab(THREAD);
    void browserCompanionAct(THREAD, browserAction.reopen());
    void browserCompanionAct(THREAD, browserAction.duplicate(toPageId('a')));
    void browserCompanionAct(THREAD, browserAction.download('download-open', toDownloadId('d')));
    void browserCompanionAct(THREAD, browserAction.onTab('restore', toPageId('a')));
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
    void browserCompanionAct(THREAD, browserAction.select(toPageId('a')));
    void browserCompanionAct(THREAD, browserAction.step(-1));
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
    void browserCompanionAct(THREAD, browserAction.select(toPageId('a')));
    pane.release();
    pane = attachBrowserCompanion(THREAD);
    await vi.waitFor(() => expect(pane.current).not.toBeNull());
    void browserCompanionAct(THREAD, browserAction.select(toPageId('a')));
    expect(sent.map(order)).toEqual([
      { kind: 'activate', mount: 'mount-1', select: 1 },
      { kind: 'activate', mount: 'mount-1', select: 1 },
    ]);
    pane.release();
  });

  it('sends a selection made with no pane mounted unnumbered', () => {
    void browserCompanionAct(THREAD, browserAction.select(toPageId('a')));
    expect(sent.map(order)).toEqual([{ kind: 'activate', mount: '', select: 0 }]);
  });
});
