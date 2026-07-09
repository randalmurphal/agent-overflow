import { describe, expect, it, beforeAll, beforeEach, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installComposerDefaults,
  installThreadViewDefaults,
  makeThread,
  resetAppState,
  seedSidebarProject,
} from './_helpers';
import { setBindingMock } from '../mocks/bindings-app';
import { createPane, getFocusedPaneId } from '../../lib/stores/panes.svelte';
import {
  getPaneLayoutItems,
  applyPaneBoundaryDrag,
  setPaneLayoutItemsForTest,
} from '../../lib/stores/paneLayout.svelte';
import type { Thread } from '../../lib/types/models';
import type { PaneLayoutPersistedSettings } from '../../lib/types/settings';

beforeAll(installAnimateShim);

function savePaneLayout(
  panes: Array<{ paneId: string; threadId: string; widthPx: number }>,
  focusedPaneId: string | null,
): PaneLayoutPersistedSettings {
  return {
    version: 3,
    panes: panes.map((pane) => ({ ...pane, kind: 'thread' })),
    focusedPaneId,
  };
}

/**
 * Stateful appStorage backend mock: the server bucket starts with the
 * given persisted layout and absorbs SetUIState / DeleteUIState writes,
 * so tests can assert on what actually landed durably.
 */
function installUIStateWithPaneLayout(initialPaneLayout: unknown) {
  const entries: Record<string, string> = {
    paneLayout: JSON.stringify(initialPaneLayout),
  };
  const setUIState = vi.fn(async (_clientId: string, patch: Record<string, string>) => {
    Object.assign(entries, patch);
    return null;
  });
  setBindingMock('GetUIState', async () => ({ ...entries }));
  setBindingMock('SetUIState', setUIState);
  setBindingMock('DeleteUIState', async (_clientId: string, keys: string[]) => {
    for (const key of keys) delete entries[key];
    return null;
  });
  return {
    get paneLayout(): PaneLayoutPersistedSettings {
      return JSON.parse(entries.paneLayout) as PaneLayoutPersistedSettings;
    },
    setUIState,
  };
}

function installThreadMocks(threads: Thread[]): void {
  installAppDefaults();
  setBindingMock('ListThreads', async () => threads);
  seedSidebarProject(threads);
  installThreadViewDefaults();
  for (const thread of threads) installComposerDefaults(thread.id);
}

describe('App integration - pane restoration', () => {
  beforeEach(() => {
    resetAppState();
  });

  it('waits for saved panes to validate and switch before mounting PaneHost', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left Thread' });
    const right = makeThread({ id: 'right-thread', title: 'Right Thread' });
    let resolveThreads: (threads: Thread[]) => void = () => {};
    const threadsPromise = new Promise<Thread[]>((resolve) => {
      resolveThreads = resolve;
    });

    installAppDefaults();
    setBindingMock('ListThreads', async () => threadsPromise);
    seedSidebarProject([left, right]);
    installThreadViewDefaults();
    installComposerDefaults(left.id);
    installComposerDefaults(right.id);
    installUIStateWithPaneLayout(savePaneLayout([
      { paneId: 'left', threadId: left.id, widthPx: 660 },
      { paneId: 'right', threadId: right.id, widthPx: 1100 },
    ], 'right'));

    const rendered = render(App);
    await flush();

    expect(rendered.queryByTestId('pane-host')).toBeNull();
    expect(rendered.queryByTestId('pane-host-empty')).toBeNull();

    resolveThreads([left, right]);

    await waitFor(() => expect(rendered.getByTestId('pane-host')).toBeInTheDocument());
    const paneSections = Array.from(
      rendered.container.querySelectorAll<HTMLElement>('[data-pane-kind="thread"]'),
    );
    expect(paneSections.map((section) => section.dataset.paneId)).toEqual(['left', 'right']);
    expect(paneSections.map((section) => section.dataset.paneWidth)).toEqual(['660', '1100']);
    expect(getFocusedPaneId()).toBe('right');
  });

  it('preserves the saved pane layout if the app closes before startup restore finishes', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left Thread' });
    let resolveThreads: (threads: Thread[]) => void = () => {};
    const threadsPromise = new Promise<Thread[]>((resolve) => {
      resolveThreads = resolve;
    });

    installAppDefaults();
    setBindingMock('ListThreads', async () => threadsPromise);
    seedSidebarProject([left]);
    installThreadViewDefaults();
    installComposerDefaults(left.id);
    const uiState = installUIStateWithPaneLayout(savePaneLayout([
      { paneId: 'left', threadId: left.id, widthPx: 1 },
    ], 'left'));
    const savedLayout = uiState.paneLayout;

    const rendered = render(App);
    await flush();

    window.dispatchEvent(new Event('pagehide'));
    expect(uiState.setUIState).not.toHaveBeenCalled();
    expect(uiState.paneLayout).toEqual(savedLayout);

    resolveThreads([left]);
    await waitFor(() => expect(rendered.getByTestId('pane-host')).toBeInTheDocument());
  });

  it('does not treat a startup thread load failure as an empty saved layout', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left Thread' });
    installAppDefaults();
    setBindingMock('ListThreads', async () => {
      throw new Error('thread load failed');
    });
    seedSidebarProject([left]);
    installThreadViewDefaults();
    installComposerDefaults(left.id);
    const uiState = installUIStateWithPaneLayout(savePaneLayout([
      { paneId: 'left', threadId: left.id, widthPx: 1 },
    ], 'left'));
    const savedLayout = uiState.paneLayout;
    vi.spyOn(console, 'error').mockImplementation(() => {});

    const rendered = render(App);

    await waitFor(() => expect(rendered.getByTestId('pane-host-empty')).toBeInTheDocument());
    window.dispatchEvent(new Event('pagehide'));

    expect(uiState.setUIState).not.toHaveBeenCalled();
    expect(uiState.paneLayout).toEqual(savedLayout);
  });

  it('does not rewrite pane layout on pagehide when there is no pending layout write', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left Thread' });
    installThreadMocks([left]);
    const uiState = installUIStateWithPaneLayout(savePaneLayout([
      { paneId: 'left', threadId: left.id, widthPx: 1 },
    ], 'left'));

    const rendered = render(App);
    await waitFor(() => expect(rendered.getByTestId('pane-host')).toBeInTheDocument());
    window.dispatchEvent(new Event('pagehide'));

    expect(uiState.setUIState).not.toHaveBeenCalled();
  });

  it('drops saved panes whose threads are no longer available', async () => {
    const kept = makeThread({ id: 'kept-thread', title: 'Kept Thread' });
    installThreadMocks([kept]);
    installUIStateWithPaneLayout(savePaneLayout([
      { paneId: 'kept', threadId: kept.id, widthPx: 840 },
      { paneId: 'deleted', threadId: 'deleted-thread', widthPx: 600 },
    ], 'deleted'));

    const rendered = render(App);

    await waitFor(() => expect(rendered.getByTestId('pane-host')).toBeInTheDocument());
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['kept']);
    expect(getFocusedPaneId()).toBe('kept');
    expect(rendered.container.querySelector('[data-pane-id="deleted"]')).toBeNull();
  });

  it('renders the empty pane state when no saved panes are valid', async () => {
    installThreadMocks([]);
    installUIStateWithPaneLayout(savePaneLayout([
      { paneId: 'deleted', threadId: 'deleted-thread', widthPx: 1 },
    ], 'deleted'));

    const rendered = render(App);

    await waitFor(() => expect(rendered.getByTestId('pane-host-empty')).toBeInTheDocument());
    expect(getPaneLayoutItems()).toEqual([]);
    expect(getFocusedPaneId()).toBeNull();
  });

  it('flushes pending pane layout persistence on pagehide', async () => {
    installThreadMocks([]);
    const uiState = installUIStateWithPaneLayout(savePaneLayout([], null));
    const left = makeThread({ id: 'left-thread', title: 'Left Thread' });
    const right = makeThread({ id: 'right-thread', title: 'Right Thread' });

    const rendered = render(App);
    await waitFor(() => expect(rendered.getByTestId('pane-host-empty')).toBeInTheDocument());

    await createPane('left').replaceThread(left);
    await createPane('right').replaceThread(right);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
    ]);
    await waitFor(() => expect(uiState.setUIState).toHaveBeenCalled());
    uiState.setUIState.mockClear();

    applyPaneBoundaryDrag({
      leftPaneId: 'left',
      rightPaneId: 'right',
      startWidths: new Map([['left', 800], ['right', 800]]),
      deltaPx: 120,
      minPaneWidth: 560,
      overflowPx: 0,
      zeroSum: true,
    });
    expect(uiState.setUIState).not.toHaveBeenCalled();

    window.dispatchEvent(new Event('pagehide'));

    await waitFor(() => expect(uiState.setUIState).toHaveBeenCalledTimes(1));
    expect(uiState.paneLayout.panes.map((pane) => pane.threadId)).toEqual([left.id, right.id]);
  });
});
