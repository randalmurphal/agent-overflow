import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Thread } from '../types/models';
import type { PaneLayoutPersistedSettings } from '../types/settings';
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
  loadPersistedPaneLayout,
  persistPaneLayout,
  resetPaneLayoutPersistenceForTest,
  waitForPaneLayoutPersistenceForTest,
} from './paneLayoutPersistence';
import { appStorageGet, hydrateAppStorage, resetAppStorageForTest } from './appStorage';
import { installPaneMocks, makeItem } from '../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

const LEGACY_KEY = 'agentOverflowPaneLayout';

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

function persistedPaneLayout(): unknown {
  const raw = appStorageGet('paneLayout');
  return raw === null ? null : JSON.parse(raw);
}

/**
 * Seed the appStorage bucket with a persisted layout by hydrating from
 * a mocked GetUIState (mirrors a real boot: server bucket → memory).
 * Passing null leaves the bucket empty.
 */
async function installUIStateMock(initialPaneLayout: unknown = makeSavedLayout([], null)) {
  setBindingMock('GetUIState', async () =>
    initialPaneLayout === null ? {} : { paneLayout: JSON.stringify(initialPaneLayout) },
  );
  const setUIState = vi.fn(async () => null);
  setBindingMock('SetUIState', setUIState);
  setBindingMock('DeleteUIState', async () => null);
  await hydrateAppStorage();
  return { setUIState };
}

describe('pane layout persistence', () => {
  beforeEach(async () => {
    localStorage.removeItem(LEGACY_KEY);
    resetBindingMocks();
    resetAppStorageForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
    installPaneLayoutPersistence();
    await installUIStateMock();
  });

  afterEach(() => {
    resetPaneLayoutPersistenceForTest();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('round-trips panes and the focused pane id through appStorage', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left' });
    const right = makeThread({ id: 'right-thread', title: 'Right' });
    installPaneMocks();
    seedPane('left', left);
    seedPane('right', right);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 0.75 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1.25 },
    ]);
    focusPane('right');

    persistPaneLayout();
    await waitForPaneLayoutPersistenceForTest();
    resetPanesForTest();
    setPaneLayoutItemsForTest([]);

    await loadPersistedPaneLayout([left, right]);

    expect(persistedPaneLayout()).toEqual(makeSavedLayout([
      { paneId: 'left', threadId: 'left-thread', ratio: 0.75 },
      { paneId: 'right', threadId: 'right-thread', ratio: 1.25 },
    ], 'right'));
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
    await installUIStateMock(makeSavedLayout([
      { paneId: 'main', threadId: thread.id, ratio: 1 },
    ], 'main'));

    await loadPersistedPaneLayout([thread]);

    expect(getAllPanes().get('main')?.items.map((item) => item.id)).toContain('restored-item');
  });

  it('coalesces multiple resize persistence requests into one trailing write', async () => {
    const mocks = await installUIStateMock();
    vi.useFakeTimers();
    const left = makeThread({ id: 'left-thread' });
    const right = makeThread({ id: 'right-thread' });
    seedPane('left', left);
    seedPane('right', right);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);

    resizeAdjacentPaneLayoutItems('left', 'right', 800, 800, 100, 560);
    resizeAdjacentPaneLayoutItems('left', 'right', 900, 700, -100, 560);
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(199);
    expect(mocks.setUIState).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(1);
    await waitForPaneLayoutPersistenceForTest();

    expect(mocks.setUIState).toHaveBeenCalledTimes(1);
    const layout = persistedPaneLayout() as PaneLayoutPersistedSettings;
    expect(layout.panes.map((pane) => pane.threadId)).toEqual(['left-thread', 'right-thread']);
  });

  it('flushes a pending resize persistence write immediately', async () => {
    const mocks = await installUIStateMock();
    vi.useFakeTimers();
    const left = makeThread({ id: 'left-thread' });
    const right = makeThread({ id: 'right-thread' });
    seedPane('left', left);
    seedPane('right', right);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);

    resizeAdjacentPaneLayoutItems('left', 'right', 800, 800, 120, 560);
    await vi.advanceTimersByTimeAsync(0);
    expect(mocks.setUIState).not.toHaveBeenCalled();

    await flushPaneLayoutPersistence();

    expect(mocks.setUIState).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(400);
    expect(mocks.setUIState).toHaveBeenCalledTimes(1);
  });

  it('drops saved panes whose threads are no longer returned by ListThreads', async () => {
    await installUIStateMock(makeSavedLayout([
      { paneId: 'left', threadId: 'kept-thread', ratio: 1 },
      { paneId: 'right', threadId: 'deleted-thread', ratio: 1 },
    ], 'right'));

    installPaneMocks();
    await loadPersistedPaneLayout([makeThread({ id: 'kept-thread' })]);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left']);
    expect(getAllPanes().get('left')?.threadId).toBe('kept-thread');
    expect(getFocusedPaneId()).toBe('left');
  });

  it('adopts a legacy localStorage layout when the bucket is still empty', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left' });
    // The suite-wide beforeEach hydrates an empty layout into the
    // bucket; adoption only fires for a key with no bucket value at
    // all, so start this scenario from a truly empty bucket.
    resetAppStorageForTest();
    const mocks = await installUIStateMock(null);
    localStorage.setItem(LEGACY_KEY, JSON.stringify(makeSavedLayout([
      { paneId: 'left', threadId: left.id, ratio: 1.5 },
    ], 'left')));
    installPaneMocks();

    await loadPersistedPaneLayout([left]);
    await waitForPaneLayoutPersistenceForTest();

    expect(getPaneLayoutItems()).toEqual([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 1.5 },
    ]);
    expect(persistedPaneLayout()).toEqual(makeSavedLayout([
      { paneId: 'left', threadId: left.id, ratio: 1.5 },
    ], 'left'));
    expect(mocks.setUIState).toHaveBeenCalledWith(
      expect.any(String),
      expect.objectContaining({ paneLayout: expect.stringContaining('left-thread') }),
    );
    expect(localStorage.getItem(LEGACY_KEY)).toBeNull();
  });

  it('continues restoring other panes when one pane hydration fails', async () => {
    const good = makeThread({ id: 'good-thread' });
    const badThreadId = 'bad-thread';
    const bad = makeThreadThatThrowsDuringSwitch(badThreadId);
    await installUIStateMock(makeSavedLayout([
      { paneId: 'good', threadId: good.id, ratio: 1 },
      { paneId: 'bad', threadId: badThreadId, ratio: 1 },
    ], 'bad'));
    installPaneMocks();
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});

    await expect(loadPersistedPaneLayout([good, bad])).resolves.toBeUndefined();

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['good']);
    expect(getAllPanes().get('good')?.threadId).toBe(good.id);
    expect(getAllPanes().has('bad')).toBe(false);
    expect(getFocusedPaneId()).toBe('good');
    expect(consoleError).toHaveBeenCalledWith(
      expect.stringContaining('Failed to restore pane "bad"'),
      expect.any(Error),
    );
  });

  it('ignores unsafe persisted pane ids and caps corrupt layouts', async () => {
    const panes = Array.from({ length: 30 }, (_, index) => ({
      paneId: `pane-${index + 1}`,
      threadId: `thread-${index + 1}`,
      ratio: 1,
    }));
    await installUIStateMock(makeSavedLayout([
      { paneId: 'bad"] [data-pane-id="x', threadId: 'bad-thread', ratio: 1 },
      ...panes,
    ], 'bad"] [data-pane-id="x'));
    installPaneMocks();

    await loadPersistedPaneLayout(
      panes.map((pane) => makeThread({ id: pane.threadId })),
    );

    expect(getPaneLayoutItems()).toHaveLength(24);
    expect(getPaneLayoutItems().map((item) => item.paneId)).not.toContain('bad"] [data-pane-id="x');
    expect(getFocusedPaneId()).toBe('pane-1');
  });

  it('falls back to an empty layout for missing, malformed, and mismatched persisted values', async () => {
    const cases: unknown[] = [
      null,
      { version: 2, panes: [], focusedPaneId: null },
      { version: 1, panes: 'bad', focusedPaneId: null },
    ];
    for (const paneLayout of cases) {
      resetPanesForTest();
      resetPaneLayoutForTest();
      resetAppStorageForTest();
      seedPane('left', makeThread({ id: 'left-thread' }));
      setPaneLayoutItemsForTest([{ id: 'left', paneId: 'left', kind: 'thread', ratio: 1 }]);
      await installUIStateMock(paneLayout);
      installPaneMocks();

      await loadPersistedPaneLayout([makeThread({ id: 'left-thread' })]);

      expect(getPaneLayoutItems()).toEqual([]);
      expect(getAllPanes().size).toBe(0);
      expect(getFocusedPaneId()).toBeNull();
    }
  });

  it('persists an explicit empty pane layout when saving an empty layout', async () => {
    await installUIStateMock(makeSavedLayout([
      { paneId: 'left', threadId: 'thread-1', ratio: 1 },
    ], 'left'));
    setPaneLayoutItemsForTest([]);

    persistPaneLayout();
    await waitForPaneLayoutPersistenceForTest();

    expect(persistedPaneLayout()).toEqual(makeSavedLayout([], null));
  });

  it('treats write failures as best-effort', async () => {
    seedPane('left', makeThread({ id: 'left-thread' }));
    setPaneLayoutItemsForTest([{ id: 'left', paneId: 'left', kind: 'thread', ratio: 1 }]);
    setBindingMock('SetUIState', async () => {
      throw new Error('ui state write failed');
    });
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});

    expect(() => persistPaneLayout()).not.toThrow();
    await waitForPaneLayoutPersistenceForTest();
    expect(consoleError).toHaveBeenCalledWith(
      'appStorage: flush failed:',
      expect.any(Error),
    );
  });
});
