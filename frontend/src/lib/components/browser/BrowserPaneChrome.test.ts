import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import BrowserPane from './BrowserPane.svelte';
import { makeStubPanelContext } from '../../../test/helpers/panelContext';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';
import { focusBrowserAddressBar, resetBrowserCompanionForTest } from '../../stores/browserCompanion.svelte';
import { installCompanionPanes, openCompanion, resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { getPromotedPaneId, resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { createPane, resetPanesForTest } from '../../stores/panes.svelte';
import { resetToastsForTest } from '../../stores/toast.svelte';
import { BROWSER_FILE_SEARCH_DEBOUNCE_MS } from './browserFileSearch.svelte';

const THREAD = 'thread-1';

function page(id: string, extra: Record<string, unknown> = {}) {
  return { id, url: `https://example.test/${id}`, title: `Page ${id}`, canGoBack: false, canGoForward: false, ...extra };
}

function state(pages: Record<string, unknown>[]) {
  return { kind: 'state', threadId: THREAD, activePageId: String(pages[0]?.id ?? ''), visible: true, pages };
}

function listing(path: string) {
  return {
    path: path === '~' ? '/home/me' : path,
    parent: '/',
    separator: '/',
    entries: [
      { name: 'docs', isDir: true, hidden: false, isRepo: false },
      { name: 'report.html', isDir: false, hidden: false, isRepo: false },
    ],
    truncated: false,
    exists: true,
  };
}

function resetAll(): void {
  resetBrowserCompanionForTest();
  resetToastsForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  resetPanesForTest();
}

describe('<BrowserPane> chrome', () => {
  let browse: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    resetAll();
    installCompanionPanes();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
    createPane('main').replaceThread(makeThread({ id: THREAD }));
    openCompanion('main', 'browser');
    setBindingMock('BrowserCompanionPaneDetach', async () => undefined);
    setBindingMock('BrowserCompanionPaneRect', async () => undefined);
    browse = setBindingMock('BrowseDirectory', vi.fn(async (path: string) => listing(path)));
  });
  afterEach(() => {
    vi.useRealTimers();
    resetAll();
  });

  function mount(pages: Record<string, unknown>[]) {
    setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: 'mount-1', state: state(pages) }));
    return render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD, paneId: 'main', workspacePath: '/repo' }) });
  }

  async function ready(view: { container: HTMLElement }): Promise<HTMLInputElement> {
    const address = view.container.querySelector<HTMLInputElement>('input[aria-label="Address"]')!;
    await waitFor(() => expect(address.disabled).toBe(false));
    return address;
  }

  it('opens a file picked in the folder view, rooted at the workspace, in the active tab', async () => {
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => state([page('a')])));
    const view = mount([page('a')]);
    await ready(view);
    await fireEvent.click(view.getByRole('button', { name: 'Open file' }));
    const entry = await view.findByText('report.html');
    expect(browse).toHaveBeenCalledWith('/repo');
    await fireEvent.click(entry);
    await waitFor(() => expect(act).toHaveBeenCalledWith(THREAD, expect.objectContaining({ kind: 'navigate', pageId: 'a', address: '/repo/report.html' })));
    expect(view.queryByTestId('browser-file-picker')).toBeNull();
  });

  it('opens a file found by the workspace search, and shows the browser\'s refusal', async () => {
    const search = setBindingMock('SearchWorkspaceFiles', vi.fn(async () => ({
      root: '/repo',
      truncated: false,
      files: [
        { path: 'docs', kind: 'directory' },
        { path: 'docs/guide.html', kind: 'file' },
        { path: 'site/index.html', kind: 'file' },
      ],
    })));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async (_thread: string, action: { kind: string }) => {
      if (action.kind === 'navigate') throw new Error('browser: file is outside the workspace');
      return state([page('a')]);
    }));
    const view = mount([page('a')]);
    await ready(view);
    await fireEvent.click(view.getByRole('button', { name: 'Open file' }));
    const field = await view.findByTestId('browser-file-search');
    await fireEvent.input(field, { target: { value: 'html' } });
    await vi.advanceTimersByTimeAsync(BROWSER_FILE_SEARCH_DEBOUNCE_MS);
    await waitFor(() => expect(view.getAllByTestId('browser-file-result')).toHaveLength(2));
    expect(search).toHaveBeenCalledWith({ projectId: 'project-1', workspacePath: '/repo' }, 'html', 50);
    await fireEvent.keyDown(field, { key: 'ArrowDown' });
    await fireEvent.keyDown(field, { key: 'Enter' });
    await waitFor(() => expect(act).toHaveBeenCalledWith(THREAD, expect.objectContaining({ kind: 'navigate', pageId: 'a', address: '/repo/site/index.html' })));
    expect((await view.findByRole('alert')).textContent).toContain('outside the workspace');
  });

  it('offers only the results of the query on screen while a new one is pending', async () => {
    let releaseCss: (() => void) | null = null;
    setBindingMock('SearchWorkspaceFiles', vi.fn(async (_ws: unknown, text: string) => {
      if (text === 'css') await new Promise<void>((resolve) => (releaseCss = resolve));
      return { root: '/repo', truncated: false, files: [{ path: `old/${text}.file`, kind: 'file' }] };
    }));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => state([page('a')])));
    const view = mount([page('a')]);
    await ready(view);
    await fireEvent.click(view.getByRole('button', { name: 'Open file' }));
    const field = await view.findByTestId('browser-file-search');
    await fireEvent.input(field, { target: { value: 'html' } });
    await vi.advanceTimersByTimeAsync(BROWSER_FILE_SEARCH_DEBOUNCE_MS);
    await waitFor(() => expect(view.getAllByTestId('browser-file-result')).toHaveLength(1));

    // The query changes; its search is still running.
    await fireEvent.input(field, { target: { value: 'css' } });
    expect(view.queryAllByTestId('browser-file-result')).toHaveLength(0);
    await fireEvent.keyDown(field, { key: 'Enter' });
    await vi.advanceTimersByTimeAsync(BROWSER_FILE_SEARCH_DEBOUNCE_MS);
    expect(view.queryAllByTestId('browser-file-result')).toHaveLength(0);
    await fireEvent.keyDown(field, { key: 'Enter' });
    expect(act).not.toHaveBeenCalledWith(THREAD, expect.objectContaining({ kind: 'navigate' }));

    releaseCss!();
    await waitFor(() => expect(view.getByTestId('browser-file-result').textContent).toContain('old/css.file'));
    await fireEvent.keyDown(field, { key: 'Enter' });
    await waitFor(() => expect(act).toHaveBeenCalledWith(THREAD, expect.objectContaining({ kind: 'navigate', address: '/repo/old/css.file' })));
    expect(act).not.toHaveBeenCalledWith(THREAD, expect.objectContaining({ address: '/repo/old/html.file' }));
  });

  it('returns focus to More after an action launched from its menu, the file picker included', async () => {
    setBindingMock('BrowserCompanionDo', vi.fn(async () => state([page('a', { url: 'file:///repo/x.html' })])));
    setBindingMock('BrowserCompanionRevealPageFile', vi.fn(async () => undefined));
    const view = mount([page('a', { url: 'file:///repo/x.html' })]);
    await ready(view);
    const more = view.getByTestId('browser-more');
    const choose = async (label: string) => {
      more.focus();
      await fireEvent.click(more);
      const item = (await view.findAllByRole('menuitem')).find((el) => el.textContent?.includes(label))!;
      item.focus();
      await fireEvent.click(item);
    };

    await choose('Open file');
    const field = await view.findByTestId('browser-file-search');
    await waitFor(() => expect(document.activeElement).toBe(field));
    await fireEvent.keyDown(field, { key: 'Escape' });
    await waitFor(() => expect(view.queryByTestId('browser-file-picker')).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(more));

    for (const label of ['Show in folder', 'Open devtools']) {
      await choose(label);
      await waitFor(() => expect(view.queryAllByRole('menuitem')).toHaveLength(0));
      expect(document.activeElement, label).toBe(more);
    }
  });

  it('shows a failed search in the picker', async () => {
    setBindingMock('SearchWorkspaceFiles', vi.fn(async () => {
      throw new Error('workspace is gone');
    }));
    setBindingMock('BrowserCompanionDo', async () => undefined);
    const view = mount([page('a')]);
    await ready(view);
    await fireEvent.click(view.getByRole('button', { name: 'Open file' }));
    await fireEvent.input(await view.findByTestId('browser-file-search'), { target: { value: 'x' } });
    await vi.advanceTimersByTimeAsync(BROWSER_FILE_SEARCH_DEBOUNCE_MS);
    expect((await view.findByTestId('browser-file-search-error')).textContent).toContain('workspace is gone');
  });

  it('opens a picked file in a new tab when the active tab cannot take an address', async () => {
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => state([page('a')])));
    const view = mount([page('a', { error: 'profile is locked' })]);
    await view.findByText('Page a');
    await fireEvent.click(view.getByRole('button', { name: 'Open file' }));
    await fireEvent.click(await view.findByText('report.html'));
    await waitFor(() => expect(act).toHaveBeenCalledWith(THREAD, expect.objectContaining({ kind: 'new', address: '/repo/report.html' })));
  });

  it('a double-click on the empty tab strip maximizes the browser pane and restores it', async () => {
    setBindingMock('BrowserCompanionDo', async () => undefined);
    const view = mount([page('a')]);
    await view.findByText('Page a');
    await fireEvent.dblClick(view.getByTestId('browser-tab-strip-space'));
    expect(getPromotedPaneId()).toBe('browser-main');
    await fireEvent.dblClick(view.getByTestId('browser-tab-strip-space'));
    expect(getPromotedPaneId()).toBeNull();
  });

  it('Escape in the address hands focus to the page area', async () => {
    setBindingMock('BrowserCompanionDo', async () => undefined);
    const view = mount([page('a')]);
    const address = await ready(view);
    address.focus();
    await fireEvent.keyDown(address, { key: 'Escape' });
    expect(document.activeElement).toBe(view.getByTestId('browser-pane-host-rect'));
  });

  it.each([
    ['Escape', 'https://example.test/a'],
    ['Enter', 'https://example.test/typed'],
  ])('%s in the address asks the engine to give the page the keyboard', async (key, typed) => {
    const actions: string[] = [];
    setBindingMock('BrowserCompanionDo', async (_thread: string, action: { kind: string }) => {
      actions.push(action.kind);
      return undefined;
    });
    const view = mount([page('a')]);
    const address = await ready(view);
    address.focus();
    await fireEvent.input(address, { target: { value: typed } });
    actions.length = 0;
    await fireEvent.keyDown(address, { key });
    await waitFor(() => expect(actions).toContain('focus-page'));
    expect(actions.includes('navigate')).toBe(key === 'Enter');
  });

  it('mod+l focuses its address with the text selected, until the pane unmounts', async () => {
    setBindingMock('BrowserCompanionDo', async () => undefined);
    const view = mount([page('a')]);
    const address = await ready(view);
    expect(focusBrowserAddressBar(THREAD)).toBe(true);
    expect(document.activeElement).toBe(address);
    expect([address.selectionStart, address.selectionEnd]).toEqual([0, address.value.length]);
    view.unmount();
    expect(focusBrowserAddressBar(THREAD)).toBe(false);
  });
});
