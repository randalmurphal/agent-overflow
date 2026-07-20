import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createPane,
  closePanesShowingThread,
  closePanesShowingThreads,
  destroyPane,
  ensureMainPane,
  focusAdjacentPane,
  focusPane,
  getAllPanes,
  getFocusedPane,
  getFocusedPaneId,
  getFocusedThreadPaneId,
  getMainPane,
  getPaneActivation,
  isThreadVisible,
  listPanes,
  moveFocusedPane,
  openEmptyPane,
  openThreadInNewPane,
  openThreadFromNavigation,
  openThreadInPane,
  panesShowingThread,
  registerPaneForTest,
  resetPanesForTest,
  revealPane,
  syncThread,
} from './panes.svelte';
import { REVEAL_PANE_EVENT } from './eventNames';
import { createThreadPane } from './thread.svelte';
import {
  getThreads,
  prependThread,
  refreshThreads,
} from './threads.svelte';
import {
  addPaneLayoutItem,
  getPaneLayoutItems,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
} from './paneLayout.svelte';
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

function mockThreadSwitch(thread: Thread): void {
  setBindingMock('SwitchThread', async () => thread);
  setBindingMock('ListRecentThreadItems', async () => ({
    items: [], oldestTurnIndex: -1, hasMore: false,
  }));
  setBindingMock('ListThreadSliceAround', async () => ({
    items: [], oldestTurnIndex: -1, hasMore: false,
  }));
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('GetThreadLiveState', async () => null);
  setBindingMock('ListPendingInteractiveRequests', async () => null);
}

describe('panes store', () => {
  beforeEach(() => {
    // Module state is shared across tests; drain between cases.
    resetPanesForTest();
    resetPaneLayoutForTest();
    setBindingMock('AutoResumeThread', async () => {});
  });

  afterEach(() => {
    resetBindingMocks();
  });

  describe('getMainPane()', () => {
    it('fails visibly when the main pane is missing', () => {
      expect(getAllPanes().size).toBe(0);
      expect(() => getMainPane()).toThrow(/missing the main pane/i);
    });

    it('ensureMainPane creates the main pane explicitly', () => {
      expect(getAllPanes().size).toBe(0);
      const pane = ensureMainPane();
      expect(pane).toBeDefined();
      expect(getAllPanes().size).toBe(1);
      expect(getAllPanes().get('main')).toBe(pane);
    });

    it('returns the same instance on subsequent calls', () => {
      const a = ensureMainPane();
      const b = getMainPane();
      expect(a).toBe(b);
    });

    it('exposes a usable pane contract', () => {
      const pane = ensureMainPane();
      expect(pane.thread).toBeNull();
      expect(pane.items).toEqual([]);
      expect(pane.pendingApprovals).toEqual([]);
      expect(typeof pane.upsertItem).toBe('function');
    });
  });

  describe('pane routing', () => {
    it('tracks focused pane separately from the main pane singleton', () => {
      const main = ensureMainPane();
      const secondary = createPane('secondary');

      expect(getFocusedPane()).toBe(main);
      focusPane('secondary');

      expect(getFocusedPane()).toBe(secondary);
      expect(getFocusedPaneId()).toBe('secondary');
    });

    it('lists panes without exposing the registry as the production iteration contract', () => {
      const main = ensureMainPane();
      const secondary = createPane('secondary');

      expect(listPanes()).toEqual([main, secondary]);
    });

    it('destroys a pane and moves focus to a remaining pane', () => {
      const main = ensureMainPane();
      createPane('secondary');
      focusPane('secondary');

      destroyPane('secondary');

      expect(listPanes()).toEqual([main]);
      expect(getFocusedPane()).toBe(main);
      expect(getPaneActivation('secondary')).toBe('committed');
    });

    it('destroying a focused pane moves focus to the adjacent left neighbor', () => {
      const left = createPane('left');
      const middle = createPane('middle');
      createPane('right');
      setPaneLayoutItemsForTest([
        { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
        { id: 'middle', paneId: 'middle', kind: 'thread', widthPx: 1 },
        { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
      ]);
      focusPane('right');

      destroyPane('right');

      expect(getFocusedPane()).toBe(middle);
      expect(listPanes()).toEqual([left, middle]);
    });

    it('destroying the leftmost focused pane moves focus to the new leftmost pane', () => {
      createPane('left');
      const right = createPane('right');
      setPaneLayoutItemsForTest([
        { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
        { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
      ]);
      focusPane('left');

      destroyPane('left');

      expect(getFocusedPane()).toBe(right);
    });

    it('destroying the last focused pane leaves no focused pane and no layout items', () => {
      const main = ensureMainPane();
      focusPane('main');

      destroyPane(main.paneId);

      expect(getFocusedPaneId()).toBeNull();
      expect(getAllPanes().size).toBe(0);
      expect(getPaneLayoutItems()).toEqual([]);
    });

    it('opens a thread in the requested pane when it is not already visible', async () => {
      const thread = makeThread({ id: 'target' });
      const pane = createThreadPane({ paneId: 'external' });
      mockThreadSwitch(thread);

      await openThreadInPane(thread, pane);

      expect(pane.threadId).toBe('target');
      expect(getFocusedPaneId()).toBe('external');
    });

    it('throws when replacing a thread into a missing string-target pane', async () => {
      const thread = makeThread({ id: 'target' });

      await expect(openThreadInPane(thread, 'missing-pane')).rejects.toThrow(/missing-pane/);
      expect(getAllPanes().size).toBe(0);
    });

    it('focuses the existing pane instead of duplicating a visible thread', async () => {
      const thread = makeThread({ id: 'visible' });
      mockThreadSwitch(thread);
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
      mockThreadSwitch(thread);
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

    it('opens a thread in a new pane with layout when it is not already visible', async () => {
      const thread = makeThread({ id: 'new-pane-thread' });
      mockThreadSwitch(thread);

      const pane = await openThreadInNewPane(thread);

      expect(pane.threadId).toBe(thread.id);
      expect(getFocusedPaneId()).toBe(pane.paneId);
      expect(getPaneLayoutItems().some((item) => item.paneId === pane.paneId)).toBe(true);
    });

    it('opens a new thread pane immediately to the right of the focused pane', async () => {
      const thread = makeThread({ id: 'right-of-focused' });
      mockThreadSwitch(thread);
      createPane('left');
      createPane('middle');
      createPane('right');
      setPaneLayoutItemsForTest([
        { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
        { id: 'middle', paneId: 'middle', kind: 'thread', widthPx: 1 },
        { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
      ]);
      focusPane('middle');

      const pane = await openThreadInNewPane(thread);

      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
        'left',
        'middle',
        pane.paneId,
        'right',
      ]);
      expect(getFocusedPaneId()).toBe(pane.paneId);
    });

    it('appends a new thread pane when the focused pane is already rightmost', async () => {
      const thread = makeThread({ id: 'right-edge-new-pane' });
      mockThreadSwitch(thread);
      createPane('left');
      createPane('right');
      setPaneLayoutItemsForTest([
        { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
        { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
      ]);
      focusPane('right');

      const pane = await openThreadInNewPane(thread);

      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left', 'right', pane.paneId]);
    });

    it('appends a new thread pane when there is no valid focused layout pane', async () => {
      const thread = makeThread({ id: 'missing-focus-new-pane' });
      mockThreadSwitch(thread);
      createPane('left');
      createPane('right');
      setPaneLayoutItemsForTest([
        { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
        { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
      ]);

      const pane = await openThreadInNewPane(thread);

      expect(getFocusedPaneId()).toBe(pane.paneId);
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left', 'right', pane.paneId]);
    });

    it('uses an explicit new-pane insertion index instead of focused-pane placement', async () => {
      const thread = makeThread({ id: 'explicit-index-new-pane' });
      mockThreadSwitch(thread);
      createPane('left');
      createPane('middle');
      createPane('right');
      setPaneLayoutItemsForTest([
        { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
        { id: 'middle', paneId: 'middle', kind: 'thread', widthPx: 1 },
        { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
      ]);
      focusPane('middle');

      const pane = await openThreadInNewPane(thread, 0);

      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
        pane.paneId,
        'left',
        'middle',
        'right',
      ]);
    });

    it('opens an empty pane immediately to the right of the focused pane', () => {
      createPane('left');
      createPane('middle');
      createPane('right');
      setPaneLayoutItemsForTest([
        { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
        { id: 'middle', paneId: 'middle', kind: 'thread', widthPx: 1 },
        { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
      ]);
      focusPane('middle');

      const pane = openEmptyPane();

      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
        'left',
        'middle',
        pane.paneId,
        'right',
      ]);
      expect(getFocusedPaneId()).toBe(pane.paneId);
    });

    it('new-pane routing focuses the existing pane instead of duplicating a visible thread', async () => {
      const thread = makeThread({ id: 'visible-new-pane' });
      mockThreadSwitch(thread);
      const left = createPane('left');
      await left.switchThread(thread);
      setPaneLayoutItemsForTest([
        { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
      ]);

      const focused = await openThreadInNewPane(thread);

      expect(focused).toBe(left);
      expect(listPanes()).toEqual([left]);
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left']);
      expect(getFocusedPaneId()).toBe('left');
    });

    it('focusPane sets logical focus without dispatching a reveal', () => {
      createPane('left');
      createPane('right');
      setPaneLayoutItemsForTest([
        { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
        { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
      ]);
      const onReveal = vi.fn();
      window.addEventListener(REVEAL_PANE_EVENT, onReveal);
      try {
        focusPane('right');
        expect(getFocusedPaneId()).toBe('right');
        expect(onReveal).not.toHaveBeenCalled();

        revealPane('right');
        expect(onReveal).toHaveBeenCalledTimes(1);
      } finally {
        window.removeEventListener(REVEAL_PANE_EVENT, onReveal);
      }
    });

    it('a focused companion resolves to its source for thread-scoped consumers', () => {
      const source = createPane('source');
      setPaneLayoutItemsForTest([
        { id: 'source', paneId: 'source', kind: 'thread', widthPx: 1 },
        { id: 'review-source', paneId: 'review-source', kind: 'review', widthPx: 1, sourcePaneId: 'source' },
      ]);

      focusPane('review-source');

      expect(getFocusedPaneId()).toBe('review-source');
      expect(getFocusedThreadPaneId()).toBe('source');
      expect(getFocusedPane()).toBe(source);
    });

    it('focusPane rejects ids that are neither registered panes nor layout items', () => {
      createPane('only');
      setPaneLayoutItemsForTest([
        { id: 'only', paneId: 'only', kind: 'thread', widthPx: 1 },
      ]);
      focusPane('only');

      focusPane('ghost');

      expect(getFocusedPaneId()).toBe('only');
    });

    it('focusAdjacentPane stops on companion panes in layout order', () => {
      createPane('left');
      createPane('right');
      setPaneLayoutItemsForTest([
        { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
        { id: 'review-left', paneId: 'review-left', kind: 'review', widthPx: 1, sourcePaneId: 'left' },
        { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
      ]);
      focusPane('left');

      const first = focusAdjacentPane(1);
      expect(first?.paneId).toBe('review-left');
      expect(first?.kind).toBe('review');
      expect(getFocusedPaneId()).toBe('review-left');

      const second = focusAdjacentPane(1);
      expect(second?.paneId).toBe('right');
      expect(focusAdjacentPane(1)).toBeNull();
      expect(getFocusedPaneId()).toBe('right');
    });

    it('moves the focused pane through the layout order and clamps at edges', () => {
      createPane('left');
      createPane('middle');
      createPane('right');
      setPaneLayoutItemsForTest([
        { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
        { id: 'middle', paneId: 'middle', kind: 'thread', widthPx: 1 },
        { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
      ]);
      focusPane('middle');

      moveFocusedPane(-1);
      moveFocusedPane(-1);
      moveFocusedPane(1);

      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left', 'middle', 'right']);
    });

    it('moveFocusedPane with a focused companion moves the whole source block', () => {
      createPane('left');
      createPane('source');
      setPaneLayoutItemsForTest([
        { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
        { id: 'source', paneId: 'source', kind: 'thread', widthPx: 1 },
        { id: 'review-source', paneId: 'review-source', kind: 'review', widthPx: 1, sourcePaneId: 'source' },
      ]);
      focusPane('review-source');

      moveFocusedPane(-1);

      // The [source + companion] block steps past 'left' as one unit —
      // a companion can never be separated from its source.
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
        'source',
        'review-source',
        'left',
      ]);
      expect(getFocusedPaneId()).toBe('review-source');
    });

    it('destroys every pane showing a removed thread', async () => {
      const thread = makeThread({ id: 'removed-thread' });
      const other = makeThread({ id: 'other-thread' });
      const left = createPane('left');
      const right = createPane('right');
      const untouched = createPane('untouched');
      addPaneLayoutItem({ id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 });
      addPaneLayoutItem({ id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 });
      addPaneLayoutItem({ id: 'untouched', paneId: 'untouched', kind: 'thread', widthPx: 1 });

      await left.replaceThread(thread);
      await right.replaceThread(thread);
      await untouched.replaceThread(other);

      closePanesShowingThread(thread.id);

      expect(getAllPanes().has('left')).toBe(false);
      expect(getAllPanes().has('right')).toBe(false);
      expect(getAllPanes().has('untouched')).toBe(true);
      expect(getPaneLayoutItems().find((i) => i.paneId === 'left')).toBeUndefined();
      expect(getPaneLayoutItems().find((i) => i.paneId === 'right')).toBeUndefined();
      expect(untouched.threadId).toBe(other.id);
    });

    it('transfers focus to adjacent pane when focused pane is destroyed', async () => {
      const thread = makeThread({ id: 'doomed-thread' });
      const other = makeThread({ id: 'survivor-thread' });
      const left = createPane('left');
      const right = createPane('right');
      addPaneLayoutItem({ id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 });
      addPaneLayoutItem({ id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 });

      await left.replaceThread(other);
      await right.replaceThread(thread);
      focusPane('right');

      closePanesShowingThread(thread.id);

      expect(getFocusedPaneId()).toBe('left');
    });

    it('destroys every pane showing one of a removed project thread set', async () => {
      const first = makeThread({ id: 'first-deleted' });
      const second = makeThread({ id: 'second-deleted' });
      const other = makeThread({ id: 'other-thread' });
      const left = createPane('left');
      const right = createPane('right');
      const untouched = createPane('untouched');
      addPaneLayoutItem({ id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 });
      addPaneLayoutItem({ id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 });
      addPaneLayoutItem({ id: 'untouched', paneId: 'untouched', kind: 'thread', widthPx: 1 });

      await left.replaceThread(first);
      await right.replaceThread(second);
      await untouched.replaceThread(other);

      closePanesShowingThreads([first.id, second.id]);

      expect(getAllPanes().has('left')).toBe(false);
      expect(getAllPanes().has('right')).toBe(false);
      expect(getAllPanes().has('untouched')).toBe(true);
      expect(getPaneLayoutItems().find((i) => i.paneId === 'left')).toBeUndefined();
      expect(getPaneLayoutItems().find((i) => i.paneId === 'right')).toBeUndefined();
      expect(untouched.threadId).toBe(other.id);
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
