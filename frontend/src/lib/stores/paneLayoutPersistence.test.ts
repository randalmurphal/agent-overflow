import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Thread } from '../types/models';
import type { PaneLayoutPersistedSettings } from '../types/settings';
import {
  flushPaneLayoutPersistence,
  getPaneLayoutItems,
  resetPaneLayoutForTest,
  applyPaneBoundaryDrag,
  setPaneLayoutItemsForTest,
} from './paneLayout.svelte';
import {
  getCompanionPane,
  resetCompanionPanesForTest,
} from './companionPanes.svelte';
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

// A thread whose hydration blows up. The throw is on `mode` rather than `id`
// deliberately: `id` is read by the restore path BEFORE hydration (matching
// persisted pane rows to threads, and the one-thread-one-pane dedup), so a
// counted `id` getter models "hydration failed" only until the next reader is
// added — and then it throws synchronously outside the per-pane isolation and
// takes the whole restore with it. `mode` is read by `switchThread` alone.
function makeThreadThatThrowsDuringSwitch(threadId: string): Thread {
  const thread = makeThread({ id: threadId });
  Object.defineProperty(thread, 'mode', {
    configurable: true,
    get: () => {
      throw new Error('switch failed');
    },
  });
  return thread;
}

function makeSavedLayout(
  panes: Array<
    | { paneId: string; threadId: string; widthPx: number }
    | { paneId: string; kind: 'thread'; threadId: string; widthPx: number }
    | { paneId: string; kind: 'plan' | 'design-preview' | 'review'; sourcePaneId: string; widthPx: number }
  >,
  focusedPaneId: string | null,
): PaneLayoutPersistedSettings {
  return {
    version: 3,
    panes: panes.map((pane) => 'kind' in pane ? pane : { ...pane, kind: 'thread' }),
    focusedPaneId,
  };
}

function makeSavedLayoutV1(
  panes: Array<{ paneId: string; threadId: string; ratio: number }>,
  focusedPaneId: string | null,
): unknown {
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
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    installPaneLayoutPersistence();
    await installUIStateMock();
  });

  afterEach(() => {
    resetPaneLayoutPersistenceForTest();
    resetCompanionPanesForTest();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('round-trips panes, review/plan companions, and the focused pane id through appStorage', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left' });
    const right = makeThread({ id: 'right-thread', title: 'Right' });
    installPaneMocks();
    seedPane('left', left);
    seedPane('right', right);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 660 },
      { id: 'plan-left', paneId: 'plan-left', kind: 'plan', widthPx: 590, sourcePaneId: 'left' },
      { id: 'review-left', paneId: 'review-left', kind: 'review', widthPx: 720, sourcePaneId: 'left' },
      { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1100 },
    ]);
    focusPane('right');

    persistPaneLayout();
    await waitForPaneLayoutPersistenceForTest();
    resetPanesForTest();
    setPaneLayoutItemsForTest([]);

    await loadPersistedPaneLayout([left, right]);

    expect(persistedPaneLayout()).toEqual(makeSavedLayout([
      { paneId: 'left', threadId: 'left-thread', widthPx: 660 },
      { paneId: 'plan-left', kind: 'plan', sourcePaneId: 'left', widthPx: 590 },
      { paneId: 'review-left', kind: 'review', sourcePaneId: 'left', widthPx: 720 },
      { paneId: 'right', threadId: 'right-thread', widthPx: 1100 },
    ], 'right'));
    expect(getPaneLayoutItems()).toEqual([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 660 },
      { id: 'plan-left', paneId: 'plan-left', kind: 'plan', widthPx: 590, sourcePaneId: 'left' },
      { id: 'review-left', paneId: 'review-left', kind: 'review', widthPx: 720, sourcePaneId: 'left' },
      { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1100 },
    ]);
    expect(getAllPanes().get('left')?.threadId).toBe('left-thread');
    expect(getAllPanes().get('right')?.threadId).toBe('right-thread');
    expect(getCompanionPane('plan-left')).toEqual({
      paneId: 'plan-left',
      kind: 'plan',
      sourcePaneId: 'left',
    });
    expect(getCompanionPane('review-left')).toEqual({
      paneId: 'review-left',
      kind: 'review',
      sourcePaneId: 'left',
    });
    expect(getFocusedPaneId()).toBe('right');
  });

  it('round-trips a focused companion pane id', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left' });
    installPaneMocks();
    seedPane('left', left);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 660 },
      { id: 'review-left', paneId: 'review-left', kind: 'review', widthPx: 720, sourcePaneId: 'left' },
    ]);
    focusPane('review-left');

    persistPaneLayout();
    await waitForPaneLayoutPersistenceForTest();
    resetPanesForTest();
    setPaneLayoutItemsForTest([]);

    await loadPersistedPaneLayout([left]);

    // hydrate falls back to a thread pane first; the focused companion is
    // upgraded once its layout item is restored.
    expect(getFocusedPaneId()).toBe('review-left');
    expect(getCompanionPane('review-left')).toEqual({
      paneId: 'review-left',
      kind: 'review',
      sourcePaneId: 'left',
    });
  });

  it('snapshots a focused take-control pane as its source thread pane', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left' });
    installPaneMocks();
    seedPane('left', left);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 660 },
      { id: 'take-control-left', paneId: 'take-control-left', kind: 'take-control', widthPx: 660, sourcePaneId: 'left' },
    ]);
    focusPane('take-control-left');

    persistPaneLayout();
    await waitForPaneLayoutPersistenceForTest();

    // take-control panes are never persisted (a live PTY mirror cannot be
    // restored), so the focus falls back to the source thread pane instead
    // of pointing at a pane the restore can't produce.
    expect(persistedPaneLayout()).toEqual(makeSavedLayout([
      { paneId: 'left', threadId: 'left-thread', widthPx: 660 },
    ], 'left'));
  });

  it('falls back to a surviving thread pane when the focused companion is dropped on restore', async () => {
    const thread = makeThread({ id: 'chat-thread', mode: 'chat' });
    await installUIStateMock(makeSavedLayout([
      { paneId: 'main', threadId: thread.id, widthPx: 1 },
      { paneId: 'design-preview-main', kind: 'design-preview', sourcePaneId: 'main', widthPx: 700 },
    ], 'design-preview-main'));
    installPaneMocks();

    await loadPersistedPaneLayout([thread]);

    // The design-preview companion is dropped (source thread is not
    // design-mode), so its persisted focus cannot be honored.
    expect(getCompanionPane('design-preview-main')).toBeNull();
    expect(getFocusedPaneId()).toBe('main');
  });

  it('rejects a persisted THREAD pane whose id is shaped like a companion id', async () => {
    const good = makeThread({ id: 'good-thread' });
    const impostor = makeThread({ id: 'impostor-thread' });
    await installUIStateMock(makeSavedLayout([
      { paneId: 'main', threadId: good.id, widthPx: 1 },
      // Would collide with the deterministic companion id minted when a
      // plan companion later opens for a pane named 'main'.
      { paneId: 'plan-main', kind: 'thread', threadId: impostor.id, widthPx: 1 },
    ], 'plan-main'));
    installPaneMocks();

    await loadPersistedPaneLayout([good, impostor]);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['main']);
    expect(getFocusedPaneId()).toBe('main');
  });

  it('hydrates restored panes through the normal switchThread path', async () => {
    const thread = makeThread({ id: 'restored-thread' });
    const restoredItem = makeItem({ id: 'restored-item', threadId: thread.id, summary: 'Loaded history' });
    installPaneMocks([restoredItem]);
    await installUIStateMock(makeSavedLayout([
      { paneId: 'main', threadId: thread.id, widthPx: 1 },
    ], 'main'));

    await loadPersistedPaneLayout([thread]);

    expect(getAllPanes().get('main')?.items.map((item) => item.id)).toContain('restored-item');
  });

  it('parses v1 persisted panes as thread panes, migrating ratios to widths', async () => {
    const thread = makeThread({ id: 'v1-thread' });
    await installUIStateMock(makeSavedLayoutV1([
      { paneId: 'main', threadId: thread.id, ratio: 1.5 },
    ], 'main'));
    installPaneMocks();

    await loadPersistedPaneLayout([thread]);

    // 1.5 x the compact density minimum (560).
    expect(getPaneLayoutItems()).toEqual([
      { id: 'main', paneId: 'main', kind: 'thread', widthPx: 840 },
    ]);
    expect(getAllPanes().get('main')?.threadId).toBe(thread.id);
    expect(getFocusedPaneId()).toBe('main');
  });

  it('falls back to the default width for corrupt v3 widthPx values', async () => {
    const thread = makeThread({ id: 'corrupt-width-thread' });
    await installUIStateMock({
      version: 3,
      focusedPaneId: 'main',
      panes: [{ paneId: 'main', kind: 'thread', threadId: thread.id, widthPx: 'huge' }],
    });
    installPaneMocks();

    await loadPersistedPaneLayout([thread]);

    expect(getPaneLayoutItems()).toEqual([
      { id: 'main', paneId: 'main', kind: 'thread', widthPx: 560 },
    ]);
  });

  it('migrates v2 persisted ratios to widths', async () => {
    const thread = makeThread({ id: 'v2-thread' });
    await installUIStateMock({
      version: 2,
      focusedPaneId: 'main',
      panes: [{ paneId: 'main', kind: 'thread', threadId: thread.id, ratio: 2 }],
    });
    installPaneMocks();

    await loadPersistedPaneLayout([thread]);

    // 2 x the compact density minimum (560).
    expect(getPaneLayoutItems()).toEqual([
      { id: 'main', paneId: 'main', kind: 'thread', widthPx: 1120 },
    ]);
  });

  it('clamps oversized legacy ratios and falls back for invalid ones', async () => {
    const big = makeThread({ id: 'big-ratio-thread' });
    const bad = makeThread({ id: 'bad-ratio-thread' });
    await installUIStateMock({
      version: 2,
      focusedPaneId: 'big',
      panes: [
        { paneId: 'big', kind: 'thread', threadId: big.id, ratio: 500 },
        { paneId: 'bad', kind: 'thread', threadId: bad.id, ratio: -3 },
      ],
    });
    installPaneMocks();

    await loadPersistedPaneLayout([big, bad]);

    // 500 caps to 100x min (56000), then normalizes down to the pane-width
    // maximum; -3 is not a positive ratio so it degrades to the density min.
    expect(getPaneLayoutItems()).toEqual([
      { id: 'big', paneId: 'big', kind: 'thread', widthPx: 10000 },
      { id: 'bad', paneId: 'bad', kind: 'thread', widthPx: 560 },
    ]);
  });

  it('coalesces multiple resize persistence requests into one trailing write', async () => {
    const mocks = await installUIStateMock();
    vi.useFakeTimers();
    const left = makeThread({ id: 'left-thread' });
    const right = makeThread({ id: 'right-thread' });
    seedPane('left', left);
    seedPane('right', right);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
    ]);

    const startWidths = new Map([['left', 800], ['right', 800]]);
    applyPaneBoundaryDrag({
      leftPaneId: 'left', rightPaneId: 'right', startWidths,
      deltaPx: 100, minPaneWidth: 560, overflowPx: 0, zeroSum: true,
    });
    applyPaneBoundaryDrag({
      leftPaneId: 'left', rightPaneId: 'right', startWidths,
      deltaPx: -100, minPaneWidth: 560, overflowPx: 0, zeroSum: true,
    });
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
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
    ]);

    applyPaneBoundaryDrag({
      leftPaneId: 'left',
      rightPaneId: 'right',
      startWidths: new Map([['left', 800], ['right', 800]]),
      deltaPx: 120,
      minPaneWidth: 560,
      overflowPx: 0,
      zeroSum: true,
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(mocks.setUIState).not.toHaveBeenCalled();

    await flushPaneLayoutPersistence();

    expect(mocks.setUIState).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(400);
    expect(mocks.setUIState).toHaveBeenCalledTimes(1);
  });

  it('drops saved panes whose threads are no longer returned by ListThreads', async () => {
    await installUIStateMock(makeSavedLayout([
      { paneId: 'left', threadId: 'kept-thread', widthPx: 1 },
      { paneId: 'right', threadId: 'deleted-thread', widthPx: 1 },
    ], 'right'));

    installPaneMocks();
    await loadPersistedPaneLayout([makeThread({ id: 'kept-thread' })]);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left']);
    expect(getAllPanes().get('left')?.threadId).toBe('kept-thread');
    expect(getFocusedPaneId()).toBe('left');
  });

  it('drops a companion whose source thread pane is missing from the snapshot', async () => {
    const kept = makeThread({ id: 'kept-thread' });
    await installUIStateMock({
      version: 3,
      focusedPaneId: 'left',
      panes: [
        { paneId: 'left', kind: 'thread', threadId: kept.id, widthPx: 1 },
        { paneId: 'plan-ghost', kind: 'plan', sourcePaneId: 'ghost', widthPx: 1 },
      ],
    });
    installPaneMocks();

    await loadPersistedPaneLayout([kept]);

    expect(getPaneLayoutItems()).toEqual([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
    ]);
    expect(getCompanionPane('plan-ghost')).toBeNull();
  });

  it('drops design-preview companions when the restored source thread is not design-mode', async () => {
    const thread = makeThread({ id: 'chat-thread', mode: 'chat' });
    await installUIStateMock(makeSavedLayout([
      { paneId: 'main', threadId: thread.id, widthPx: 1 },
      { paneId: 'design-preview-main', kind: 'design-preview', sourcePaneId: 'main', widthPx: 700 },
    ], 'main'));
    installPaneMocks();

    await loadPersistedPaneLayout([thread]);

    expect(getPaneLayoutItems()).toEqual([
      { id: 'main', paneId: 'main', kind: 'thread', widthPx: 1 },
    ]);
    expect(getCompanionPane('design-preview-main')).toBeNull();
  });

  it('adopts a legacy localStorage layout when the bucket is still empty', async () => {
    const left = makeThread({ id: 'left-thread', title: 'Left' });
    // The suite-wide beforeEach hydrates an empty layout into the
    // bucket; adoption only fires for a key with no bucket value at
    // all, so start this scenario from a truly empty bucket.
    resetAppStorageForTest();
    const mocks = await installUIStateMock(null);
    localStorage.setItem(LEGACY_KEY, JSON.stringify(makeSavedLayout([
      { paneId: 'left', threadId: left.id, widthPx: 840 },
    ], 'left')));
    installPaneMocks();

    await loadPersistedPaneLayout([left]);
    await waitForPaneLayoutPersistenceForTest();

    expect(getPaneLayoutItems()).toEqual([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 840 },
    ]);
    expect(persistedPaneLayout()).toEqual(makeSavedLayout([
      { paneId: 'left', threadId: left.id, widthPx: 840 },
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
      { paneId: 'good', threadId: good.id, widthPx: 1 },
      { paneId: 'bad', threadId: badThreadId, widthPx: 1 },
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
      widthPx: 1,
    }));
    await installUIStateMock(makeSavedLayout([
      { paneId: 'bad"] [data-pane-id="x', threadId: 'bad-thread', widthPx: 1 },
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
      { version: 3, panes: [], focusedPaneId: null },
      { version: 1, panes: 'bad', focusedPaneId: null },
      // Unknown/newer schema version: reject rather than misread.
      { version: 4, panes: [{ paneId: 'main', kind: 'thread', threadId: 'left-thread', widthPx: 900 }], focusedPaneId: 'main' },
    ];
    for (const paneLayout of cases) {
      resetPanesForTest();
      resetPaneLayoutForTest();
      resetAppStorageForTest();
      seedPane('left', makeThread({ id: 'left-thread' }));
      setPaneLayoutItemsForTest([{ id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 }]);
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
      { paneId: 'left', threadId: 'thread-1', widthPx: 1 },
    ], 'left'));
    setPaneLayoutItemsForTest([]);

    persistPaneLayout();
    await waitForPaneLayoutPersistenceForTest();

    expect(persistedPaneLayout()).toEqual(makeSavedLayout([], null));
  });

  it('treats write failures as best-effort', async () => {
    seedPane('left', makeThread({ id: 'left-thread' }));
    setPaneLayoutItemsForTest([{ id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 }]);
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
