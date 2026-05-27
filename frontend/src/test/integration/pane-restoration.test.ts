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
  resizeAdjacentPaneLayoutItems,
  setPaneLayoutItemsForTest,
} from '../../lib/stores/paneLayout.svelte';
import type { Thread } from '../../lib/types/models';
import type { PaneLayoutPersistedSettings, Settings } from '../../lib/types/settings';
import { makeSettings } from '../helpers/settings';

beforeAll(installAnimateShim);

function savePaneLayout(
  panes: Array<{ paneId: string; threadId: string; ratio: number }>,
  focusedPaneId: string | null,
): PaneLayoutPersistedSettings {
  return {
    version: 1,
    panes,
    focusedPaneId,
  };
}

function installSettingsWithPaneLayout(initialPaneLayout: unknown) {
  let settings = makeSettings({
    paneLayout: initialPaneLayout as PaneLayoutPersistedSettings,
  });
  const updateSettings = vi.fn(async (patch: Partial<Settings>) => {
    settings = makeSettings({ ...settings, ...patch });
    return settings;
  });
  setBindingMock('GetSettings', async () => settings);
  setBindingMock('UpdateSettings', updateSettings);
  return {
    get paneLayout(): PaneLayoutPersistedSettings {
      return settings.paneLayout;
    },
    updateSettings,
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
    installSettingsWithPaneLayout(savePaneLayout([
      { paneId: 'left', threadId: left.id, ratio: 0.75 },
      { paneId: 'right', threadId: right.id, ratio: 1.25 },
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
    expect(paneSections.map((section) => section.dataset.paneRatio)).toEqual(['0.75', '1.25']);
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
    const settings = installSettingsWithPaneLayout(savePaneLayout([
      { paneId: 'left', threadId: left.id, ratio: 1 },
    ], 'left'));
    const savedLayout = settings.paneLayout;

    const rendered = render(App);
    await flush();

    window.dispatchEvent(new Event('pagehide'));
    expect(settings.updateSettings).not.toHaveBeenCalled();
    expect(settings.paneLayout).toBe(savedLayout);

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
    const settings = installSettingsWithPaneLayout(savePaneLayout([
      { paneId: 'left', threadId: left.id, ratio: 1 },
    ], 'left'));
    const savedLayout = settings.paneLayout;
    vi.spyOn(console, 'error').mockImplementation(() => {});

    const rendered = render(App);

    await waitFor(() => expect(rendered.getByTestId('pane-host-empty')).toBeInTheDocument());
    window.dispatchEvent(new Event('pagehide'));

    expect(settings.updateSettings).not.toHaveBeenCalled();
    expect(settings.paneLayout).toBe(savedLayout);
  });

  it('does not rewrite pane layout on pagehide when there is no pending layout write', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left Thread' });
    installThreadMocks([left]);
    const settings = installSettingsWithPaneLayout(savePaneLayout([
      { paneId: 'left', threadId: left.id, ratio: 1 },
    ], 'left'));

    const rendered = render(App);
    await waitFor(() => expect(rendered.getByTestId('pane-host')).toBeInTheDocument());
    window.dispatchEvent(new Event('pagehide'));

    expect(settings.updateSettings).not.toHaveBeenCalled();
  });

  it('drops saved panes whose threads are no longer available', async () => {
    const kept = makeThread({ id: 'kept-thread', title: 'Kept Thread' });
    installThreadMocks([kept]);
    installSettingsWithPaneLayout(savePaneLayout([
      { paneId: 'kept', threadId: kept.id, ratio: 1.5 },
      { paneId: 'deleted', threadId: 'deleted-thread', ratio: 0.5 },
    ], 'deleted'));

    const rendered = render(App);

    await waitFor(() => expect(rendered.getByTestId('pane-host')).toBeInTheDocument());
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['kept']);
    expect(getFocusedPaneId()).toBe('kept');
    expect(rendered.container.querySelector('[data-pane-id="deleted"]')).toBeNull();
  });

  it('renders the empty pane state when no saved panes are valid', async () => {
    installThreadMocks([]);
    installSettingsWithPaneLayout(savePaneLayout([
      { paneId: 'deleted', threadId: 'deleted-thread', ratio: 1 },
    ], 'deleted'));

    const rendered = render(App);

    await waitFor(() => expect(rendered.getByTestId('pane-host-empty')).toBeInTheDocument());
    expect(getPaneLayoutItems()).toEqual([]);
    expect(getFocusedPaneId()).toBeNull();
  });

  it('flushes pending pane layout persistence on pagehide', async () => {
    installThreadMocks([]);
    const settings = installSettingsWithPaneLayout(savePaneLayout([], null));
    const left = makeThread({ id: 'left-thread', title: 'Left Thread' });
    const right = makeThread({ id: 'right-thread', title: 'Right Thread' });

    const rendered = render(App);
    await waitFor(() => expect(rendered.getByTestId('pane-host-empty')).toBeInTheDocument());

    await createPane('left').replaceThread(left);
    await createPane('right').replaceThread(right);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);
    await waitFor(() => expect(settings.updateSettings).toHaveBeenCalled());
    settings.updateSettings.mockClear();

    resizeAdjacentPaneLayoutItems('left', 'right', 800, 800, 120, 560);
    expect(settings.updateSettings).not.toHaveBeenCalled();

    window.dispatchEvent(new Event('pagehide'));

    await waitFor(() => expect(settings.updateSettings).toHaveBeenCalledTimes(1));
    expect(settings.paneLayout.panes.map((pane) => pane.threadId)).toEqual([left.id, right.id]);
  });
});
