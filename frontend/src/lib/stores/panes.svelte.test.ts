import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  createPane,
  destroyPane,
  focusPane,
  getAllPanes,
  getFocusedPane,
  getFocusedPaneId,
  getMainPane,
  getPaneActivation,
  isThreadVisible,
  listPanes,
  openThreadFromNavigation,
  openThreadInPane,
  panesShowingThread,
  registerPaneForTest,
  resetPanesForTest,
  syncThread,
} from './panes.svelte';
import { createThreadPane } from './thread.svelte';
import {
  getThreads,
  prependThread,
  refreshThreads,
} from './threads.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { Thread } from '../types/models';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
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

describe('panes store', () => {
  beforeEach(() => {
    // Module state is shared across tests; drain between cases.
    resetPanesForTest();
    setBindingMock('AutoResumeThread', async () => {});
  });

  afterEach(() => {
    resetBindingMocks();
  });

  describe('getMainPane()', () => {
    it('creates the main pane lazily on first call', () => {
      expect(getAllPanes().size).toBe(0);
      const pane = getMainPane();
      expect(pane).toBeDefined();
      expect(getAllPanes().size).toBe(1);
      expect(getAllPanes().get('main')).toBe(pane);
    });

    it('returns the same instance on subsequent calls', () => {
      const a = getMainPane();
      const b = getMainPane();
      expect(a).toBe(b);
    });

    it('exposes a usable pane contract', () => {
      const pane = getMainPane();
      expect(pane.thread).toBeNull();
      expect(pane.items).toEqual([]);
      expect(pane.pendingApprovals).toEqual([]);
      expect(typeof pane.upsertItem).toBe('function');
    });
  });

  describe('pane routing', () => {
    it('tracks focused pane separately from the main pane singleton', () => {
      const main = getMainPane();
      const secondary = createPane('secondary');

      expect(getFocusedPane()).toBe(main);
      focusPane('secondary');

      expect(getFocusedPane()).toBe(secondary);
      expect(getFocusedPaneId()).toBe('secondary');
    });

    it('lists panes without exposing the registry as the production iteration contract', () => {
      const main = getMainPane();
      const secondary = createPane('secondary');

      expect(listPanes()).toEqual([main, secondary]);
    });

    it('destroys a pane and moves focus to a remaining pane', () => {
      const main = getMainPane();
      createPane('secondary');
      focusPane('secondary');

      destroyPane('secondary');

      expect(listPanes()).toEqual([main]);
      expect(getFocusedPane()).toBe(main);
      expect(getPaneActivation('secondary')).toBe('committed');
    });

    it('opens a thread in the requested pane when it is not already visible', async () => {
      const thread = makeThread({ id: 'target' });
      const pane = createThreadPane({ paneId: 'external' });
      setBindingMock('SwitchThread', async () => thread);
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [], oldestTurnIndex: -1, hasMore: false,
      }));
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [], oldestTurnIndex: -1, hasMore: false,
      }));
      setBindingMock('ListRecentTurns', async () => []);
      setBindingMock('ListThreadCheckpoints', async () => []);
      setBindingMock('GetThreadLiveState', async () => null);
      setBindingMock('ListPendingInteractiveRequests', async () => null);

      await openThreadInPane(thread, pane);

      expect(pane.threadId).toBe('target');
      expect(getFocusedPaneId()).toBe('external');
    });

    it('focuses the existing pane instead of duplicating a visible thread', async () => {
      const thread = makeThread({ id: 'visible' });
      setBindingMock('SwitchThread', async () => thread);
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [], oldestTurnIndex: -1, hasMore: false,
      }));
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [], oldestTurnIndex: -1, hasMore: false,
      }));
      setBindingMock('ListRecentTurns', async () => []);
      setBindingMock('ListThreadCheckpoints', async () => []);
      setBindingMock('GetThreadLiveState', async () => null);
      setBindingMock('ListPendingInteractiveRequests', async () => null);
      const left = createPane('left');
      const right = createPane('right');
      await left.switchThread(thread);

      const focused = await openThreadInPane(thread, right);

      expect(focused).toBe(left);
      expect(right.threadId).toBeNull();
      expect(getFocusedPaneId()).toBe('left');
      expect(isThreadVisible(thread.id)).toBe(true);
      expect(panesShowingThread(thread.id)).toEqual([left]);
    });

    it('promotes an existing preview pane when opened through the committed path', async () => {
      const thread = makeThread({ id: 'previewed' });
      setBindingMock('SwitchThread', async () => thread);
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [], oldestTurnIndex: -1, hasMore: false,
      }));
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [], oldestTurnIndex: -1, hasMore: false,
      }));
      setBindingMock('ListRecentTurns', async () => []);
      setBindingMock('ListThreadCheckpoints', async () => []);
      setBindingMock('GetThreadLiveState', async () => null);
      setBindingMock('ListPendingInteractiveRequests', async () => null);
      const left = createPane('left');
      const right = createPane('right');

      const preview = await openThreadFromNavigation(thread, left);
      expect(preview).toBe(left);
      expect(getPaneActivation('left')).toBe('preview');

      const committed = await openThreadInPane(thread, right);
      expect(committed).toBe(left);
      expect(right.threadId).toBeNull();
      expect(getPaneActivation('left')).toBe('committed');
    });
  });

  describe('syncThread()', () => {
    async function buildPaneWithThread(thread: Thread) {
      setBindingMock('SwitchThread', async () => thread);
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [], oldestTurnIndex: -1, hasMore: false,
      }));
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [], oldestTurnIndex: -1, hasMore: false,
      }));
      setBindingMock('ListItems', async () => []);
      setBindingMock('ListRecentTurns', async () => []);
      setBindingMock('ListThreadCheckpoints', async () => []);
      setBindingMock('MarkThreadRead', async () => {});
      setBindingMock('MarkThreadUnread', async () => {});
      const pane = createThreadPane();
      await pane.switchThread(thread);
      registerPaneForTest('main', pane);
      return pane;
    }

    it('updates a pane currently displaying the thread', async () => {
      const original = makeThread({ id: 't', title: 'Old' });
      const pane = await buildPaneWithThread(original);

      syncThread({ ...original, title: 'New' });

      expect(pane.thread?.title).toBe('New');
    });

    it('updates the global threads registry too', async () => {
      const original = makeThread({ id: 't', title: 'Old' });
      // Seed the global registry with the original.
      setBindingMock('ListThreads', async () => [original]);
      await refreshThreads();
      await buildPaneWithThread(original);

      syncThread({ ...original, title: 'New' });

      expect(getThreads().find((t) => t.id === 't')?.title).toBe('New');
    });

    it('does not update a pane displaying a different thread', async () => {
      const other = makeThread({ id: 'other', title: 'Other' });
      const pane = await buildPaneWithThread(other);

      syncThread(makeThread({ id: 't', title: 'Unrelated' }));

      expect(pane.thread?.title).toBe('Other');
    });

    it('is a no-op for unknown thread ids in the registry (registry only replaces existing)', () => {
      // No pane registered, no entry in registry. syncThread should not throw.
      expect(() => syncThread(makeThread({ id: 'ghost' }))).not.toThrow();
      expect(getThreads().find((t) => t.id === 'ghost')).toBeUndefined();
    });

    it('replaces existing registry entry without touching siblings', async () => {
      prependThread(makeThread({ id: 'a', title: 'A' }));
      prependThread(makeThread({ id: 'b', title: 'B' }));

      syncThread(makeThread({ id: 'a', title: 'A renamed' }));

      const list = getThreads();
      expect(list.find((t) => t.id === 'a')?.title).toBe('A renamed');
      expect(list.find((t) => t.id === 'b')?.title).toBe('B');
    });
  });
});
