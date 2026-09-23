// stores/threadSubagentChildren.test.ts
//
// Subagent children through the pane: streamed child rows never enter the
// main window and leave no per-child state there, while scoped surfaces
// load them themselves. Also the recent-window prune over the rows the
// window does hold.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createThreadPane } from './thread.svelte';
import { __resetAgentPaneStateForTest } from './agentPane.svelte';
import { createAgentScopeView } from './agentScopeView.svelte';
import { type Item } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { installTimelineScopeCapability, installPaneMocks, makeItem, makeThread, stubScrollController } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import {
  ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
  ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
  ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
} from './threadPaneShared';

const MAX = ACTIVE_TIMELINE_WINDOW_MAX_ITEMS;
const TARGET = ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS;
const CEILING = ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS;

describe('subagent children', () => {
  beforeEach(() => {
    installTimelineScopeCapability();
    installThreadPaneTestEnv();
    __resetAgentPaneStateForTest();
  });

  describe('main window scope', () => {
    // Live turns stream subagent child rows. The main window drops each one
    // at admission and holds no record of it: a collapsed card reads its
    // launch row's backend decoration, and an expanded card or the agent
    // pane loads the rows through a scoped surface.
    function launchItem(threadId: string, overrides: Partial<Item> = {}): Item {
      return makeItem({
        id: 'anchor',
        threadId,
        turnIndex: 1,
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Task',
        status: 'running',
        summary: 'Task: investigate',
        ...overrides,
      });
    }

    function childItem(threadId: string, overrides: Partial<Item> = {}): Item {
      return makeItem({
        id: 'child-1',
        threadId,
        turnIndex: 1,
        itemIndex: 1,
        parentId: 'anchor',
        kind: 'tool_call',
        toolName: 'Bash',
        status: 'completed',
        summary: 'ran the build',
        ...overrides,
      });
    }

    async function paneWithAnchor(threadId: string, anchor?: Item) {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 'pre', threadId, turnIndex: 0, itemIndex: 0 }),
          anchor ?? launchItem(threadId),
        ],
        oldestTurnIndex: 0,
        newestTurnIndex: 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: threadId }));
      return pane;
    }

    it('drops 1,000 streamed children onto 1,000 roots without committing the root list', async () => {
      const threadId = 'children-cost';
      const roots: Item[] = [];
      for (let i = 0; i < 1000; i += 1) {
        const launch = i % 100 === 0;
        roots.push(makeItem({
          id: `root-${i}`, threadId, turnIndex: i, itemIndex: 0,
          kind: launch ? 'tool_call' : 'assistant_text',
          toolName: launch ? 'Agent' : undefined,
          status: launch ? 'running' : 'completed',
          summary: launch ? 'Agent: work' : `row ${i}`,
        }));
      }
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: roots, oldestTurnIndex: 0, newestTurnIndex: 999,
        hasMore: false, hasMoreOlder: false, hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: threadId }));
      expect(pane.items).toHaveLength(1000);
      const before = pane.items;
      const revision = pane.timelineRevision;
      const memory = pane.debugMemoryStats();

      const started = performance.now();
      for (let i = 0; i < 1000; i += 1) {
        const anchor = (i % 10) * 100;
        pane.applyProviderItemUpserts([makeItem({
          id: `child-${i}`, threadId, turnIndex: anchor, itemIndex: 1 + i,
          parentId: `root-${anchor}`, kind: 'tool_call', toolName: 'Bash',
          status: 'completed', summary: `ran ${i}`,
        })]);
      }
      const elapsed = performance.now() - started;

      expect(pane.items).toBe(before);
      expect(pane.timelineRevision).toBe(revision);
      expect(pane.debugMemoryStats()).toEqual(memory);
      expect(elapsed).toBeLessThan(100);
    });

    it('keeps no state for streamed children, their deltas or their patches', async () => {
      const threadId = 'children-state';
      const pane = await paneWithAnchor(threadId);
      const itemsBefore = pane.items;
      const revisionBefore = pane.timelineRevision;
      const memory = pane.debugMemoryStats();
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      try {
        for (let index = 1; index <= 20; index += 1) {
          pane.upsertItem(childItem(threadId, { id: `child-${index}`, itemIndex: index, payloadId: `payload-${index}` }));
        }
        // A replayed upsert (transport reconnect echo) changes nothing either.
        pane.upsertItem(childItem(threadId, { id: 'child-1', itemIndex: 1, payloadId: 'payload-1' }));
        // A streaming child's creation, deltas and settle patch (the
        // streaming-text shape), a nested launch and its own child, and
        // children of an unloaded or non-launch parent.
        pane.upsertItem(childItem(threadId, {
          id: 'text', itemIndex: 30, kind: 'assistant_text', toolName: '', status: 'streaming', summary: 'par', updatedAt: 1,
        }));
        pane.applyItemDelta({ threadId, itemId: 'text', parentId: 'anchor', kind: 'assistant_text', delta: 'tial', updatedAt: 2 });
        pane.applyItemDelta({ threadId, itemId: 'think', parentId: 'anchor', kind: 'thinking', delta: 'hmm', updatedAt: 2 });
        pane.applyItemPatch({
          threadId, itemId: 'text', parentId: 'anchor', kind: 'assistant_text',
          patch: { rev: 3, status: 'completed', summary: 'partial', updatedAt: 3 },
        });
        pane.upsertItem(childItem(threadId, { id: 'nested', itemIndex: 31, toolName: 'Task', status: 'running', summary: 'Task: nested' }));
        pane.upsertItem(childItem(threadId, { id: 'grandchild', itemIndex: 32, parentId: 'nested', summary: 'deep work' }));
        pane.upsertItem(childItem(threadId, { id: 'stray', itemIndex: 33, parentId: 'missing' }));
        pane.upsertItem(childItem(threadId, { id: 'flat-child', itemIndex: 34, parentId: 'pre' }));

        expect(pane.items).toBe(itemsBefore);
        expect(pane.items.map((it) => it.id)).toEqual(['pre', 'anchor']);
        expect(pane.timelineRevision).toBe(revisionBefore);
        expect(pane.debugMemoryStats()).toEqual(memory);
        expect(warn).not.toHaveBeenCalled();
      } finally {
        warn.mockRestore();
      }
    });

    it('keeps children out of the window whether the card is expanded or collapsed', async () => {
      const pane = await paneWithAnchor('children-collapse');
      let index = 1;
      for (const expanded of [true, false, true]) {
        expect(pane.toggleSubagentGroupExpanded('anchor')).toBe(expanded);
        pane.upsertItem(childItem('children-collapse', { id: `child-${index}`, itemIndex: index, summary: 'ran tests' }));
        expect(pane.items.some(it => it.parentId === 'anchor')).toBe(false);
        index += 1;
      }
    });

    it('drops a child riding a rolled-back revert that restores its anchor', async () => {
      const pane = await paneWithAnchor('children-revert');
      const removed = pane.removeItemsFromTurn(1, pane.threadId!);
      expect(removed.map((it) => it.id)).toEqual(['anchor']);

      // A rolled-back revert re-inserts the turn through `upsertItems`.
      pane.upsertItems([...removed, childItem('children-revert', { status: 'streaming', updatedAt: 3 })]);
      expect(pane.items.map((it) => it.id)).toEqual(['pre', 'anchor']);
    });

    it('never counts children toward the window cap', async () => {
      const pane = createThreadPane();
      const initial = [
        launchItem('children-cap', { turnIndex: 0 }),
        ...Array.from({ length: MAX - 1 }, (_, index) =>
          makeItem({
            id: `t${index + 1}`,
            threadId: 'children-cap',
            turnIndex: index + 1,
            itemIndex: 0,
          }),
        ),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: MAX - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'children-cap' }));

      pane.upsertItem(childItem('children-cap', { turnIndex: 0 }));
      expect(pane.items).toHaveLength(MAX);

      pane.upsertItem(
        makeItem({ id: `t${MAX}`, threadId: 'children-cap', turnIndex: MAX, itemIndex: 0 }),
      );

      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items.some((it) => it.id === 'anchor')).toBe(false);
    });

    it('a tray window survives host pruning without retaining an island in the host', async () => {
      const threadId = 'fold-held';
      const pane = createThreadPane();
      const launch = launchItem(threadId, { turnIndex: 0, status: 'completed' });
      const children = Array.from({ length: 3 }, (_, index) =>
        childItem(threadId, { id: `child-${index}`, turnIndex: 0, itemIndex: index + 1 }),
      );
      const hostPage = [
        launch,
        ...Array.from({ length: MAX - 1 }, (_, index) =>
          makeItem({ id: `t${index + 1}`, threadId, turnIndex: index + 1, itemIndex: 0 }),
        ),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: hostPage,
        oldestTurnIndex: 0,
        newestTurnIndex: MAX - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: threadId }));
      installPaneMocks([launch, ...children, ...hostPage.slice(1)]);
      const view = createAgentScopeView(pane, 'anchor', { viewKey: 'tray', toolsOnly: true, openAgentPane: () => {} });
      view.start();
      await vi.waitFor(() => expect(view.pane.loading).toBe(false));
      expect(view.items.map(item => item.id)).toEqual(children.map(item => item.id));

      const settle = (turnIndex: number) => {
        pane.setActiveTurn({ turnId: `turn-${turnIndex}`, turnIndex, startedAt: 1 });
        pane.upsertItem(makeItem({ id: `t${turnIndex}`, threadId, turnIndex, itemIndex: 0 }));
        pane.settleTurn({
          turnId: `turn-${turnIndex}`,
          turnIndex,
          startedAt: 1,
          completedAt: 2,
          stopReason: 'end_turn',
          assistantMessageId: null,
          tokenUsage: null,
          aborted: false,
          errorMessage: '',
        });
      };
      settle(MAX);

      expect(pane.items.some(item => item.id === 'anchor')).toBe(false);
      expect(pane.items).toHaveLength(TARGET);
      expect(pane.hasMoreHistory).toBe(true);
      expect(view.root?.id).toBe('anchor');
      expect(view.items.map(item => item.id)).toEqual(children.map(item => item.id));
      view.dispose();
      expect(view.items).toEqual([]);
    });

    it('opens an out-of-window scope without changing the host cursors or revert floor', async () => {
      const threadId = 'fold-island';
      const pane = createThreadPane();
      const items = Array.from({ length: 3 }, (_, index) =>
        makeItem({ id: `t${index + 5}`, threadId, turnIndex: index + 5, itemIndex: 0 }));
      installPaneMocks(items);
      await pane.switchThread(makeThread({ id: threadId }));
      const launch = launchItem(threadId, { turnIndex: 0, status: 'completed' });
      const child = childItem(threadId, { turnIndex: 6, itemIndex: 4 });
      installPaneMocks([launch, child]);
      const view = createAgentScopeView(pane, 'anchor', { viewKey: 'tray', openAgentPane: () => {} });
      view.start();
      await vi.waitFor(() => expect(view.pane.loading).toBe(false));
      expect(view.items.map(item => item.id)).toEqual(['child-1']);
      expect(pane.items.map(item => item.id)).toEqual(['t5', 't6', 't7']);
      expect(pane.oldestLoadedCursor?.turnIndex).toBe(5);
      pane.removeRevertedItems(7, []);
      expect(pane.items.map(item => item.id)).toEqual(['t5', 't6']);
      expect(pane.oldestLoadedCursor?.turnIndex).toBe(5);
      view.dispose();
      expect(pane.items.map(item => item.id)).toEqual(['t5', 't6']);
    });

    it('defers the recent-window prune while a turn is active and runs it on settle', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: MAX }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'fold-defer',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: MAX - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-defer' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: MAX, startedAt: 1 });

      // Mid-turn growth past the cap: a head-drop here repaints the
      // visible timeline (incident 2026-06-10), so the prune waits.
      pane.upsertItem(
        makeItem({ id: `t${MAX}`, threadId: 'fold-defer', turnIndex: MAX, itemIndex: 0 }),
      );
      expect(pane.items).toHaveLength(MAX + 1);

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: MAX,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });

      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe(`t${MAX + 1 - TARGET}`);
      expect(pane.hasMoreHistory).toBe(true);
    });

    it('records the settle prune as pending and runs it inside the transaction on retry', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: MAX }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'prune-rebase',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: MAX - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'prune-rebase' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: MAX, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: `t${MAX}`,
          threadId: 'prune-rebase',
          turnIndex: MAX,
          itemIndex: 0,
        }),
      );

      let itemCountDuringGuard = 0;
      const canPreserveTimelineWindow = vi.fn((keepsItem: (itemId: string) => boolean) => {
        itemCountDuringGuard = pane.items.length;
        expect(keepsItem(`t${MAX - TARGET}`)).toBe(false);
        expect(keepsItem(`t${MAX + 1 - TARGET}`)).toBe(true);
        return true;
      });
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: MAX,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });

      // Wire settle is not visual quiet: a pane with a mounted timeline
      // (the controller offers the anchor transaction) records the prune
      // as pending for the quiet scheduler instead of repainting the
      // head-drop into the reveal drain's glide.
      expect(canPreserveTimelineWindow).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(MAX + 1);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.retryDeferredRecentWindowPrune();

      expect(canPreserveTimelineWindow).toHaveBeenCalledTimes(1);
      expect(itemCountDuringGuard).toBe(MAX + 1);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe(`t${MAX + 1 - TARGET}`);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('a retry landing while the next turn already streams keeps the prune pending', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: MAX }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'prune-next-turn',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: MAX - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'prune-next-turn' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: MAX, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: `t${MAX}`,
          threadId: 'prune-next-turn',
          turnIndex: MAX,
          itemIndex: 0,
        }),
      );

      const canPreserveTimelineWindow = vi.fn(() => true);
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: MAX,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      // The user fires the next turn before quiet ever arrives: the
      // retry must stand down (mid-stream head-drops are banned —
      // incident 2026-06-10) but keep the debt recorded for the next
      // quiet window.
      pane.setActiveTurn({ turnId: 'turn-801', turnIndex: MAX + 1, startedAt: 3 });
      pane.retryDeferredRecentWindowPrune();

      expect(canPreserveTimelineWindow).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(MAX + 1);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.settleTurn({
        turnId: 'turn-801',
        turnIndex: MAX + 1,
        startedAt: 3,
        completedAt: 4,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });
      pane.retryDeferredRecentWindowPrune();

      expect(canPreserveTimelineWindow).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('defers a recent-window prune when the scroll-controller cannot preserve the visible anchor', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: MAX }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'prune-veto',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: MAX - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'prune-veto' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: MAX, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: `t${MAX}`,
          threadId: 'prune-veto',
          turnIndex: MAX,
          itemIndex: 0,
        }),
      );

      const canPreserveTimelineWindow = vi.fn(() => false);
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: MAX,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      // The quiet retry runs into the anchor veto: the reader is parked
      // on a row the prune would drop, so the window stays and the debt
      // stays recorded.
      pane.retryDeferredRecentWindowPrune();

      expect(canPreserveTimelineWindow).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(MAX + 1);
      expect(pane.items[0].id).toBe('t0');
      expect(pane.hasMoreHistory).toBe(false);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      const retryPreserve = vi.fn(() => true);
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow: retryPreserve }),
      );

      pane.retryDeferredRecentWindowPrune();

      expect(retryPreserve).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe(`t${MAX + 1 - TARGET}`);
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('keeps prune mutation ownership in the pane after the viewport guard approves it', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: MAX }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'prune-missing-run',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: MAX - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'prune-missing-run' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: MAX, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: `t${MAX}`,
          threadId: 'prune-missing-run',
          turnIndex: MAX,
          itemIndex: 0,
        }),
      );

      const canPreserveTimelineWindow = vi.fn(() => true);
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: MAX,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.retryDeferredRecentWindowPrune();

      expect(canPreserveTimelineWindow).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe(`t${MAX + 1 - TARGET}`);
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('prunes mid-turn anyway once the hard ceiling is exceeded', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: MAX }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'fold-ceiling',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: MAX - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-ceiling' }));
      pane.setActiveTurn({ turnId: 'turn-x', turnIndex: MAX, startedAt: 1 });

      // Grow to exactly the ceiling — still deferred.
      pane.upsertItems(
        Array.from({ length: CEILING - MAX }, (_, index) =>
          makeItem({
            id: `t${MAX + index}`,
            threadId: 'fold-ceiling',
            turnIndex: MAX + index,
            itemIndex: 0,
          }),
        ),
      );
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS);

      // One more row breaches the ceiling: the cut no longer waits for
      // settle. No controller is attached, so the tail is the anchor.
      pane.upsertItem(
        makeItem({ id: `t${CEILING}`, threadId: 'fold-ceiling', turnIndex: CEILING, itemIndex: 0 }),
      );
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items.at(-1)?.id).toBe(`t${CEILING}`);
      expect(pane.hasMoreHistory).toBe(true);
    });

    it('keeps a ceiling-breaching window whole when the viewport guard vetoes the cut', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: MAX }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'fold-ceiling-veto',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: MAX - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-ceiling-veto' }));
      pane.setActiveTurn({ turnId: 'turn-x', turnIndex: MAX, startedAt: 1 });

      const canPreserveTimelineWindow = vi.fn(() => false);
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow }),
      );

      pane.upsertItems(
        Array.from({ length: CEILING - MAX }, (_, index) =>
          makeItem({
            id: `t${MAX + index}`,
            threadId: 'fold-ceiling-veto',
            turnIndex: MAX + index,
            itemIndex: 0,
          }),
        ),
      );
      expect(canPreserveTimelineWindow).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(CEILING);

      pane.upsertItem(
        makeItem({
          id: `t${CEILING}`,
          threadId: 'fold-ceiling-veto',
          turnIndex: CEILING,
          itemIndex: 0,
        }),
      );

      // The ceiling ends the settle deferral, not the visible-row rule: a
      // cut the viewport cannot survive stays pending at any count.
      expect(canPreserveTimelineWindow).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(CEILING + 1);
      expect(pane.items[0].id).toBe('t0');
      expect(pane.hasMoreHistory).toBe(false);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      // Once the viewport can hold its rows through the cut, the retry
      // applies it around the tail.
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow: () => true }),
      );
      pane.retryDeferredRecentWindowPrune();
      expect(pane.items).toHaveLength(TARGET);
      expect(pane.items[0].id).toBe(`t${CEILING + 1 - TARGET}`);
      expect(pane.items.at(-1)?.id).toBe(`t${CEILING}`);
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('a settled deferred cut keeps waiting past the ceiling while the viewport guard vetoes it', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: MAX }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'settled-ceiling-veto',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: MAX - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'settled-ceiling-veto' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: MAX, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: `t${MAX}`,
          threadId: 'settled-ceiling-veto',
          turnIndex: MAX,
          itemIndex: 0,
        }),
      );

      const canPreserveTimelineWindow = vi.fn(() => false);
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: MAX,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });
      expect(canPreserveTimelineWindow).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(MAX + 1);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.upsertItem(
        makeItem({
          id: `t${MAX + 1}`,
          threadId: 'settled-ceiling-veto',
          turnIndex: MAX + 1,
          itemIndex: 0,
        }),
      );
      expect(canPreserveTimelineWindow).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(MAX + 2);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.upsertItems(
        Array.from({ length: CEILING - MAX - 1 }, (_, index) =>
          makeItem({
            id: `t${MAX + 2 + index}`,
            threadId: 'settled-ceiling-veto',
            turnIndex: MAX + 2 + index,
            itemIndex: 0,
          }),
        ),
      );

      // Past the ceiling the cut is attempted on the append path and the
      // guard vetoes it: the rows stay, the debt stays recorded.
      expect(canPreserveTimelineWindow).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(CEILING + 1);
      expect(pane.items[0].id).toBe('t0');
      expect(pane.hasMoreHistory).toBe(false);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);
    });
  });
});
