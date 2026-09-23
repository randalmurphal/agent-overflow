// stores/threadSubagentFold.test.ts
//
// utils/subagentFold.ts through the pane: streamed subagent children never
// enter the pane window; each launch anchor's live aggregate records them
// for its collapsed card, and scoped surfaces load the rows themselves.

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
import { MAX_CACHED_SNAPSHOT_CHARS } from './threadItemCache';

describe('subagent fold', () => {
  beforeEach(() => {
    installTimelineScopeCapability();
    installThreadPaneTestEnv();
    __resetAgentPaneStateForTest();
  });

  describe('subagent live aggregates', () => {
    // Live turns stream subagent child rows. The pane routes each one at
    // admission to its launch anchor's aggregate (count, newest active and
    // terminal previews) and never holds the row. SQLite keeps the
    // canonical rows, so an expanded card or the agent pane loads them
    // through a scoped surface.
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

    it('records a settled child in the anchor aggregate without touching the window', async () => {
      const pane = await paneWithAnchor('fold-admit');
      const itemsBefore = pane.items;
      const revisionBefore = pane.timelineRevision;

      pane.upsertItem(childItem('fold-admit'));

      expect(pane.items).toBe(itemsBefore);
      expect(pane.timelineRevision).toBe(revisionBefore);
      expect(pane.subagentLiveAggregate('anchor')).toEqual({
        count: 1,
        activePreview: '',
        activeTurnIndex: -1,
        activeItemIndex: -1,
        terminalPreview: 'ran the build',
        terminalTurnIndex: 1,
        terminalItemIndex: 1,
      });

      // A replayed upsert (transport reconnect echo) is not counted again.
      pane.upsertItem(childItem('fold-admit'));
      expect(pane.items).toBe(itemsBefore);
      expect(pane.subagentLiveAggregate('anchor')?.count).toBe(1);
    });

    it('admits 1,000 settled children onto 1,000 roots without committing the root list', async () => {
      const threadId = 'fold-cost';
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
      expect(pane.subagentLiveAggregate('root-0')?.count).toBe(100);
      expect(pane.subagentLiveAggregate('root-900')?.terminalPreview).toBe('ran 999');
      expect(elapsed).toBeLessThan(100);
    });

    it('creates no row UI state for children', async () => {
      const pane = await paneWithAnchor('fold-row-ui');
      const before = pane.debugMemoryStats().rowUiState;
      for (let i = 1; i <= 20; i += 1) {
        pane.upsertItem(childItem('fold-row-ui', { id: `child-${i}`, itemIndex: i, payloadId: `payload-${i}` }));
      }
      expect(pane.debugMemoryStats().rowUiState).toEqual(before);
      expect(pane.subagentLiveAggregate('anchor')?.count).toBe(20);
    });

    it('tracks a streaming child and settles it through the upsert that completes it', async () => {
      const pane = await paneWithAnchor('fold-streaming');

      pane.upsertItem(childItem('fold-streaming', { status: 'streaming', summary: 'working...', updatedAt: 1 }));
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')).toMatchObject({ count: 1, activePreview: 'working...' });

      pane.upsertItem(childItem('fold-streaming', { summary: 'finished the build', updatedAt: 2 }));
      expect(pane.subagentLiveAggregate('anchor')).toMatchObject({
        count: 1,
        activePreview: '',
        terminalPreview: 'finished the build',
      });
    });

    it('settles a child through a wire status patch (streaming-text settle shape)', async () => {
      const pane = await paneWithAnchor('fold-patch');
      pane.upsertItem(childItem('fold-patch', { id: 'tool', itemIndex: 1, status: 'running', summary: 'Bash: build', updatedAt: 1 }));
      pane.upsertItem(childItem('fold-patch', {
        id: 'text',
        itemIndex: 2,
        kind: 'assistant_text',
        toolName: '',
        status: 'streaming',
        summary: 'partial',
        updatedAt: 1,
      }));

      // Streaming text rows settle via triage field patches, not upserts.
      pane.applyItemPatch({
        threadId: 'fold-patch',
        itemId: 'text',
        kind: 'assistant_text',
        patch: { rev: 0, status: 'completed', summary: 'full text', updatedAt: 2 },
      });
      pane.applyItemPatch({
        threadId: 'fold-patch',
        itemId: 'tool',
        kind: 'tool_call',
        patch: { rev: 0, status: 'completed', updatedAt: 2 },
      });

      expect(pane.items.some((it) => it.parentId === 'anchor')).toBe(false);
      // Prose never becomes the preview; the settled tool does.
      expect(pane.subagentLiveAggregate('anchor')).toMatchObject({
        count: 2,
        activePreview: '',
        terminalPreview: 'Bash: build',
      });
    });

    it('keeps children out of the window whether the card is expanded or collapsed', async () => {
      const pane = await paneWithAnchor('fold-collapse');
      let index = 1;
      for (const expanded of [true, false, true]) {
        expect(pane.toggleSubagentGroupExpanded('anchor')).toBe(expanded);
        pane.upsertItem(childItem('fold-collapse', { id: `child-${index}`, itemIndex: index, summary: 'ran tests' }));
        expect(pane.items.some(it => it.parentId === 'anchor')).toBe(false);
        expect(pane.subagentLiveAggregate('anchor')?.count).toBe(index);
        index += 1;
      }
    });

    it('counts a nested launch transcript on the outer and the nested card', async () => {
      const pane = await paneWithAnchor('fold-nested');
      pane.upsertItem(childItem('fold-nested'));
      pane.upsertItem(childItem('fold-nested', {
        id: 'nested',
        itemIndex: 2,
        toolName: 'Task',
        status: 'running',
        summary: 'Task: nested',
        updatedAt: 1,
      }));
      pane.upsertItem(childItem('fold-nested', {
        id: 'grandchild',
        itemIndex: 3,
        parentId: 'nested',
        summary: 'deep work',
      }));

      expect(pane.items.map((it) => it.id)).toEqual(['pre', 'anchor']);
      expect(pane.subagentLiveAggregate('anchor')).toMatchObject({
        count: 3,
        activePreview: 'Task: nested',
        terminalPreview: 'deep work',
      });
      expect(pane.subagentLiveAggregate('nested')).toMatchObject({ count: 1, terminalPreview: 'deep work' });
    });

    it('records streaming and settled children of a background anchor', async () => {
      const pane = await paneWithAnchor(
        'fold-background',
        launchItem('fold-background', { isBackground: true }),
      );
      pane.upsertItem(childItem('fold-background', { status: 'streaming', summary: 'live', updatedAt: 1 }));
      pane.upsertItem(childItem('fold-background', { id: 'child-2', itemIndex: 2, summary: 'done already' }));

      expect(pane.items.some((it) => it.parentId === 'anchor')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')).toMatchObject({
        count: 2,
        activePreview: 'live',
        terminalPreview: 'done already',
      });
    });

    it('records no aggregate for a child of an unloaded or non-launch parent', async () => {
      const pane = await paneWithAnchor('fold-flat');
      const itemsBefore = pane.items;

      pane.upsertItem(childItem('fold-flat', { id: 'stray', itemIndex: 5, parentId: 'missing' }));
      pane.upsertItem(childItem('fold-flat', { id: 'flat-child', itemIndex: 6, parentId: 'pre' }));

      expect(pane.items).toBe(itemsBefore);
      expect(pane.subagentLiveAggregate('missing')).toBeUndefined();
      expect(pane.subagentLiveAggregate('pre')).toBeUndefined();
    });

    it('drops the aggregate with its anchor on revert and counts a restored child once', async () => {
      const pane = await paneWithAnchor('fold-revert');
      pane.upsertItem(childItem('fold-revert'));
      expect(pane.subagentLiveAggregate('anchor')?.count).toBe(1);

      const removed = pane.removeItemsFromTurn(1, pane.threadId!);

      expect(removed.map((it) => it.id)).toEqual(['anchor']);
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
      // A rolled-back revert re-inserts the turn through `upsertItems`; the
      // anchor leads the batch, so its child is recorded against it again.
      pane.upsertItems([...removed, childItem('fold-revert', { status: 'streaming', updatedAt: 3 })]);
      expect(pane.items.map((it) => it.id)).toEqual(['pre', 'anchor']);
      expect(pane.subagentLiveAggregate('anchor')).toMatchObject({ count: 1, activePreview: 'ran the build' });
    });

    it('carries aggregates through the thread-switch snapshot cache', async () => {
      const pane = createThreadPane();
      const sliceByThread: Record<string, Item[]> = {
        'fold-cache-a': [
          makeItem({ id: 'pre', threadId: 'fold-cache-a', turnIndex: 0, itemIndex: 0 }),
          launchItem('fold-cache-a'),
        ],
        'fold-cache-b': [
          makeItem({ id: 'b-only', threadId: 'fold-cache-b', turnIndex: 0, itemIndex: 0 }),
        ],
      };
      setBindingMock('ListThreadSliceAround', async (threadId: string) => ({
        items: sliceByThread[threadId] ?? [],
        oldestTurnIndex: 0,
        newestTurnIndex: 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));

      await pane.switchThread(makeThread({ id: 'fold-cache-a' }));
      pane.upsertItem(childItem('fold-cache-a'));
      expect(pane.subagentLiveAggregate('anchor')?.count).toBe(1);

      // Aggregates belong to the thread; they must not leak into the next one.
      await pane.switchThread(makeThread({ id: 'fold-cache-b' }));
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();

      // Warm re-entry restores the aggregate with the cached window, so the
      // collapsed card's count survives without any live event.
      await pane.switchThread(makeThread({ id: 'fold-cache-a' }));
      expect(pane.subagentLiveAggregate('anchor')).toEqual({
        count: 1,
        activePreview: '',
        activeTurnIndex: -1,
        activeItemIndex: -1,
        terminalPreview: 'ran the build',
        terminalTurnIndex: 1,
        terminalItemIndex: 1,
      });
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);

      // A replay of the carried child is not counted again.
      pane.upsertItem(childItem('fold-cache-a'));
      expect(pane.subagentLiveAggregate('anchor')?.count).toBe(1);
    });

    it('drops an aggregate when the window prune drops its anchor', async () => {
      const pane = createThreadPane();
      const initial = [
        launchItem('fold-prune', { turnIndex: 0 }),
        ...Array.from({ length: MAX - 1 }, (_, index) =>
          makeItem({
            id: `t${index + 1}`,
            threadId: 'fold-prune',
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
      await pane.switchThread(makeThread({ id: 'fold-prune' }));

      pane.upsertItem(childItem('fold-prune', { turnIndex: 0 }));
      expect(pane.subagentLiveAggregate('anchor')?.count).toBe(1);
      // Children never count toward the window cap.
      expect(pane.items).toHaveLength(MAX);

      pane.upsertItem(
        makeItem({ id: `t${MAX}`, threadId: 'fold-prune', turnIndex: MAX, itemIndex: 0 }),
      );

      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items.some((it) => it.id === 'anchor')).toBe(false);
      // Aggregates are only meaningful while their anchor row is loaded;
      // the next load of that region decorates anchors from SQLite.
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
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

    it('clears aggregates on re-entry when the outgoing snapshot was too large to cache', async () => {
      const pane = createThreadPane();
      const big = [
        launchItem('fold-reject', { turnIndex: 0 }),
        // Blows MAX_CACHED_SNAPSHOT_CHARS so the switch-away snapshot is
        // rejected and re-entry takes the fresh-state path. (The char
        // budget, not the item cap, keeps the window prune out of play.)
        makeItem({
          id: 'huge',
          threadId: 'fold-reject',
          turnIndex: 1,
          itemIndex: 0,
          summary: 'x'.repeat(MAX_CACHED_SNAPSHOT_CHARS + 1),
        }),
      ];
      setBindingMock('ListThreadSliceAround', async (threadId: string) => ({
        items: threadId === 'fold-reject' ? big : [],
        oldestTurnIndex: 0,
        newestTurnIndex: 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-reject' }));
      pane.upsertItem(childItem('fold-reject', { turnIndex: 0 }));
      expect(pane.subagentLiveAggregate('anchor')?.count).toBe(1);

      await pane.switchThread(makeThread({ id: 'fold-reject-other' }));
      await pane.switchThread(makeThread({ id: 'fold-reject' }));

      // The anchor now comes from SQLite with its decoration; nothing
      // stale survives the fresh-state path.
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
      pane.upsertItem(childItem('fold-reject', {
        turnIndex: 0,
        status: 'streaming',
        summary: 'live again',
        updatedAt: 3,
      }));
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')).toMatchObject({ count: 1, activePreview: 'live again' });
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
