import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { getAllPanes, getMainPane, syncThread } from './panes.svelte';
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
    getAllPanes().clear();
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

  describe('syncThread()', () => {
    afterEach(() => {
      resetBindingMocks();
    });

    async function buildPaneWithThread(thread: Thread) {
      setBindingMock('SwitchThread', async () => thread);
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [], oldestTurnIndex: -1, hasMore: false,
      }));
      setBindingMock('ListItems', async () => []);
      setBindingMock('ListRecentTurns', async () => []);
      setBindingMock('ListThreadCheckpoints', async () => []);
      setBindingMock('MarkThreadRead', async () => {});
      setBindingMock('MarkThreadUnread', async () => {});
      const pane = createThreadPane();
      await pane.switchThread(thread);
      getAllPanes().set('main', pane);
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
