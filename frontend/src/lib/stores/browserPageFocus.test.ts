import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actInPane } from './browserActions';
import { browserAction } from './browserActionWire';
import { pageIdForTest, paneForTest } from '../../test/helpers/browserPane';
import {
  applyBrowserCompanionState,
  attachBrowserCompanion,
  browserActionError,
  noteBrowserPaneError,
  resetBrowserCompanionForTest,
} from './browserCompanion.svelte';
import { focusBrowserPage } from './browserPageFocus';
import { installCompanionPanes, resetCompanionPanesForTest } from './companionPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { createPane, resetPanesForTest } from './panes.svelte';
import { makeThread } from '../../test/helpers/chat';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { setHostWindowFromBootstrap } from '../transport/pageHost';

const page = (url: string, title: string) => ({ id: 'page-1', url, title, canGoBack: false, canGoForward: false });

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

  // The detach withdraws the mount's intent at the backend, and a pane
  // that detached sends nothing more: the launcher is told at once.
  it('withdraws a request still on its way at the launcher when the pane unmounts', async () => {
    const detach = setBindingMock('BrowserCompanionPaneDetach', vi.fn(async () => {}));
    deferFocusAnswers();
    const request = focusBrowserPage(THREAD, 'a', area);
    pane.release();
    expect(posted.at(-1)).toBe('ao:pane-focus:{"mount":"mount-1","intent":1,"withdrawn":true}');
    await vi.waitFor(() => expect(detach).toHaveBeenCalledWith('mount-1'));
    expect(withdrawals()).toEqual([]);
    answerFocus();
    expect(await request).toBeNull();
    composer.focus();
    expect(withdrawals()).toEqual([]);
  });

  it('withdraws on the next choice however long the request took', async () => {
    vi.useFakeTimers();
    try {
      deferFocusAnswers();
      const request = focusBrowserPage(THREAD, 'a', area);
      // Past every bound the backend and the launcher keep: only they know
      // when they heard of it.
      vi.advanceTimersByTime(60_000);
      composer.focus();
      expect(posted.at(-1)).toBe('ao:pane-focus:{"mount":"mount-1","intent":1,"withdrawn":true}');
      expect(withdrawals()).toEqual([expect.objectContaining({ mount: 'mount-1', intent: 1 })]);
      answerFocus();
      await request;
    } finally {
      vi.useRealTimers();
    }
  });

  it('withdraws a request whose arm failed on the next choice: it may have landed', async () => {
    setBindingMock('BrowserCompanionDo', vi.fn(async (_thread: string, action: Action) => {
      actions.push({ ...action });
      if (action.kind === 'focus-page') throw new Error('connection lost');
      return shown;
    }));
    const failure = await focusBrowserPage(THREAD, 'a', area);
    expect(failure?.error).toContain('connection lost');
    composer.focus();
    expect(posted.at(-1)).toBe('ao:pane-focus:{"mount":"mount-1","intent":1,"withdrawn":true}');
    expect(withdrawals()).toEqual([expect.objectContaining({ mount: 'mount-1', intent: 1 })]);
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
    expect(browserActionError(THREAD)).toContain('E_FAIL');
    // The page's state, admitted after, leaves the banner up.
    applyBrowserCompanionState({ ...shown, seq: 100 });
    expect(browserActionError(THREAD)).toContain('E_FAIL');
    expect(pane.error).toBeNull();
  });

  it('never lets an obsolete refusal overwrite a newer banner', async () => {
    deferFocusAnswers();
    const refusal = (intent: number) => ({ kind: 'focus', threadId: THREAD, mount: 'mount-1', focusIntent: intent, error: `refused ${intent}` });

    // A withdrawn intent's refusal.
    const first = focusBrowserPage(THREAD, 'a', area);
    composer.focus();
    applyBrowserCompanionState(refusal(1));
    expect(browserActionError(THREAD)).toBe('');

    // A superseded intent's refusal.
    const second = focusBrowserPage(THREAD, 'a', area);
    const third = focusBrowserPage(THREAD, 'a', area);
    applyBrowserCompanionState(refusal(2));
    expect(browserActionError(THREAD)).toBe('');

    // A navigation started after the request failed: its banner stays.
    setBindingMock('BrowserCompanionDo', vi.fn(async () => {
      throw new Error('navigation refused');
    }));
    await actInPane(paneForTest(THREAD), browserAction.navigate(pageIdForTest('a'), 'https://example.test/x'));
    applyBrowserCompanionState(refusal(3));
    expect(browserActionError(THREAD)).toBe('navigation refused');
    answerFocus();
    await Promise.all([first, second, third]);

    // One that succeeded leaves no banner, so the refusal shows.
    deferFocusAnswers();
    const fourth = focusBrowserPage(THREAD, 'a', area);
    setBindingMock('BrowserCompanionDo', vi.fn(async () => shown));
    await actInPane(paneForTest(THREAD), browserAction.navigate(pageIdForTest('a'), 'https://example.test/y'));
    applyBrowserCompanionState(refusal(4));
    expect(browserActionError(THREAD)).toBe('refused 4');
    answerFocus();
    await fourth;
  });

  it('puts a refused request in the banner, and leaves a banner already up when it succeeds', async () => {
    setBindingMock('BrowserCompanionDo', vi.fn(async () => {
      throw new Error('browser: pane mount not found');
    }));
    const failure = await focusBrowserPage(THREAD, 'a', area);
    expect(failure?.error).toContain('pane mount not found');
    expect(browserActionError(THREAD)).toContain('pane mount not found');

    // The navigation the focus follows failed: its banner stays.
    noteBrowserPaneError(paneForTest(THREAD), 'navigation refused');
    setBindingMock('BrowserCompanionDo', vi.fn(async () => shown));
    expect(await focusBrowserPage(THREAD, 'a', area)).toBeNull();
    expect(browserActionError(THREAD)).toBe('navigation refused');
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

