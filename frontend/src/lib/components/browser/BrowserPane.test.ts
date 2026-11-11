import { tick } from 'svelte';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import BrowserPane from './BrowserPane.svelte';
import { makeStubPanelContext } from '../../../test/helpers/panelContext';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';
import { applyBrowserCompanionState, resetBrowserCompanionForTest } from '../../stores/browserCompanion.svelte';
import { BROWSER_FIND_DEBOUNCE_MS, resetBrowserFindForTest } from '../../stores/browserFind.svelte';
import { hasPaneFindProvider, isPaneFindOpen, openPaneFind, resetPaneFindForTest } from '../../stores/paneFind.svelte';
import { installCompanionPanes, openCompanion, resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { createPane, resetPanesForTest } from '../../stores/panes.svelte';

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
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'activate', pageId: 'a' });
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

  // A bare Escape pressed in the page is the page's; the backend announces
  // it, and the pane closes its open bar for the page it shows.
  it('closes its open bar on an Escape announced for its shown page, and only then', async () => {
    setBindingMock('BrowserCompanionFind', vi.fn(async (_thread: string, action: { kind: string; query?: string; mount: string; seq: number }) => ({
      pageId: 'a', incarnation: 1, mount: action.mount, seq: action.seq, document: 1, session: 1, active: action.kind !== 'stop', query: action.query ?? '', found: true, matches: 2, current: 0, capped: false,
    })));
    const view = mount('bar');
    await waitFor(() => expect(hasPaneFindProvider('browser-main')).toBe(true));
    const escape = (pageId: string) => applyBrowserCompanionState({ kind: 'escape', threadId: THREAD, pageId } as never);
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
