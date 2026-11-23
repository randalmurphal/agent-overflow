import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  actInPane,
  browserCompanionAct,
  browserHeaderButton,
  browserHeaderIntent,
  closeFocusedBrowserTab,
  isBrowserPaneUnattached,
  openBrowserTab,
  openBrowserTabInPane,
  runBrowserHeaderIntent,
} from './browserActions';
import { browserAction, downloadIdOf, pageIdOf } from './browserActionWire';
import { frontendErrorCaptureStateForTest, resetFrontendErrorCaptureForTest } from '../utils/frontendErrorCapture';
import {
  applyBrowserCompanionState,
  attachBrowserCompanion,
  browserActionError,
  browserThreadState,
  resetBrowserCompanionForTest,
} from './browserCompanion.svelte';
import { addressFocusIntent, armAddressFocus, settleAddressFocus, takeAddressFocus } from './browserAddressFocus.svelte';
import { closeCompanion, companionForSource, installCompanionPanes, resetCompanionPanesForTest } from './companionPanes.svelte';
import { getPromotedPaneId, promotePane, resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { REVEAL_PANE_EVENT } from './eventNames';
import { createPane, focusPane, getPane, resetPanesForTest } from './panes.svelte';
import { makeThread } from '../../test/helpers/chat';
import { attachBrowserPaneForTest, pageIdForTest, paneForTest } from '../../test/helpers/browserPane';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { getToasts, resetToastsForTest } from './toast.svelte';
import { canPresentNativeBrowser, canUseNativeBrowser } from './browserCompanion.svelte';
import { forgetBackendEntities, forgetThread, noteThread } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { setCompactLayoutForTest } from './layoutMode.svelte';
import { applySettingsSnapshot, resetSettingsForTest } from './settings.svelte';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import { makeSettings } from '../../test/helpers/settings';

// The home backend has a browser engine and the browser is on, so this
// window can present a thread's browser (canPresentNativeBrowser).
function enableBrowser(): void {
  __setTransportHelloForTest({ protocolVersion: 1, capabilities: ['browser'],
    backendId: '', backendName: '', serverTimeMs: 0, clockSkewMs: 0,
    bundleId: '', bundleVersion: '', minShellBuild: 0 });
  applySettingsSnapshot(makeSettings({ browserEnabled: true }));
}

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
    enableBrowser();
  });
  afterEach(() => {
    resetSettingsForTest();
    __setTransportHelloForTest(null);
  });

  const tab = { id: 'page-1', url: 'https://example.com/', title: 'Example', canGoBack: false, canGoForward: false };
  const none = (seq: number) => ({ kind: 'state', threadId: 'thread-browser', pages: [], seq });
  const hidden = (seq: number) => ({ kind: 'state', threadId: 'thread-browser', activePageId: 'page-1', visible: false, pages: [tab], seq });
  const shown = (seq: number) => ({ ...hidden(seq), visible: true });
  const kinds = (act: { mock: { calls: unknown[][] } }) => act.mock.calls.map((call) => (call[1] as { kind: string }).kind);

  // The Manager decides at its turn which tab to show (ShowCompanion), so
  // the press reads nothing first and names no tab.
  it('a press sends one show that names no tab, reading nothing first', async () => {
    const read = setBindingMock('BrowserCompanionThreadState', vi.fn(async () => none(1)));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => shown(2)));
    await runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    expect(read).not.toHaveBeenCalled();
    expect(act).toHaveBeenCalledTimes(1);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'show-browser', pageId: '' });
    expect(companionForSource('main', 'browser')).not.toBeNull();
  });

  // Every press sends; the Manager converges the shows on one tab and names
  // it only in the answer of the show that opened it (ShowOpened); the other
  // answers name none. The presses' focus request is for that tab, in
  // either answer order, through the second press's own pointer press.
  for (const order of [[0, 1], [1, 0]] as const) {
    it(`two presses before the pane appears focus the tab they opened, answered ${order.join(', ')}`, async () => {
      const replies: Array<(state: unknown) => void> = [];
      const act = setBindingMock('BrowserCompanionDo', vi.fn(() => new Promise((resolve) => { replies.push(resolve); })));
      const first = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
      window.dispatchEvent(new Event('pointerdown'));
      const again = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
      await vi.waitFor(() => expect(act).toHaveBeenCalledTimes(2));
      expect(kinds(act)).toEqual(['show-browser', 'show-browser']);
      const answers = [{ ...shown(2), pageId: 'page-1' }, shown(3)];
      replies[order[0]](answers[order[0]]);
      await vi.waitFor(() => expect(addressFocusIntent('thread-browser')).toEqual(order[0] === 0 ? ['page-1'] : []));
      replies[order[1]](answers[order[1]]);
      await Promise.all([first, again]);
      expect(addressFocusIntent('thread-browser')).toEqual(['page-1']);
      expect(takeAddressFocus('thread-browser', { id: 'page-1' })).toBe(true);
    });
  }

  // Both answers name no tab (the thread had one): nothing is focused.
  it('two presses that open no tab focus nothing', async () => {
    const replies: Array<(state: unknown) => void> = [];
    setBindingMock('BrowserCompanionDo', vi.fn(() => new Promise((resolve) => { replies.push(resolve); })));
    const first = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    const again = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    await vi.waitFor(() => expect(replies).toHaveLength(2));
    replies[0](shown(2));
    await first;
    expect(addressFocusIntent('thread-browser')).toEqual([]);
    replies[1](shown(3));
    await again;
    expect(addressFocusIntent('thread-browser')).toBeNull();
  });

  // The first show's tab opened the pane, and its
  // last tab closed before the show answered. The button offers show
  // again, and a press is a new show, not the old one joined.
  it('a press after the shown pane closed sends a show while an older one is unanswered', async () => {
    const replies: Array<(state: unknown) => void> = [];
    const act = setBindingMock('BrowserCompanionDo', vi.fn(() => new Promise((resolve) => { replies.push(resolve); })));
    const first = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    await vi.waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    applyBrowserCompanionState(shown(2) as never);
    expect(companionForSource('main', 'browser')).not.toBeNull();
    applyBrowserCompanionState(none(3) as never);
    expect(companionForSource('main', 'browser')).toBeNull();
    expect(browserHeaderIntent('thread-browser').kind).toBe('show');
    const again = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    await vi.waitFor(() => expect(act).toHaveBeenCalledTimes(2));
    replies[0](none(3));
    replies[1]({ ...shown(4), pageId: 'page-1' });
    await Promise.all([first, again]);
    expect(companionForSource('main', 'browser')).not.toBeNull();
    expect(addressFocusIntent('thread-browser')).toEqual(['page-1']);
  });

  // Two presses refused for one reason: one toast says it.
  it('a repeated press refused for the same reason raises one toast', async () => {
    resetToastsForTest();
    const refusals: Array<(err: Error) => void> = [];
    setBindingMock('BrowserCompanionDo', vi.fn(() => new Promise((_resolve, reject) => { refusals.push(reject); })));
    const first = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    const again = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    await vi.waitFor(() => expect(refusals).toHaveLength(2));
    refusals[0](new Error('browser: engine unavailable'));
    refusals[1](new Error('browser: engine unavailable'));
    await Promise.all([first, again]);
    expect(getToasts().map((toast) => toast.message)).toEqual(['browser: engine unavailable']);
    expect(addressFocusIntent('thread-browser')).toBeNull();
  });

  // A hide between two shows: the second show is the user's newest press,
  // and the pane ends shown.
  it('show, hide, show ends shown while the first show is still unanswered', async () => {
    const replies: Array<(state: unknown) => void> = [];
    const act = setBindingMock('BrowserCompanionDo', vi.fn((_thread: string, action: { kind: string }) => (
      action.kind === 'hide' ? Promise.resolve(hidden(3)) : new Promise((resolve) => { replies.push(resolve); })
    )));
    const first = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    await vi.waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    // The opened tab's push shows the pane before the first answer.
    applyBrowserCompanionState(shown(2) as never);
    expect(browserHeaderIntent('thread-browser').kind).toBe('hide');
    await runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    expect(companionForSource('main', 'browser')).toBeNull();
    const again = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    expect(again).not.toBe(first);
    await vi.waitFor(() => expect(act).toHaveBeenCalledTimes(3));
    expect(kinds(act)).toEqual(['show-browser', 'hide', 'show-browser']);
    replies[0]({ ...shown(2), pageId: 'page-1' });
    replies[1](shown(4));
    await Promise.all([first, again]);
    expect(companionForSource('main', 'browser')).not.toBeNull();
  });

  it('the header button pressed while its pane is shown hides it without reading', async () => {
    applyBrowserCompanionState(shown(1) as never);
    expect(companionForSource('main', 'browser')).not.toBeNull();
    const read = setBindingMock('BrowserCompanionThreadState', vi.fn(async () => hidden(2)));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => hidden(3)));
    await runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    expect(read).not.toHaveBeenCalled();
    expect(act).toHaveBeenCalledTimes(1);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'hide' });
    expect(companionForSource('main', 'browser')).toBeNull();
  });

  // A maximized pane is the user's; only the user's own press or link
  // brings a pane out from under it.
  describe('while a pane is maximized', () => {
    let reveals: string[] = [];
    const onReveal = (event: Event): void => {
      reveals.push((event as CustomEvent<{ paneId: string }>).detail.paneId);
    };
    beforeEach(() => {
      reveals = [];
      window.addEventListener(REVEAL_PANE_EVENT, onReveal);
    });
    afterEach(() => window.removeEventListener(REVEAL_PANE_EVENT, onReveal));

    it('a state from the agent opens the pane without revealing it; with none maximized it reveals', () => {
      promotePane('main');
      applyBrowserCompanionState(shown(1) as never);
      expect(companionForSource('main', 'browser')).not.toBeNull();
      expect(reveals).toEqual([]);
      expect(getPromotedPaneId()).toBe('main');

      applyBrowserCompanionState(none(2) as never);
      expect(companionForSource('main', 'browser')).toBeNull();
      resetPaneLayoutForTest();
      setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
      applyBrowserCompanionState(shown(3) as never);
      expect(reveals).toEqual(['browser-main']);
    });

    it('the header press on a pane under the maximized one shows it rather than closing it', async () => {
      applyBrowserCompanionState(shown(1) as never);
      promotePane('main');
      reveals = [];
      expect(browserHeaderIntent('thread-browser').kind).toBe('show');
      expect(browserHeaderButton('thread-browser')).toMatchObject({ rendered: true, pressed: false });
      const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => shown(2)));
      await runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
      expect(kinds(act)).toEqual(['show-browser']);
      expect(companionForSource('main', 'browser')).not.toBeNull();
      expect(reveals).toEqual(['browser-main']);
    });

    it('a link opened in the thread\'s browser reveals its pane; a refused one does not', async () => {
      applyBrowserCompanionState(shown(1) as never);
      promotePane('main');
      reveals = [];
      setBindingMock('BrowserCompanionDo', vi.fn(async () => { throw new Error('refused'); }));
      expect(await openBrowserTab('thread-browser', { address: 'https://example.com/' })).not.toBeNull();
      expect(reveals).toEqual([]);
      setBindingMock('BrowserCompanionDo', vi.fn(async () => shown(2)));
      expect(await openBrowserTab('thread-browser', { address: 'https://example.com/' })).toBeNull();
      expect(reveals).toEqual(['browser-main']);
    });

    it('a press or link on a pane already on screen reveals nothing more', async () => {
      applyBrowserCompanionState(shown(1) as never);
      reveals = [];
      setBindingMock('BrowserCompanionDo', vi.fn(async () => shown(2)));
      expect(await openBrowserTab('thread-browser', { address: 'https://example.com/' })).toBeNull();
      expect(reveals).toEqual([]);
    });
  });

  // The tab the show opened is named in the answer, and its address bar
  // takes focus; a show of a tab the thread had names none.
  it('focuses the address of the tab the show opened, and of no tab it found', async () => {
    setBindingMock('BrowserCompanionDo', async () => ({ ...shown(2), pageId: 'page-1' }));
    await runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    expect(addressFocusIntent('thread-browser')).toEqual(['page-1']);
    expect(takeAddressFocus('thread-browser', { id: 'page-1' })).toBe(true);
    closeCompanion('browser-main');
    setBindingMock('BrowserCompanionDo', async () => shown(3));
    await runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    expect(addressFocusIntent('thread-browser')).toBeNull();
  });

  const second = { ...tab, id: 'page-2', url: 'https://example.com/b', title: 'B' };

  // A tab an agent opens while the show is on its way is the one the
  // Manager shows; the window sends no second action for a blank tab.
  it('a tab an agent opens while the header show is on its way is shown, with one action sent', async () => {
    let reply: (state: unknown) => void = () => {};
    const act = setBindingMock('BrowserCompanionDo', vi.fn(() => new Promise((resolve) => { reply = resolve; })));
    const pressed = runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    await vi.waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    applyBrowserCompanionState({ kind: 'state', threadId: 'thread-browser', activePageId: 'page-2', visible: false, pages: [second], seq: 2 } as never);
    reply({ kind: 'state', threadId: 'thread-browser', activePageId: 'page-2', visible: true, pages: [second], seq: 3 });
    await pressed;
    expect(kinds(act)).toEqual(['show-browser']);
    expect(companionForSource('main', 'browser')).not.toBeNull();
    expect(browserThreadState('thread-browser')?.pages).toHaveLength(1);
  });

  // The show may open a tab, and compact presents no browser pane: a press
  // made as the layout turned compact sends nothing.
  it('a press under compact sends nothing', async () => {
    applyBrowserCompanionState(hidden(1) as never);
    setCompactLayoutForTest(true);
    try {
      const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => shown(2)));
      await runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
      expect(act).not.toHaveBeenCalled();
      expect(addressFocusIntent('thread-browser')).toBeNull();
    } finally {
      setCompactLayoutForTest(false);
    }
  });

  // A press there would send nothing, so the button is not offered for a
  // thread with tabs: not under compact, and not with the browser turned
  // off before the backend has closed the tabs. Where the pane is in the
  // layout, the button stays to hide it.
  it('offers no button under compact for a thread with tabs', () => {
    applyBrowserCompanionState(hidden(1) as never);
    expect(browserHeaderButton('thread-browser')).toMatchObject({ rendered: true, intent: { kind: 'show' } });
    setCompactLayoutForTest(true);
    try {
      expect(browserHeaderButton('thread-browser')).toEqual({ rendered: false });
    } finally {
      setCompactLayoutForTest(false);
    }
  });

  it('offers no show with the browser turned off while the thread has tabs, and still the hide', () => {
    applyBrowserCompanionState(hidden(1) as never);
    applySettingsSnapshot(makeSettings({ browserEnabled: false }));
    expect(browserHeaderButton('thread-browser')).toEqual({ rendered: false });
    applySettingsSnapshot(makeSettings({ browserEnabled: true }));
    applyBrowserCompanionState(shown(2) as never);
    applySettingsSnapshot(makeSettings({ browserEnabled: false }));
    expect(browserHeaderButton('thread-browser')).toMatchObject({ rendered: true, pressed: true, intent: { kind: 'hide' } });
  });

  // The pane the button was in no longer shows the thread: nothing would
  // present the tab.
  it('a press for a thread no pane shows sends nothing', async () => {
    const intent = browserHeaderIntent('thread-browser');
    getPane('main')?.replaceThread(makeThread({ id: 'thread-other' }));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => shown(2)));
    await runBrowserHeaderIntent(intent);
    expect(act).not.toHaveBeenCalled();
  });

  // A backend from before the show-browser action refuses it by kind.
  it('a backend that lacks the show says so in a toast and focuses nothing', async () => {
    resetToastsForTest();
    setBindingMock('BrowserCompanionDo', async () => {
      throw new Error('browser: unsupported companion action "show-browser"');
    });
    await runBrowserHeaderIntent(browserHeaderIntent('thread-browser'));
    expect(getToasts().map((toast) => toast.message)).toContain('browser: unsupported companion action "show-browser"');
    expect(addressFocusIntent('thread-browser')).toBeNull();
    expect(companionForSource('main', 'browser')).toBeNull();
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
    // The close names the tab the window shows.
    expect(calls).toEqual([['thread-browser', 'close', 'page-1']]);
    // Still open: the pane follows the backend's answer, not the keystroke.
    expect(companionForSource('main', 'browser')).not.toBeNull();
    await vi.waitFor(() => expect(companionForSource('main', 'browser')).toBeNull());
  });

  // The backend moved to another tab while this client was away: the
  // reattach's state shows it, and the chord names that tab, in the
  // reattached mount's order.
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
    await vi.waitFor(() => expect(closes).toEqual([{ kind: 'close', pageId: 'page-2', mount: 'mount-1', select: 1 }]));
    attachment.release();
  });

  // The keyboard's close takes its place in the mount's order at the
  // keypress, names the tab the window shows then and is sent at once; the
  // backend closes that tab at its turn if it is still there, so nothing
  // here waits for an earlier answer.
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
      void actInPane(paneForTest('thread-browser'), browserAction.select(pageIdForTest('b')));
      expect(closeFocusedBrowserTab()).toBe(true);
      void actInPane(paneForTest('thread-browser'), browserAction.select(pageIdForTest('c')));
      expect(sent).toEqual([
        { kind: 'activate', pageId: 'b', mount: 'mount-1', select: 1 },
        { kind: 'close', pageId: 'a', mount: 'mount-1', select: 2 },
        { kind: 'activate', pageId: 'c', mount: 'mount-1', select: 3 },
      ]);
    });

    it('sends one numbered close per press when presses come faster than the answers', () => {
      expect(closeFocusedBrowserTab()).toBe(true);
      expect(closeFocusedBrowserTab()).toBe(true);
      expect(sent).toEqual([
        { kind: 'close', pageId: 'a', mount: 'mount-1', select: 1 },
        { kind: 'close', pageId: 'a', mount: 'mount-1', select: 2 },
      ]);
    });

    // The backend refuses a number that lost its place; the window starts a
    // new epoch only once every action it sent in the old one answered or
    // failed, so nothing of the old epoch is still meant to act.
    it('starts a new epoch only when none of the mount\'s actions is unanswered', async () => {
      const epochs: Array<{ epoch?: number; select?: number }> = [];
      const failNext: boolean[] = [];
      setBindingMock('BrowserCompanionDo', vi.fn(async (threadId: string, action: { kind: string; epoch?: number; select?: number }) => {
        epochs.push({ epoch: action.epoch, select: action.select });
        await new Promise<void>((resolve) => answers.push(resolve));
        if (failNext.shift()) throw new Error('browser: this tab action arrived too late to apply; try it again');
        return { kind: 'state', threadId, activePageId: 'a', visible: true, seq: 1, pages: ['a', 'b', 'c'].map(tab) };
      }));
      failNext.push(false, true);
      const first = actInPane(paneForTest('thread-browser'), browserAction.select(pageIdForTest('b')));
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

    // The x and the tab menu's closes name their tab and take their place
    // in the same order as the keyboard's, so Mod+W on a and then a's x
    // close a once (internal/browser CloseCompanionPage).
    it('numbers a close from the x or the tab menu in the same order, naming its tab', () => {
      void actInPane(paneForTest('thread-browser'), browserAction.select(pageIdForTest('b')));
      expect(closeFocusedBrowserTab()).toBe(true);
      void actInPane(paneForTest('thread-browser'), browserAction.onTab('close', pageIdForTest('a')));
      void actInPane(paneForTest('thread-browser'), browserAction.onTab('close-others', pageIdForTest('c')));
      void actInPane(paneForTest('thread-browser'), browserAction.onTab('close-right', pageIdForTest('c')));
      void actInPane(paneForTest('thread-browser'), browserAction.onTab('reload', pageIdForTest('c')));
      expect(sent).toEqual([
        { kind: 'activate', pageId: 'b', mount: 'mount-1', select: 1 },
        { kind: 'close', pageId: 'a', mount: 'mount-1', select: 2 },
        { kind: 'close', pageId: 'a', mount: 'mount-1', select: 3 },
        { kind: 'close-others', pageId: 'c', mount: 'mount-1', select: 4 },
        { kind: 'close-right', pageId: 'c', mount: 'mount-1', select: 5 },
        { kind: 'reload', pageId: 'c', mount: undefined, select: undefined },
      ]);
    });
  });

  // A pane not attached yet has no mount, and its close is not one the
  // backend can order: pane.close is not offered (the chord is not taken),
  // closes no tab and sends nothing.
  it('offers no close while the pane has no mount, and sends nothing', () => {
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ kind: 'state', threadId: 'thread-browser', pages: [] })));
    focusPane('browser-main');
    expect(isBrowserPaneUnattached()).toBe(true);
    expect(closeFocusedBrowserTab()).toBe(false);
    expect(act).not.toHaveBeenCalled();
  });

  // The banner is the pane's that sent: a refusal that answers after its
  // pane closed is logged, and never shown in the pane that replaced it.
  it('puts a refusal in the banner of the pane that sent it, and logs it once that pane closed', async () => {
    resetFrontendErrorCaptureForTest();
    const refusals: Array<(err: Error) => void> = [];
    setBindingMock('BrowserCompanionDo', () => new Promise((_resolve, reject) => refusals.push(reject)));
    const first = await attachBrowserPaneForTest('thread-browser', 'mount-1');
    const reload = actInPane(paneForTest('thread-browser'), browserAction.onTab('reload', pageIdForTest('page-1')));
    first.release();
    await attachBrowserPaneForTest('thread-browser', 'mount-2');
    refusals[0](new Error('reload refused'));
    expect(await reload).toEqual(expect.objectContaining({ error: 'reload refused' }));
    expect(browserActionError('thread-browser')).toBe('');
    expect(frontendErrorCaptureStateForTest().pendingCount).toBe(1);

    // The pane attached now shows its own.
    const next = actInPane(paneForTest('thread-browser'), browserAction.onTab('reload', pageIdForTest('page-1')));
    refusals[1](new Error('second refused'));
    await next;
    expect(browserActionError('thread-browser')).toBe('second refused');
    resetFrontendErrorCaptureForTest();
  });

  // A page id is minted only from a nonblank string, so no page-naming
  // action can be built for an empty page.
  // Whitespace alone is blank: an empty string is refused before the
  // validator runs, so only these reach it.
  it('mints no page or download id from a blank string', () => {
    expect(pageIdOf('')).toBeNull();
    expect(pageIdOf('  ')).toBeNull();
    expect(pageIdOf('\t\n')).toBeNull();
    expect(pageIdOf('a')).toBe('a');
    expect(downloadIdOf('')).toBeNull();
    expect(downloadIdOf('  ')).toBeNull();
    expect(downloadIdOf('\t\n')).toBeNull();
    expect(downloadIdOf('d')).toBe('d');
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
    expect(await browserCompanionAct('thread-browser', browserAction.hide()))
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
    const failure = await browserCompanionAct(THREAD, browserAction.hide());
    expect(failure?.error).toContain('Two computers claim this conversation');
    expect(await openBrowserTab(THREAD, { address: 'https://example.com/' })).not.toBeNull();
    expect(act).not.toHaveBeenCalled();
  });

  it('sends nothing for a header press made after the claim was contested, and says why', async () => {
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ kind: 'state', threadId: THREAD, pages: [] })));
    const intent = browserHeaderIntent(THREAD);
    contest();
    await runBrowserHeaderIntent(intent);
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
    expect(addressFocusIntent(THREAD)).toEqual([]);
    // Not while it is created: there is no page to take an address yet.
    expect(takeAddressFocus(THREAD, { id: 'n', creating: true })).toBe(false);
    answer();
    expect(await opened).toBeNull();
    expect(addressFocusIntent(THREAD)).toEqual(['n']);
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

  // Two New Tab presses, the second made while the first is unanswered:
  // each press's request belongs to the tab it opens. In the order the
  // backend sends them, the first tab is created and becomes ready while the
  // second press is due, and its answer names it, which the pane reads with
  // the first tab shown; then the second tab is created, becomes ready, and
  // its answer names it. Only the second tab takes the focus: the second
  // press moved the user on from the first.
  for (const firstFails of [false, true]) {
    it(`focuses the second new tab when the first ${firstFails ? 'fails' : 'becomes ready'} first`, async () => {
      const answers: Array<(pageId: string) => void> = [];
      setBindingMock('BrowserCompanionDo', vi.fn(() => new Promise((resolve, reject) => {
        answers.push((pageId) => {
          if (!pageId) reject(new Error('refused'));
          else resolve({ kind: 'state', threadId: THREAD, activePageId: pageId, visible: true, pages: [tab(pageId)], pageId });
        });
      })));
      // What the pane does with each state it shows: asks for the focus.
      const shown = (id: string, extra: Record<string, unknown> = {}) => takeAddressFocus(THREAD, { id, ...extra });
      const first = openBrowserTab(THREAD, { focusAddress: true });
      await vi.waitFor(() => expect(answers).toHaveLength(1));
      expect(shown('a', { creating: true })).toBe(false);
      // The user presses New again.
      window.dispatchEvent(new Event('pointerdown'));
      const second = openBrowserTab(THREAD, { focusAddress: true });
      await vi.waitFor(() => expect(answers).toHaveLength(2));
      expect(shown('a', firstFails ? { error: 'refused' } : {})).toBe(false);
      answers[0](firstFails ? '' : 'a');
      await first;
      // The pane reads the answer with the first tab still shown.
      expect(shown('a', firstFails ? { error: 'refused' } : {})).toBe(false);
      expect(addressFocusIntent(THREAD)).toEqual([]);
      expect(shown('b', { creating: true })).toBe(false);
      expect(shown('b')).toBe(false);
      answers[1]('b');
      await second;
      expect(addressFocusIntent(THREAD)).toEqual(['b']);
      expect(shown('b')).toBe(true);
      expect(addressFocusIntent(THREAD)).toBeNull();
    });
  }

  // Two header shows converge on one tab, which only the answer of the show
  // that opened it names: they share one request, and that tab takes it.
  it('shares one focus request between overlapping header shows', async () => {
    const answers: Array<(pageId: string) => void> = [];
    setBindingMock('BrowserCompanionDo', vi.fn(() => new Promise((resolve) => {
      answers.push((pageId) => resolve({ kind: 'state', threadId: THREAD, activePageId: 'n', visible: true, pages: [tab('n')], pageId }));
    })));
    const ticket = armAddressFocus(THREAD, 'show');
    window.dispatchEvent(new Event('pointerdown'));
    const joined = armAddressFocus(THREAD, 'show');
    expect(joined.request).toBe(ticket.request);
    settleAddressFocus(ticket, 'n');
    settleAddressFocus(joined, '');
    expect(takeAddressFocus(THREAD, { id: 'n' })).toBe(true);
  });

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
    await vi.waitFor(() => expect(pane.current?.pane).toBeTruthy());
    void actInPane(paneForTest(THREAD), browserAction.select(pageIdForTest('a')));
    void actInPane(paneForTest(THREAD), browserAction.step(1));
    void actInPane(paneForTest(THREAD), browserAction.onTab('close', pageIdForTest('a')));
    void openBrowserTabInPane(paneForTest(THREAD));
    void actInPane(paneForTest(THREAD), browserAction.reopen());
    void actInPane(paneForTest(THREAD), browserAction.duplicate(pageIdForTest('a')));
    void actInPane(paneForTest(THREAD), browserAction.download('download-open', downloadIdOf('d') ?? fail()));
    void actInPane(paneForTest(THREAD), browserAction.onTab('restore', pageIdForTest('a')));
    expect(sent.map(order)).toEqual([
      { kind: 'activate', mount: 'mount-1', select: 1 },
      { kind: 'step', mount: 'mount-1', select: 2 },
      { kind: 'close', mount: 'mount-1', select: 3 },
      { kind: 'new', mount: 'mount-1', select: 4 },
      { kind: 'reopen', mount: 'mount-1', select: 5 },
      { kind: 'duplicate', mount: 'mount-1', select: 6 },
      { kind: 'download-open', mount: 'mount-1', select: 7 },
      { kind: 'restore', mount: '', select: 0 },
    ]);

    // The connection drops and the pane attaches again as a new mount, which
    // the backend counts from 1.
    pane.release();
    pane = attachBrowserCompanion(THREAD);
    await vi.waitFor(() => expect(pane.current?.pane).toBeTruthy());
    sent = [];
    void actInPane(paneForTest(THREAD), browserAction.select(pageIdForTest('a')));
    void actInPane(paneForTest(THREAD), browserAction.step(-1));
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
    await vi.waitFor(() => expect(pane.current?.pane).toBeTruthy());
    void actInPane(paneForTest(THREAD), browserAction.select(pageIdForTest('a')));
    pane.release();
    pane = attachBrowserCompanion(THREAD);
    await vi.waitFor(() => expect(pane.current?.pane).toBeTruthy());
    void actInPane(paneForTest(THREAD), browserAction.select(pageIdForTest('a')));
    expect(sent.map(order)).toEqual([
      { kind: 'activate', mount: 'mount-1', select: 1 },
      { kind: 'activate', mount: 'mount-1', select: 1 },
    ]);
    pane.release();
  });

  // With no pane mounted there is no attachment to send a selection from;
  // a new tab from the header or a link applies in arrival order.
  it('sends a new tab opened with no pane mounted unnumbered', () => {
    void openBrowserTab(THREAD);
    expect(sent.map(order)).toEqual([{ kind: 'new', mount: '', select: 0 }]);
  });
});

function fail(): never {
  throw new Error('a blank id');
}
