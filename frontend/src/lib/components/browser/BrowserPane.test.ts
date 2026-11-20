import { tick } from 'svelte';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import BrowserPane from './BrowserPane.svelte';
import { makeStubPanelContext } from '../../../test/helpers/panelContext';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';
import { applyBrowserCompanionState, noteBrowserPaneError, resetBrowserCompanionForTest } from '../../stores/browserCompanion.svelte';
import { paneForTest } from '../../../test/helpers/browserPane';
import { openBrowserTab } from '../../stores/browserActions';
import { BROWSER_FIND_DEBOUNCE_MS, BROWSER_FIND_STOP_ATTEMPTS, BROWSER_FIND_STOP_RETRY_MS, resetBrowserFindForTest } from '../../stores/browserFind.svelte';
import { closePaneFind, hasPaneFindProvider, isPaneFindOpen, openPaneFind, resetPaneFindForTest } from '../../stores/paneFind.svelte';
import { installCompanionPanes, openCompanion, resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { createPane, resetPanesForTest } from '../../stores/panes.svelte';
import { getToasts, resetToastsForTest } from '../../stores/toast.svelte';
import { forgetBackendEntities, forgetThread, noteThread } from '../../transport/entityIndex';
import { HOME_BACKEND } from '../../transport/backendKey';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';

const THREAD = 'thread-1';

function page(id: string, suspended: boolean) {
  return {
    id,
    url: `https://example.test/${id}`,
    title: `Page ${id}`,
    canGoBack: false,
    canGoForward: false,
    ...(suspended ? { suspended: true } : {}),
  };
}

function state(pages: ReturnType<typeof page>[]) {
  return { kind: 'state', threadId: THREAD, activePageId: pages[0]?.id ?? '', visible: true, pages };
}

describe('<BrowserPane> suspended pages', () => {
  beforeEach(() => {
    resetBrowserCompanionForTest();
    setBindingMock('BrowserCompanionPaneDetach', async () => undefined);
    setBindingMock('BrowserCompanionPaneRect', async () => undefined);
  });
  afterEach(() => resetBrowserCompanionForTest());

  it('restores a suspended active page by presenting it, once', async () => {
    setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: 'mount-1', state: state([page('a', true), page('b', false)]) }));
    let restore!: () => void;
    const act = setBindingMock(
      'BrowserCompanionDo',
      vi.fn(() => new Promise((resolve) => {
        restore = () => resolve(state([page('a', false), page('b', false)]));
      })),
    );
    render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD }) });

    await waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    expect(act.mock.calls[0][0]).toBe(THREAD);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'restore', pageId: 'a' });
    // A restore is not the user's selection: it carries no number.
    expect((act.mock.calls[0][1] as { select?: number }).select ?? 0).toBe(0);
    // A state push while the restore runs asks nothing more.
    applyBrowserCompanionState(state([page('a', true), page('b', false)]) as never);
    await tick();
    restore();
    await tick();
    expect(act).toHaveBeenCalledTimes(1);
  });

  it('reports a refused restore in the banner and asks again only once the page was live', async () => {
    setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: 'mount-1', state: state([page('a', true)]) }));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => {
      throw new Error('browser: page a cannot be restored: file is not allowed');
    }));
    const view = render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD }) });

    expect((await view.findByRole('alert')).textContent).toContain('cannot be restored');
    applyBrowserCompanionState(state([page('a', true)]) as never);
    await tick();
    expect(act).toHaveBeenCalledTimes(1);
    // The page came back and was suspended again: presenting it restores it.
    applyBrowserCompanionState(state([page('a', false)]) as never);
    await tick();
    applyBrowserCompanionState(state([page('a', true)]) as never);
    await waitFor(() => expect(act).toHaveBeenCalledTimes(2));
  });

  it('takes no address until it knows the page it shows, then navigates that page', async () => {
    let attach!: () => void;
    setBindingMock('BrowserCompanionPaneAttach', () => new Promise((resolve) => {
      attach = () => resolve({ id: 'mount-1', state: state([page('a', false)]) });
    }));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => state([page('a', false)])));
    const view = render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD }) });
    const address = view.getByRole('textbox', { name: 'Address' }) as HTMLInputElement;

    // An Enter here would have no page to name, and the backend opens a new
    // page for a navigation that names none.
    expect(address.disabled).toBe(true);
    await fireEvent.keyDown(address, { key: 'Enter' });
    expect(act).not.toHaveBeenCalled();

    attach();
    await waitFor(() => expect(address.disabled).toBe(false));
    await fireEvent.focus(address);
    await fireEvent.input(address, { target: { value: 'notes.html' } });
    await fireEvent.keyDown(address, { key: 'Enter' });
    // Leaving the address also asks the engine to focus the page; exactly
    // one navigation goes out, and it names the page.
    const navigations = act.mock.calls
      .map((call) => call[1] as { kind: string })
      .filter((action) => action.kind === 'navigate');
    expect(navigations).toHaveLength(1);
    expect(navigations[0]).toMatchObject({ kind: 'navigate', pageId: 'a', address: 'notes.html' });
  });

  // The tabs a known state shows are not the pane's to act on until its
  // attach answers: the chrome is unavailable, and a press sends nothing.
  it('makes its chrome unavailable until its attach answers, and sends nothing from it', async () => {
    applyBrowserCompanionState(state([page('a', false), page('b', false)]) as never);
    let attach!: () => void;
    setBindingMock('BrowserCompanionPaneAttach', () => new Promise((resolve) => {
      attach = () => resolve({ id: 'mount-1', state: state([page('a', false), page('b', false)]) });
    }));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => state([page('a', false), page('b', false)])));
    const view = render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD }) });
    const chrome = view.getByTestId('browser-pane-chrome');
    expect(chrome.inert).toBe(true);
    expect(chrome.getAttribute('aria-busy')).toBe('true');
    const tabs = view.getAllByTestId('browser-tab');
    await fireEvent.click(tabs[1]);
    expect(act).not.toHaveBeenCalled();

    attach();
    await waitFor(() => expect(chrome.inert).toBe(false));
    expect(chrome.getAttribute('aria-busy')).toBe('false');
    await fireEvent.click(view.getAllByTestId('browser-tab')[1]);
    await waitFor(() => expect(act).toHaveBeenCalledWith(THREAD, expect.objectContaining({ kind: 'activate', pageId: 'b', mount: 'mount-1' })));
  });

  // Two computers claim the thread at one ownership epoch: this window may
  // not host its browser, so the attached pane's chrome is unavailable and
  // the home computer's tabs are not shown.
  it('makes its chrome unavailable and shows no tab once two computers claim the thread', async () => {
    setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: 'mount-1', state: state([page('a', false), page('b', false)]) }));
    setBindingMock('BrowserCompanionDo', vi.fn(async () => state([page('a', false), page('b', false)])));
    const view = render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD }) });
    const chrome = view.getByTestId('browser-pane-chrome');
    await waitFor(() => expect(chrome.inert).toBe(false));
    expect(view.getAllByTestId('browser-tab')).toHaveLength(2);
    const detach = setBindingMock('BrowserCompanionPaneDetach', vi.fn(async () => {}));
    stageBackend({ id: 'remote', backendId: 'remote' });
    try {
      noteThread(THREAD, HOME_BACKEND, 4);
      noteThread(THREAD, 'remote', 4);
      await tick();
      expect(chrome.inert).toBe(true);
      expect(view.queryAllByTestId('browser-tab')).toHaveLength(0);
      // The mount goes with the ownership, which takes the native view down.
      await waitFor(() => expect(detach).toHaveBeenCalledWith('mount-1'));
      // This computer owns it again: the pane attaches afresh.
      const attach = setBindingMock('BrowserCompanionPaneAttach', vi.fn(async () => ({ id: 'mount-2', state: state([page('a', false), page('b', false)]) })));
      noteThread(THREAD, HOME_BACKEND, 5);
      await waitFor(() => expect(chrome.inert).toBe(false));
      expect(attach).toHaveBeenCalledTimes(1);
    } finally {
      forgetThread(THREAD);
      forgetBackendEntities('remote');
      resetStagedBackends();
    }
  });

  it('asks nothing for a live active page', async () => {
    setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: 'mount-1', state: state([page('a', false), page('b', true)]) }));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => state([page('a', false), page('b', true)])));
    const view = render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD }) });
    await view.findByText('Page a');
    await tick();
    expect(act).not.toHaveBeenCalled();
  });
});

describe('<BrowserPane> find', () => {
  function resetAll(): void {
    resetBrowserCompanionForTest();
    resetBrowserFindForTest();
    resetPaneFindForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
  }

  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    resetAll();
    installCompanionPanes();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
    createPane('main').replaceThread(makeThread({ id: THREAD }));
    openCompanion('main', 'browser');
    setBindingMock('BrowserCompanionPaneDetach', async () => undefined);
    setBindingMock('BrowserCompanionPaneRect', async () => undefined);
    setBindingMock('BrowserCompanionDo', async () => state([page('a', false)]));
  });
  afterEach(() => {
    vi.useRealTimers();
    resetAll();
  });

  function mount(findMode: string) {
    setBindingMock('BrowserCompanionPaneAttach', async () => ({
      id: 'mount-1',
      state: { ...state([page('a', false)]), findMode },
    }));
    return render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD, paneId: 'main' }) });
  }

  it('drives the shared bar on the shown page and closes it when the backend ends the find', async () => {
    let seq = 0;
    const find = setBindingMock('BrowserCompanionFind', vi.fn(async (_thread: string, action: { kind: string; query?: string; mount: string; seq: number }) => {
      seq = action.seq;
      return {
        pageId: 'a', incarnation: 1, mount: action.mount, seq, document: 1, session: 1, active: action.kind !== 'stop', query: action.query ?? '', found: true, matches: 2, current: 0, capped: false,
      };
    }));
    const view = mount('bar');
    await waitFor(() => expect(hasPaneFindProvider('browser-main')).toBe(true));
    expect(view.queryByTestId('pane-find-bar')).toBeNull();
    openPaneFind('browser-main');
    const input = (await view.findByTestId('pane-find-input')) as HTMLInputElement;
    expect(input.placeholder).toBe('Find in page');
    await fireEvent.input(input, { target: { value: 'alpha' } });
    await vi.advanceTimersByTimeAsync(BROWSER_FIND_DEBOUNCE_MS);
    expect(find).toHaveBeenCalledWith(THREAD, expect.objectContaining({ kind: 'start', pageId: 'a', query: 'alpha', mount: 'mount-1' }));
    await waitFor(() => expect(view.getByTestId('pane-find-count').textContent?.trim()).toBe('1/2'));

    applyBrowserCompanionState({
      kind: 'find', threadId: THREAD, pageId: 'a',
      find: { pageId: 'a', incarnation: 1, mount: 'mount-1', seq, document: 1, session: 2, active: false, query: '', found: false, matches: 0, current: -1, capped: false },
    } as never);
    await waitFor(() => expect(view.queryByTestId('pane-find-bar')).toBeNull());
  });

  // A refusal is the refused search's result, in the bar: the next query
  // replaces it, and the pane shows no alert for it at any point.
  it('shows a refused query in the bar only, until a short query finds', async () => {
    const tooLong = 'that search is too long; shorten it to 1024 bytes or fewer';
    setBindingMock('BrowserCompanionFind', vi.fn(async (_thread: string, action: { kind: string; query?: string; mount: string; seq: number }) => {
      if (action.kind === 'start' && (action.query ?? '').length > 1024) throw new Error(tooLong);
      return {
        pageId: 'a', incarnation: 1, mount: action.mount, seq: action.seq, document: 1, session: 1, active: action.kind !== 'stop', query: action.query ?? '', found: true, matches: 2, current: 0, capped: false,
      };
    }));
    const view = mount('bar');
    await waitFor(() => expect(hasPaneFindProvider('browser-main')).toBe(true));
    openPaneFind('browser-main');
    const input = await view.findByTestId('pane-find-input');
    await fireEvent.input(input, { target: { value: 'a'.repeat(1025) } });
    await vi.advanceTimersByTimeAsync(BROWSER_FIND_DEBOUNCE_MS);
    await waitFor(() => expect(view.getByTestId('pane-find-error').textContent).toBe(`Find failed: ${tooLong}`));
    expect(view.getAllByRole('alert'), 'the pane shows no alert of its own').toHaveLength(1);
    await fireEvent.input(input, { target: { value: 'alpha' } });
    await vi.advanceTimersByTimeAsync(BROWSER_FIND_DEBOUNCE_MS);
    await waitFor(() => expect(view.getByTestId('pane-find-count').textContent?.trim()).toBe('1/2'));
    expect(view.queryByTestId('pane-find-error')).toBeNull();
    expect(view.queryAllByRole('alert')).toHaveLength(0);
    closePaneFind('browser-main');
    openPaneFind('browser-main');
    await tick();
    expect(view.queryAllByRole('alert')).toHaveLength(0);
  });

  // A stop that cannot be delivered after its retries may leave highlights:
  // the pane says so, apart from browser action errors, until a later stop
  // answers.
  it('shows an undelivered stop on the pane until a later stop answers, apart from action errors', async () => {
    let refusing = false;
    setBindingMock('BrowserCompanionFind', vi.fn(async (_thread: string, action: { kind: string; query?: string; mount: string; seq: number }) => {
      if (action.kind === 'stop' && refusing) throw new Error('fake engine: find stop failed');
      return {
        pageId: 'a', incarnation: 1, mount: action.mount, seq: action.seq, document: 1, session: 1, active: action.kind !== 'stop', query: action.query ?? '', found: true, matches: 2, current: 0, capped: false,
      };
    }));
    const view = mount('bar');
    await waitFor(() => expect(hasPaneFindProvider('browser-main')).toBe(true));
    openPaneFind('browser-main');
    const input = await view.findByTestId('pane-find-input');
    await fireEvent.input(input, { target: { value: 'alpha' } });
    await vi.advanceTimersByTimeAsync(BROWSER_FIND_DEBOUNCE_MS);
    await waitFor(() => expect(view.getByTestId('pane-find-count').textContent?.trim()).toBe('1/2'));
    refusing = true;
    closePaneFind('browser-main');
    await vi.advanceTimersByTimeAsync(BROWSER_FIND_STOP_RETRY_MS * BROWSER_FIND_STOP_ATTEMPTS);
    const cleanup = await view.findByTestId('browser-find-cleanup-error');
    expect(cleanup.textContent).toBe('Find could not clear the page: fake engine: find stop failed');
    noteBrowserPaneError(paneForTest(THREAD), '');
    await tick();
    expect(view.queryByTestId('browser-find-cleanup-error'), 'a browser action does not clear it').not.toBeNull();
    refusing = false;
    openPaneFind('browser-main');
    await fireEvent.input(await view.findByTestId('pane-find-input'), { target: { value: 'beta' } });
    await vi.advanceTimersByTimeAsync(BROWSER_FIND_DEBOUNCE_MS);
    closePaneFind('browser-main');
    await waitFor(() => expect(view.queryByTestId('browser-find-cleanup-error')).toBeNull());
    expect(view.queryAllByRole('alert')).toHaveLength(0);
  });

  it('opens the engine find bar instead of its own for an engine that has one', async () => {
    const find = setBindingMock('BrowserCompanionFind', vi.fn(async () => ({
      pageId: 'a', session: 1, active: true, query: 'seed', found: false, matches: 0, current: -1, capped: false,
    })));
    const view = mount('engine');
    await waitFor(() => expect(hasPaneFindProvider('browser-main')).toBe(true));
    expect(openPaneFind('browser-main', { seed: 'seed' })).toBe(true);
    await tick();
    expect(find).toHaveBeenCalledWith(THREAD, expect.objectContaining({ kind: 'start', pageId: 'a', query: 'seed' }));
    expect(view.queryByTestId('pane-find-bar')).toBeNull();
  });

  it('closing the pane closes the find on the backend, each time it is opened', async () => {
    const find = setBindingMock('BrowserCompanionFind', vi.fn(async (_thread: string, action: { kind: string; mount: string }) => ({
      pageId: 'a', incarnation: 1, mount: action.mount, document: 1, session: 1, active: action.kind === 'start', query: '', found: true, matches: 1, current: 0, capped: false,
    })));
    for (let round = 0; round < 2; round++) {
      const view = mount('bar');
      await waitFor(() => expect(hasPaneFindProvider('browser-main')).toBe(true));
      openPaneFind('browser-main');
      await fireEvent.input(await view.findByTestId('pane-find-input'), { target: { value: 'alpha' } });
      await vi.advanceTimersByTimeAsync(BROWSER_FIND_DEBOUNCE_MS);
      await waitFor(() => expect(view.getByTestId('pane-find-count').textContent?.trim()).toBe('1/1'));
      view.unmount();
      await waitFor(() => expect(find.mock.calls.at(-1)?.[1]).toMatchObject({ kind: 'stop', pageId: 'a', mount: 'mount-1' }));
      expect(hasPaneFindProvider('browser-main')).toBe(false);
    }
    expect(find.mock.calls.map((args) => (args[1] as { kind: string }).kind))
      .toEqual(['start', 'stop', 'start', 'stop']);
  });

  // The thread's state can be known before the pane's attach answers; a find
  // then would address no mount.
  it('offers find only once its mount is attached', async () => {
    let attach!: () => void;
    setBindingMock('BrowserCompanionPaneAttach', () => new Promise((resolve) => {
      attach = () => resolve({ id: 'mount-1', state: { ...state([page('a', false)]), findMode: 'bar' } });
    }));
    applyBrowserCompanionState({ ...state([page('a', false)]), findMode: 'bar' } as never);
    render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD, paneId: 'main' }) });
    await vi.advanceTimersByTimeAsync(50);
    expect(hasPaneFindProvider('browser-main')).toBe(false);
    attach();
    await waitFor(() => expect(hasPaneFindProvider('browser-main')).toBe(true));
  });

  // A bare Escape pressed in the page is the page's; the backend announces
  // it, and the pane closes its open bar for the page it shows.
  it('closes its open bar on an Escape announced for its shown page, and only then', async () => {
    setBindingMock('BrowserCompanionFind', vi.fn(async (_thread: string, action: { kind: string; query?: string; mount: string; seq: number }) => ({
      pageId: 'a', incarnation: 1, mount: action.mount, seq: action.seq, document: 1, session: 1, active: action.kind !== 'stop', query: action.query ?? '', found: true, matches: 2, current: 0, capped: false,
    })));
    const view = mount('bar');
    await waitFor(() => expect(hasPaneFindProvider('browser-main')).toBe(true));
    const escape = (pageId: string) => applyBrowserCompanionState({ kind: 'escape', threadId: THREAD, pageId, mount: 'mount-1' } as never);
    escape('a');
    expect(isPaneFindOpen('browser-main')).toBe(false);
    openPaneFind('browser-main');
    await view.findByTestId('pane-find-input');
    escape('b');
    await tick();
    expect(view.queryByTestId('pane-find-bar')).not.toBeNull();
    escape('a');
    await waitFor(() => expect(view.queryByTestId('pane-find-bar')).toBeNull());
    expect(isPaneFindOpen('browser-main')).toBe(false);
  });

  it('offers no find where the engine has none', async () => {
    const view = mount('');
    await view.findByText('Page a');
    await tick();
    expect(hasPaneFindProvider('browser-main')).toBe(false);
  });
});

describe('<BrowserPane> tab lifecycle', () => {
  function resetAll(): void {
    resetBrowserCompanionForTest();
    resetToastsForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
  }

  beforeEach(() => {
    resetAll();
    installCompanionPanes();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
    createPane('main').replaceThread(makeThread({ id: THREAD }));
    openCompanion('main', 'browser');
    setBindingMock('BrowserCompanionPaneDetach', async () => undefined);
    setBindingMock('BrowserCompanionPaneRect', async () => undefined);
    // What the address suggestions read when the field takes focus.
    setBindingMock('BrowserCompanionRecentAddresses', async () => []);
    setBindingMock('GetDevServers', async () => ({ previewHost: '', servers: [] }));
  });
  afterEach(() => resetAll());

  function mount(pages: Record<string, unknown>[], activePageId = String(pages[0]?.id ?? '')) {
    setBindingMock('BrowserCompanionPaneAttach', async () => ({
      id: 'mount-1',
      state: { kind: 'state', threadId: THREAD, activePageId, visible: true, pages },
    }));
    return render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD, paneId: 'main' }) });
  }

  it('shows each tab state and says why the active tab has no page', async () => {
    setBindingMock('BrowserCompanionDo', async () => undefined);
    const view = mount([
      { ...page('a', false), title: '', url: 'about:blank', creating: true },
      { ...page('b', false), error: 'profile is locked' },
      { ...page('c', false), loading: true },
      { ...page('d', false), favicon: 'data:image/png;base64,AAAA' },
    ]);
    await waitFor(() => expect(view.getAllByTestId('browser-tab')).toHaveLength(4));
    const tabs = view.getAllByTestId('browser-tab');
    expect(tabs.map((tab) => tab.dataset.tabState)).toEqual(['creating', 'error', 'loading', 'ready']);
    expect(tabs[0].textContent).toContain('Opening…');
    expect(tabs[3].querySelector('img')?.getAttribute('src')).toBe('data:image/png;base64,AAAA');
    expect(view.getByTestId('browser-pane-pending').textContent).toBe('Opening…');
    expect((view.getByRole('textbox', { name: 'Address' }) as HTMLInputElement).disabled).toBe(true);

    applyBrowserCompanionState({ kind: 'state', threadId: THREAD, activePageId: 'b', visible: true, pages: [{ ...page('b', false), error: 'profile is locked' }] } as never);
    await waitFor(() => expect(view.getByTestId('browser-pane-pending').textContent).toBe('This tab could not be opened: profile is locked'));
  });

  it('shows a crashed page with its reason and a Reload, and restores it only from the Reload', async () => {
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => undefined));
    const crashed = { ...page('a', true), error: "the page's process ended (crashed)" };
    const view = mount([crashed, page('b', false)]);
    await waitFor(() => expect(view.getByTestId('browser-pane-pending').textContent).toContain('This page stopped'));
    expect(view.getByTestId('browser-pane-pending-note').textContent).toBe("This page stopped: the page's process ended (crashed)");
    expect(view.getAllByTestId('browser-tab')[0].dataset.tabState).toBe('error');
    expect(view.getAllByTestId('browser-tab')[0].closest('[data-tab-id]')?.getAttribute('title')).toContain('This page stopped');
    // Presenting it asks for no restore, and the address still takes one.
    await tick();
    expect(act).not.toHaveBeenCalled();
    expect((view.getByRole('textbox', { name: 'Address' }) as HTMLInputElement).disabled).toBe(false);
    expect((view.getByTestId('browser-reload') as HTMLButtonElement).disabled).toBe(false);

    await fireEvent.click(view.getByTestId('browser-pane-crash-reload'));
    expect(act).toHaveBeenCalledTimes(1);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'reload', pageId: 'a' });
    // While it restores the pane says so, with no second Reload.
    applyBrowserCompanionState({ kind: 'state', threadId: THREAD, seq: 99, activePageId: 'a', visible: true, pages: [{ ...crashed, restoring: true }, page('b', false)] } as never);
    await waitFor(() => expect(view.getByTestId('browser-pane-pending-note').textContent).toBe('Restoring…'));
    expect(view.queryByTestId('browser-pane-crash-reload')).toBeNull();
  });

  it('focuses the address with its text selected for the tab the user opened, once it is ready', async () => {
    let answer!: () => void;
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async (_thread: string, action: { kind: string }) => {
      expect(action.kind).toBe('new');
      applyBrowserCompanionState({ kind: 'state', threadId: THREAD, seq: 1, activePageId: 'n', visible: true, pages: [page('a', false), { ...page('n', false), url: 'about:blank', title: '', creating: true }] } as never);
      await new Promise<void>((resolve) => { answer = resolve; });
      return { kind: 'state', threadId: THREAD, seq: 2, activePageId: 'n', visible: true, pages: [page('a', false), { ...page('n', false), url: 'https://example.test/n', title: '' }], pageId: 'n' };
    }));
    const view = mount([page('a', false)]);
    const address = view.getByRole('textbox', { name: 'Address' }) as HTMLInputElement;
    await waitFor(() => expect(address.disabled).toBe(false));
    await fireEvent.click(view.getByRole('button', { name: 'New tab' }));
    await waitFor(() => expect(view.getAllByTestId('browser-tab').map((tab) => tab.dataset.tabState)).toEqual(['ready', 'creating']));
    expect(document.activeElement).not.toBe(address);
    answer();
    await waitFor(() => expect(document.activeElement).toBe(address));
    expect(address.value).toBe('https://example.test/n');
    expect([address.selectionStart, address.selectionEnd]).toEqual([0, address.value.length]);
    expect(act).toHaveBeenCalledTimes(1);
  });

  // The header's new tab opens the pane with the tab: the tab is ready
  // before the pane's attach answers, and the bar takes focus once it does.
  it('focuses the address of a tab ready before the pane attached, once the attach answers', async () => {
    const ready = [page('n', false)];
    setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ kind: 'state', threadId: THREAD, seq: 2, activePageId: 'n', visible: true, pages: ready, pageId: 'n' })));
    let attach!: () => void;
    setBindingMock('BrowserCompanionPaneAttach', () => new Promise((resolve) => {
      attach = () => resolve({ id: 'mount-1', state: { kind: 'state', threadId: THREAD, seq: 2, activePageId: 'n', visible: true, pages: ready } });
    }));
    const view = render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD, paneId: 'main' }) });
    await openBrowserTab(THREAD, { focusAddress: true });
    const address = view.getByRole('textbox', { name: 'Address' }) as HTMLInputElement;
    await tick();
    expect(document.activeElement).not.toBe(address);
    attach();
    await waitFor(() => expect(document.activeElement).toBe(address));
  });

  it('leaves focus alone when the user acted before the new tab was ready', async () => {
    let answer!: () => void;
    setBindingMock('BrowserCompanionDo', vi.fn(async () => {
      await new Promise<void>((resolve) => { answer = resolve; });
      return { kind: 'state', threadId: THREAD, seq: 2, activePageId: 'n', visible: true, pages: [page('a', false), page('n', false)], pageId: 'n' };
    }));
    const view = mount([page('a', false)]);
    const address = view.getByRole('textbox', { name: 'Address' }) as HTMLInputElement;
    await waitFor(() => expect(address.disabled).toBe(false));
    await fireEvent.click(view.getByRole('button', { name: 'New tab' }));
    window.dispatchEvent(new Event('pointerdown'));
    answer();
    await waitFor(() => expect(view.getAllByTestId('browser-tab')).toHaveLength(2));
    await tick();
    await tick();
    expect(document.activeElement).not.toBe(address);
  });

  it('puts a teardown failure in the open pane\'s banner, not a toast', async () => {
    setBindingMock('BrowserCompanionDo', async () => undefined);
    const view = mount([page('a', false)]);
    await view.findByText('Page a');
    applyBrowserCompanionState({ kind: 'teardown', threadId: THREAD, pageId: 'b', error: 'profile would not stop' } as never);
    expect((await view.findByRole('alert')).textContent).toContain('profile would not stop');
    expect(getToasts()).toEqual([]);
  });
});
