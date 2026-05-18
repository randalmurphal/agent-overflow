import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Thread } from '../types/models';
import {
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
  loadFromStorage,
  PANE_LAYOUT_STORAGE_KEY,
  persistToStorage,
  resetPaneLayoutPersistenceForTest,
} from './paneLayoutPersistence';
import { installPaneMocks, makeItem } from '../../test/helpers/chat';
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

describe('pane layout persistence', () => {
  beforeEach(() => {
    localStorage.removeItem(PANE_LAYOUT_STORAGE_KEY);
    resetPanesForTest();
    resetPaneLayoutForTest();
    installPaneLayoutPersistence();
  });

  afterEach(() => {
    resetPaneLayoutPersistenceForTest();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('round-trips panes and the focused pane id', async () => {
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

    persistToStorage();
    resetPanesForTest();
    setPaneLayoutItemsForTest([]);

    await loadFromStorage([left, right]);

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
    localStorage.setItem(PANE_LAYOUT_STORAGE_KEY, JSON.stringify({
      version: 1,
      panes: [{ paneId: 'main', threadId: thread.id, ratio: 1 }],
      focusedPaneId: 'main',
    }));

    await loadFromStorage([thread]);

    expect(getAllPanes().get('main')?.items.map((item) => item.id)).toContain('restored-item');
  });

  it('coalesces multiple resize persistence requests into one trailing write', async () => {
    vi.useFakeTimers();
    const left = makeThread({ id: 'left-thread' });
    const right = makeThread({ id: 'right-thread' });
    seedPane('left', left);
    seedPane('right', right);
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);
    let writeCount = 0;
    const originalSetItem = localStorage.setItem.bind(localStorage);
    vi.spyOn(localStorage, 'setItem').mockImplementation((key: string, value: string) => {
      if (key === PANE_LAYOUT_STORAGE_KEY) writeCount += 1;
      originalSetItem(key, value);
    });

    resizeAdjacentPaneLayoutItems('left', 'right', 800, 800, 100, 560);
    resizeAdjacentPaneLayoutItems('left', 'right', 900, 700, -100, 560);
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(199);
    expect(writeCount).toBe(0);

    await vi.advanceTimersByTimeAsync(1);

    expect(writeCount).toBe(1);
    expect(localStorage.getItem(PANE_LAYOUT_STORAGE_KEY)).toContain('left-thread');
  });

  it('drops saved panes whose threads are no longer returned by ListThreads', async () => {
    localStorage.setItem(PANE_LAYOUT_STORAGE_KEY, JSON.stringify({
      version: 1,
      panes: [
        { paneId: 'left', threadId: 'kept-thread', ratio: 1 },
        { paneId: 'right', threadId: 'deleted-thread', ratio: 1 },
      ],
      focusedPaneId: 'right',
    }));

    installPaneMocks();
    await loadFromStorage([makeThread({ id: 'kept-thread' })]);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left']);
    expect(getAllPanes().get('left')?.threadId).toBe('kept-thread');
    expect(getFocusedPaneId()).toBe('left');
  });

  it('continues restoring other panes when one pane hydration fails', async () => {
    const good = makeThread({ id: 'good-thread' });
    const badThreadId = 'bad-thread';
    const bad = makeThreadThatThrowsDuringSwitch(badThreadId);
    localStorage.setItem(PANE_LAYOUT_STORAGE_KEY, JSON.stringify({
      version: 1,
      panes: [
        { paneId: 'good', threadId: good.id, ratio: 1 },
        { paneId: 'bad', threadId: badThreadId, ratio: 1 },
      ],
      focusedPaneId: 'bad',
    }));
    installPaneMocks();
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});

    await expect(loadFromStorage([good, bad])).resolves.toBeUndefined();

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['good']);
    expect(getAllPanes().get('good')?.threadId).toBe(good.id);
    expect(getAllPanes().has('bad')).toBe(false);
    expect(getFocusedPaneId()).toBe('good');
    expect(consoleError).toHaveBeenCalledWith(
      expect.stringContaining('Failed to restore pane "bad"'),
      expect.any(Error),
    );
  });

  it('falls back to an empty layout when reading localStorage throws', async () => {
    seedPane('left', makeThread({ id: 'left-thread' }));
    setPaneLayoutItemsForTest([{ id: 'left', paneId: 'left', kind: 'thread', ratio: 1 }]);
    vi.spyOn(localStorage, 'getItem').mockImplementation(() => {
      throw new Error('storage blocked');
    });
    const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    await loadFromStorage([makeThread({ id: 'left-thread' })]);

    expect(getPaneLayoutItems()).toEqual([]);
    expect(getAllPanes().size).toBe(0);
    expect(getFocusedPaneId()).toBeNull();
    expect(consoleWarn).toHaveBeenCalledWith(
      'Failed to read pane layout persistence:',
      expect.any(Error),
    );
  });

  it('ignores unsafe persisted pane ids and caps corrupt layouts', async () => {
    const panes = Array.from({ length: 30 }, (_, index) => ({
      paneId: `pane-${index + 1}`,
      threadId: `thread-${index + 1}`,
      ratio: 1,
    }));
    localStorage.setItem(PANE_LAYOUT_STORAGE_KEY, JSON.stringify({
      version: 1,
      panes: [
        { paneId: 'bad"] [data-pane-id="x', threadId: 'bad-thread', ratio: 1 },
        ...panes,
      ],
      focusedPaneId: 'bad"] [data-pane-id="x',
    }));
    installPaneMocks();

    await loadFromStorage(
      panes.map((pane) => makeThread({ id: pane.threadId })),
    );

    expect(getPaneLayoutItems()).toHaveLength(24);
    expect(getPaneLayoutItems().map((item) => item.paneId)).not.toContain('bad"] [data-pane-id="x');
    expect(getFocusedPaneId()).toBe('pane-1');
  });

  it('falls back to an empty layout for missing, malformed, and mismatched persisted JSON', async () => {
    const cases = [
      null,
      '{not-json',
      JSON.stringify({ version: 2, panes: [], focusedPaneId: null }),
    ];
    for (const raw of cases) {
      seedPane('left', makeThread({ id: 'left-thread' }));
      setPaneLayoutItemsForTest([{ id: 'left', paneId: 'left', kind: 'thread', ratio: 1 }]);
      if (raw === null) {
        localStorage.removeItem(PANE_LAYOUT_STORAGE_KEY);
      } else {
        localStorage.setItem(PANE_LAYOUT_STORAGE_KEY, raw);
      }

      installPaneMocks();
      await loadFromStorage([makeThread({ id: 'left-thread' })]);

      expect(getPaneLayoutItems()).toEqual([]);
      expect(getAllPanes().size).toBe(0);
      expect(getFocusedPaneId()).toBeNull();
    }
  });

  it('clears the storage key when saving an empty layout', () => {
    localStorage.setItem(PANE_LAYOUT_STORAGE_KEY, JSON.stringify({
      version: 1,
      panes: [{ paneId: 'left', threadId: 'thread-1', ratio: 1 }],
      focusedPaneId: 'left',
    }));
    setPaneLayoutItemsForTest([]);

    persistToStorage();

    expect(localStorage.getItem(PANE_LAYOUT_STORAGE_KEY)).toBeNull();
  });

  it('treats localStorage write failures as best-effort', () => {
    seedPane('left', makeThread({ id: 'left-thread' }));
    setPaneLayoutItemsForTest([{ id: 'left', paneId: 'left', kind: 'thread', ratio: 1 }]);
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota exceeded');
    });
    const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(() => persistToStorage()).not.toThrow();
    expect(consoleWarn).toHaveBeenCalledWith(
      'Failed to write pane layout persistence:',
      expect.any(Error),
    );
  });
});
