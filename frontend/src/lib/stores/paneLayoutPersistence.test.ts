import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Thread } from '../types/models';
import type { PaneLayoutPersistedSettings, Settings } from '../types/settings';
import {
  flushPaneLayoutPersistence,
  getPaneLayoutItems,
  resetPaneLayoutForTest,
  resizeAdjacentPaneLayoutItems,
  setPaneLayoutItemsForTest,
} from './paneLayout.svelte';
import {
  createPane,
  focusPane,
  getAllPanes,
  getFocusedPaneId,
  resetPanesForTest,
} from './panes.svelte';
import {
  installPaneLayoutPersistence,
  loadFromSettings,
  persistToSettings,
  resetPaneLayoutPersistenceForTest,
  waitForPaneLayoutPersistenceForTest,
} from './paneLayoutPersistence';
import { installPaneMocks, makeItem } from '../../test/helpers/chat';
import { makeSettings } from '../../test/helpers/settings';
import { setBindingMock } from '../../test/mocks/bindings-app';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function seedPane(paneId: string, thread: Thread): void {
  const pane = createPane(paneId);
  pane.replaceThread(thread);
}

function makeThreadThatThrowsDuringSwitch(threadId: string): Thread {
  const thread = makeThread({ id: threadId });
  let idReads = 0;
  Object.defineProperty(thread, 'id', {
    configurable: true,
    get: () => {
      idReads++;
      if (idReads === 1) return threadId;
      throw new Error('switch failed');
    },
  });
  return thread;
}

function makeSavedLayout(
  panes: Array<{ paneId: string; threadId: string; ratio: number }>,
  focusedPaneId: string | null,
): PaneLayoutPersistedSettings {
  return { version: 1, panes, focusedPaneId };
}

function installSettingsMock(initialPaneLayout: unknown = makeSavedLayout([], null)) {
  let settings = makeSettings({
    paneLayout: initialPaneLayout as PaneLayoutPersistedSettings,
  });
  const updateSettings = vi.fn(async (patch: Partial<Settings>) => {
    settings = makeSettings({
      ...settings,
      ...patch,
    });
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

describe('pane layout persistence', () => {
  beforeEach(() => {
    localStorage.removeItem('agentOverflowPaneLayout');
    resetPanesForTest();
    resetPaneLayoutForTest();
    installPaneLayoutPersistence();
    installSettingsMock();
  });

  afterEach(() => {
    resetPaneLayoutPersistenceForTest();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('round-trips panes and the focused pane id through settings', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left' });
    const right = makeThread({ id: 'right-thread', title: 'Right' });
    const settings = installSettingsMock();
    installPaneMocks();
    seedPane('left', left);
    seedPane('right', right);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 0.75 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1.25 },
    ]);
    focusPane('right');

    persistToSettings();
    await waitForPaneLayoutPersistenceForTest();
    resetPanesForTest();
    setPaneLayoutItemsForTest([]);

    await loadFromSettings([left, right]);

    expect(settings.updateSettings).toHaveBeenCalledWith({
      paneLayout: makeSavedLayout([
        { paneId: 'left', threadId: 'left-thread', ratio: 0.75 },
        { paneId: 'right', threadId: 'right-thread', ratio: 1.25 },
      ], 'right'),
    });
    expect(getPaneLayoutItems()).toEqual([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 0.75 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1.25 },
    ]);
    expect(getAllPanes().get('left')?.threadId).toBe('left-thread');
    expect(getAllPanes().get('right')?.threadId).toBe('right-thread');
    expect(getFocusedPaneId()).toBe('right');
  });

  it('hydrates restored panes through the normal switchThread path', async () => {
    const thread = makeThread({ id: 'restored-thread' });
    const restoredItem = makeItem({ id: 'restored-item', threadId: thread.id, summary: 'Loaded history' });
    installPaneMocks([restoredItem]);
    installSettingsMock(makeSavedLayout([
      { paneId: 'main', threadId: thread.id, ratio: 1 },
    ], 'main'));

    await loadFromSettings([thread]);

    expect(getAllPanes().get('main')?.items.map((item) => item.id)).toContain('restored-item');
  });

  it('coalesces multiple resize persistence requests into one trailing settings write', async () => {
    vi.useFakeTimers();
    const settings = installSettingsMock();
    const left = makeThread({ id: 'left-thread' });
    const right = makeThread({ id: 'right-thread' });
    seedPane('left', left);
    seedPane('right', right);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);
    await waitForPaneLayoutPersistenceForTest();
    settings.updateSettings.mockClear();

    resizeAdjacentPaneLayoutItems('left', 'right', 800, 800, 100, 560);
    resizeAdjacentPaneLayoutItems('left', 'right', 900, 700, -100, 560);
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(199);
    expect(settings.updateSettings).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(1);
    await waitForPaneLayoutPersistenceForTest();

    expect(settings.updateSettings).toHaveBeenCalledTimes(1);
    expect(settings.paneLayout.panes.map((pane) => pane.threadId)).toEqual(['left-thread', 'right-thread']);
  });

  it('flushes a pending resize persistence write immediately', async () => {
    vi.useFakeTimers();
    const settings = installSettingsMock();
    const left = makeThread({ id: 'left-thread' });
    const right = makeThread({ id: 'right-thread' });
    seedPane('left', left);
    seedPane('right', right);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);
    await waitForPaneLayoutPersistenceForTest();
    settings.updateSettings.mockClear();

    resizeAdjacentPaneLayoutItems('left', 'right', 800, 800, 120, 560);
    await vi.advanceTimersByTimeAsync(0);
    expect(settings.updateSettings).not.toHaveBeenCalled();

    await flushPaneLayoutPersistence();

    expect(settings.updateSettings).toHaveBeenCalledTimes(1);
    expect(settings.paneLayout.panes.map((pane) => pane.threadId)).toEqual(['left-thread', 'right-thread']);
    await vi.advanceTimersByTimeAsync(200);
    expect(settings.updateSettings).toHaveBeenCalledTimes(1);
  });

  it('drops saved panes whose threads are no longer returned by ListThreads', async () => {
    installSettingsMock(makeSavedLayout([
      { paneId: 'left', threadId: 'kept-thread', ratio: 1 },
      { paneId: 'right', threadId: 'deleted-thread', ratio: 1 },
    ], 'right'));

    installPaneMocks();
    await loadFromSettings([makeThread({ id: 'kept-thread' })]);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left']);
    expect(getAllPanes().get('left')?.threadId).toBe('kept-thread');
    expect(getFocusedPaneId()).toBe('left');
  });

  it('migrates a legacy localStorage layout when settings are still empty', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left' });
    const settings = installSettingsMock(makeSavedLayout([], null));
    localStorage.setItem('agentOverflowPaneLayout', JSON.stringify(makeSavedLayout([
      { paneId: 'left', threadId: left.id, ratio: 1.5 },
    ], 'left')));
    installPaneMocks();

    await loadFromSettings([left]);
    await waitForPaneLayoutPersistenceForTest();

    expect(getPaneLayoutItems()).toEqual([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 1.5 },
    ]);
    expect(settings.paneLayout).toEqual(makeSavedLayout([
      { paneId: 'left', threadId: left.id, ratio: 1.5 },
    ], 'left'));
    expect(localStorage.getItem('agentOverflowPaneLayout')).toBeNull();
  });

  it('continues restoring other panes when one pane hydration fails', async () => {
    const good = makeThread({ id: 'good-thread' });
    const badThreadId = 'bad-thread';
    const bad = makeThreadThatThrowsDuringSwitch(badThreadId);
    installSettingsMock(makeSavedLayout([
      { paneId: 'good', threadId: good.id, ratio: 1 },
      { paneId: 'bad', threadId: badThreadId, ratio: 1 },
    ], 'bad'));
    installPaneMocks();
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});

    await expect(loadFromSettings([good, bad])).resolves.toBeUndefined();

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['good']);
    expect(getAllPanes().get('good')?.threadId).toBe(good.id);
    expect(getAllPanes().has('bad')).toBe(false);
    expect(getFocusedPaneId()).toBe('good');
    expect(consoleError).toHaveBeenCalledWith(
      expect.stringContaining('Failed to restore pane "bad"'),
      expect.any(Error),
    );
  });

  it('surfaces GetSettings failures to the App startup error path', async () => {
    setBindingMock('GetSettings', async () => {
      throw new Error('settings unavailable');
    });

    await expect(loadFromSettings([makeThread({ id: 'left-thread' })])).rejects.toThrow('settings unavailable');
  });

  it('ignores unsafe persisted pane ids and caps corrupt layouts', async () => {
    const panes = Array.from({ length: 30 }, (_, index) => ({
      paneId: `pane-${index + 1}`,
      threadId: `thread-${index + 1}`,
      ratio: 1,
    }));
    installSettingsMock(makeSavedLayout([
      { paneId: 'bad"] [data-pane-id="x', threadId: 'bad-thread', ratio: 1 },
      ...panes,
    ], 'bad"] [data-pane-id="x'));
    installPaneMocks();

    await loadFromSettings(
      panes.map((pane) => makeThread({ id: pane.threadId })),
    );

    expect(getPaneLayoutItems()).toHaveLength(24);
    expect(getPaneLayoutItems().map((item) => item.paneId)).not.toContain('bad"] [data-pane-id="x');
    expect(getFocusedPaneId()).toBe('pane-1');
  });

  it('falls back to an empty layout for missing, malformed, and mismatched persisted settings', async () => {
    const cases: unknown[] = [
      null,
      { version: 2, panes: [], focusedPaneId: null },
      { version: 1, panes: 'bad', focusedPaneId: null },
    ];
    for (const paneLayout of cases) {
      resetPanesForTest();
      resetPaneLayoutForTest();
      seedPane('left', makeThread({ id: 'left-thread' }));
      setPaneLayoutItemsForTest([{ id: 'left', paneId: 'left', kind: 'thread', ratio: 1 }]);
      installSettingsMock(paneLayout);
      installPaneMocks();

      await loadFromSettings([makeThread({ id: 'left-thread' })]);

      expect(getPaneLayoutItems()).toEqual([]);
      expect(getAllPanes().size).toBe(0);
      expect(getFocusedPaneId()).toBeNull();
    }
  });

  it('persists an explicit empty pane layout when saving an empty layout', async () => {
    const settings = installSettingsMock(makeSavedLayout([
      { paneId: 'left', threadId: 'thread-1', ratio: 1 },
    ], 'left'));
    setPaneLayoutItemsForTest([]);

    persistToSettings();
    await waitForPaneLayoutPersistenceForTest();

    expect(settings.paneLayout).toEqual(makeSavedLayout([], null));
  });

  it('treats settings write failures as best-effort', async () => {
    seedPane('left', makeThread({ id: 'left-thread' }));
    setPaneLayoutItemsForTest([{ id: 'left', paneId: 'left', kind: 'thread', ratio: 1 }]);
    setBindingMock('UpdateSettings', async () => {
      throw new Error('settings write failed');
    });
    const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(() => persistToSettings()).not.toThrow();
    await waitForPaneLayoutPersistenceForTest();
    expect(consoleWarn).toHaveBeenCalledWith(
      'Failed to write pane layout persistence:',
      expect.any(Error),
    );
  });
});
