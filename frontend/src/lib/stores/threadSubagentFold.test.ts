// stores/threadSubagentFold.test.ts
//
// utils/subagentFold.ts through the pane: settled subagent children evict
// into a per-anchor fold and rehydrate on expansion, which is how frontend
// memory stays bounded by the visible thread.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createThreadPane } from './thread.svelte';
import { type Item } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { makeItem, makeThread, stubScrollController } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import {
  ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
  ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
} from './threadPaneShared';
import { MAX_CACHED_SNAPSHOT_CHARS } from './threadItemCache';

describe('subagent fold', () => {
  beforeEach(installThreadPaneTestEnv);

  describe('subagent live eviction (fold)', () => {
    // Live turns stream subagent child rows into pane memory; once a
    // child settles and nothing can render it (collapsed inline card,
    // suppressed background launch), the pane drops the row and folds
    // its count/preview into the per-anchor registry. SQLite keeps the
    // canonical rows (triage persists before emitting), so expansion
    // re-hydrates through ListSubagentDescendants.
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

    it('evicts a terminal child of a collapsed inline card into the fold', async () => {
      const pane = await paneWithAnchor('fold-evict');

      pane.upsertItem(childItem('fold-evict'));

      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')).toEqual({
        evictedCount: 1,
        terminalPreview: 'ran the build',
        terminalTurnIndex: 1,
        terminalItemIndex: 1,
      });

      // A replayed upsert for the folded id (transport reconnect echo)
      // must not re-insert the row or double-count it.
      pane.upsertItem(childItem('fold-evict'));
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);
    });

    it('releases only the evicted rows\' UI state, and keeps a shared payload alive', async () => {
      // The drop chokepoint splits the window in one pass and hands the
      // dropped rows straight to disposal, so the released set and the
      // surviving array come from the same walk. This is the transition
      // that catches a mismatch: two rows share a payload, and its UI
      // state must survive the first eviction and die with the second.
      const pane = await paneWithAnchor('fold-shared-payload');
      const child = (id: string, itemIndex: number, status: Item['status']) =>
        childItem('fold-shared-payload', {
          id, itemIndex, status, payloadId: 'payload-p', kind: 'tool_call',
        });
      pane.upsertItem(child('child-1', 1, 'streaming'));
      pane.upsertItem(child('child-2', 2, 'streaming'));
      pane.expansionStateFor(pane.items.find((it) => it.id === 'child-1')!);
      pane.expansionStateForPayload('payload-p', 'fold-shared-payload');
      expect(pane.debugMemoryStats().rowUiState.itemExpansionStates).toBe(1);
      expect(pane.debugMemoryStats().rowUiState.payloadExpansionStates).toBe(1);

      pane.upsertItem(child('child-2', 2, 'completed'));
      expect(pane.items.some((it) => it.id === 'child-2')).toBe(false);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      // child-1 still points at the payload, so neither registry moves.
      expect(pane.debugMemoryStats().rowUiState.itemExpansionStates).toBe(1);
      expect(pane.debugMemoryStats().rowUiState.payloadExpansionStates).toBe(1);

      pane.upsertItem(child('child-1', 1, 'completed'));
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.debugMemoryStats().rowUiState.itemExpansionStates).toBe(0);
      expect(pane.debugMemoryStats().rowUiState.payloadExpansionStates).toBe(0);
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(2);
    });

    it('does not touch the window when a settling batch evicts nothing', async () => {
      const pane = await paneWithAnchor('fold-noop');
      pane.upsertItem(childItem('fold-noop', { id: 'child-1', status: 'streaming' }));
      const revisionBefore = pane.timelineRevision;
      const itemsBefore = pane.items;

      // Settled, but not a descendant of the collapsed card — nothing to
      // evict, so the drop must not replace the array or bump a revision.
      pane.upsertItem(makeItem({
        id: 'top-level', threadId: 'fold-noop', turnIndex: 1, itemIndex: 9,
        status: 'completed',
      }));

      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      expect(pane.items).not.toBe(itemsBefore);
      // One bump for the append itself, none for a no-op drop.
      expect(pane.timelineRevision).toBe(revisionBefore + 1);
    });

    it('keeps a streaming child in memory and evicts it when it settles', async () => {
      const pane = await paneWithAnchor('fold-streaming');

      pane.upsertItem(
        childItem('fold-streaming', { status: 'streaming', summary: 'working...' }),
      );
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();

      pane.upsertItem(childItem('fold-streaming', { summary: 'finished the build' }));
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')?.terminalPreview).toBe(
        'finished the build',
      );
    });

    it('evicts a child settled by a wire status patch (streaming-text settle shape)', async () => {
      const pane = await paneWithAnchor('fold-patch');

      pane.upsertItem(
        childItem('fold-patch', {
          kind: 'assistant_text',
          toolName: '',
          status: 'streaming',
          summary: 'partial',
        }),
      );
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);

      // Streaming text/thinking rows settle via triage field patches,
      // not upserts — the eviction policy must cover this path too.
      pane.applyItemPatch({
        threadId: 'fold-patch',
        itemId: 'child-1',
        kind: 'assistant_text',
        patch: { status: 'completed', summary: 'full text', updatedAt: 2 },
      });

      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')).toEqual({
        evictedCount: 1,
        terminalPreview: '',
        terminalTurnIndex: -1,
        terminalItemIndex: -1,
      });
    });

    it('retains settled children while the card is expanded and evicts them on collapse', async () => {
      const pane = await paneWithAnchor('fold-collapse');

      expect(pane.toggleSubagentGroupExpanded('anchor')).toBe(true);
      pane.upsertItem(childItem('fold-collapse', { summary: 'ran tests' }));
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();

      expect(pane.toggleSubagentGroupExpanded('anchor')).toBe(false);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')).toEqual({
        evictedCount: 1,
        terminalPreview: 'ran tests',
        terminalTurnIndex: 1,
        terminalItemIndex: 1,
      });

      // Re-expansion hydrates from SQLite and reclaims the fold — the
      // id is folded XOR loaded, never both.
      setBindingMock('ListSubagentDescendants', async () => [
        childItem('fold-collapse', { summary: 'ran tests' }),
      ]);
      expect(pane.toggleSubagentGroupExpanded('anchor')).toBe(true);
      await expect(pane.ensureSubagentChildren('anchor')).resolves.toBe(true);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
    });

    it('sweeps the settled subtree through a nested launch when the outer card collapses', async () => {
      const pane = await paneWithAnchor('fold-backgrounded');
      pane.toggleSubagentGroupExpanded('anchor');
      pane.upsertItem(childItem('fold-backgrounded'));
      // Nested expanded launch with a settled grandchild — both retained
      // while the foreground cards are open.
      pane.upsertItem(
        childItem('fold-backgrounded', {
          id: 'nested',
          itemIndex: 2,
          kind: 'tool_call',
          toolName: 'Task',
          status: 'running',
          summary: 'Task: nested',
        }),
      );
      pane.toggleSubagentGroupExpanded('nested');
      pane.upsertItem(
        childItem('fold-backgrounded', {
          id: 'grandchild',
          itemIndex: 3,
          parentId: 'nested',
          summary: 'deep work',
        }),
      );
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      expect(pane.items.some((it) => it.id === 'grandchild')).toBe(true);

      // Collapsing the OUTER card makes the whole transcript unrenderable,
      // the nested card included — its own expansion no longer reaches a
      // reader. The sweep resolves the chain through the nested launch:
      // nested launches stay loaded as fold keys and cards, and their
      // settled children fold under their own anchor so nested entry
      // counters stay honest.
      expect(pane.toggleSubagentGroupExpanded('anchor')).toBe(false);

      expect(pane.items.some((it) => it.id === 'anchor')).toBe(true);
      expect(pane.items.some((it) => it.id === 'nested')).toBe(true);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.items.some((it) => it.id === 'grandchild')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);
      expect(pane.subagentLiveAggregate('nested')?.evictedCount).toBe(1);
    });

    it('folds terminal children of a collapsed background anchor while keeping streaming ones', async () => {
      const pane = await paneWithAnchor(
        'fold-suppressed',
        launchItem('fold-suppressed', { isBackground: true }),
      );

      pane.upsertItem(
        childItem('fold-suppressed', { status: 'streaming', summary: 'live' }),
      );
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);

      pane.upsertItem(
        childItem('fold-suppressed', {
          id: 'child-2',
          itemIndex: 2,
          summary: 'done already',
        }),
      );
      expect(pane.items.some((it) => it.id === 'child-2')).toBe(false);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);
    });

    it('never folds rows whose parent is loaded but not a launch', async () => {
      const pane = await paneWithAnchor('fold-flat');

      // Parent not loaded → nothing can render it, so it never enters
      // pane memory at all (see threadSubagentMemory.test.ts).
      pane.upsertItem(
        childItem('fold-flat', { id: 'stray', itemIndex: 5, parentId: 'missing' }),
      );
      // Parent loaded but not a launch → flat leaf, stays.
      pane.upsertItem(
        childItem('fold-flat', { id: 'flat-child', itemIndex: 6, parentId: 'pre' }),
      );

      expect(pane.items.some((it) => it.id === 'stray')).toBe(false);
      expect(pane.items.some((it) => it.id === 'flat-child')).toBe(true);
      expect(pane.subagentLiveAggregate('missing')).toBeUndefined();
      expect(pane.subagentLiveAggregate('pre')).toBeUndefined();
    });

    it('drops the fold with its anchor on revert so re-upserts are not swallowed', async () => {
      const pane = await paneWithAnchor('fold-revert');
      pane.upsertItem(childItem('fold-revert'));
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);

      const removed = pane.removeItemsFromTurn(1);

      expect(removed.map((it) => it.id)).toEqual(['anchor']);
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
      // The backend truncate deleted the child's row too; if the same ids
      // arrive again (a rolled-back revert re-inserts the turn through
      // `upsertItems`) they must land in pane memory instead of being
      // treated as a folded echo. The anchor leads the restore batch, so
      // the child is admitted with it; the card is expanded so the
      // settled child is retained rather than immediately re-folded,
      // making its presence a clean signal that nothing swallowed it.
      pane.toggleSubagentGroupExpanded('anchor');
      pane.upsertItems([...removed, childItem('fold-revert')]);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
    });

    it('carries folds through the thread-switch snapshot cache', async () => {
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
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);

      // Folds belong to the thread — they must not leak into the next one.
      await pane.switchThread(makeThread({ id: 'fold-cache-b' }));
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();

      // Warm re-entry restores the fold with the cached window, so the
      // collapsed card's count survives without any live event.
      await pane.switchThread(makeThread({ id: 'fold-cache-a' }));
      expect(pane.subagentLiveAggregate('anchor')).toEqual({
        evictedCount: 1,
        terminalPreview: 'ran the build',
        terminalTurnIndex: 1,
        terminalItemIndex: 1,
      });
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
    });

    it('drops a fold when the window prune drops its anchor', async () => {
      const pane = createThreadPane();
      const initial = [
        launchItem('fold-prune', { turnIndex: 0 }),
        ...Array.from({ length: 799 }, (_, index) =>
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
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-prune' }));

      pane.upsertItem(childItem('fold-prune', { turnIndex: 0 }));
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);
      // Folded children no longer count toward the window cap.
      expect(pane.items).toHaveLength(800);

      pane.upsertItem(
        makeItem({ id: 't800', threadId: 'fold-prune', turnIndex: 800, itemIndex: 0 }),
      );

      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items.some((it) => it.id === 'anchor')).toBe(false);
      // Folds are only meaningful while their anchor row is loaded —
      // the next load of that region decorates anchors from SQLite.
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
    });

    it('clears folds on re-entry when the outgoing snapshot was too large to cache', async () => {
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
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);

      await pane.switchThread(makeThread({ id: 'fold-reject-other' }));
      await pane.switchThread(makeThread({ id: 'fold-reject' }));

      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
      // A stale fold would swallow this re-streamed row outright; the
      // fresh-state clear lets it land (streaming rows always stay).
      pane.upsertItem(
        childItem('fold-reject', {
          turnIndex: 0,
          status: 'streaming',
          summary: 'live again',
        }),
      );
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
    });

    // Removal paths drop rows the reader asked to destroy, and those rows
    // can be hydrated subagent children whose launch ANCHOR survives the
    // same drop — a revert keeps the anchor turn's backend-enumerated
    // survivors, and a single-row removal keeps everything else by
    // construction. A surviving anchor still marked exhausted never
    // re-fetches, so its card wedges on the loading placeholder. These
    // paths hand-rolled their disposal and skipped the re-arm entirely;
    // they go through `dropTimelineItems` now. Transition coverage: the
    // marker has to be SET first, or the assertion passes vacuously.
    async function paneWithExhaustedAnchor(threadId: string) {
      const pane = await paneWithAnchor(threadId);
      let listCalls = 0;
      setBindingMock('ListSubagentDescendants', async () => {
        listCalls += 1;
        return [childItem(threadId)];
      });

      // First fetch merges the child in; the second finds nothing new and
      // marks the anchor exhausted; the third proves the marker bites.
      expect(await pane.ensureSubagentChildren('anchor')).toBe(true);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      expect(await pane.ensureSubagentChildren('anchor')).toBe(false);
      expect(await pane.ensureSubagentChildren('anchor')).toBe(false);
      expect(listCalls).toBe(2);

      return { pane, calls: () => listCalls };
    }

    it('re-arms a surviving anchor when removeItemById drops its hydrated child', async () => {
      const { pane, calls } = await paneWithExhaustedAnchor('remove-one-exhaust');

      const removed = pane.removeItemById('child-1', 'remove-one-exhaust');
      expect(removed?.id).toBe('child-1');
      expect(pane.items.some((it) => it.id === 'anchor')).toBe(true);

      // The anchor is hydratable again: the fetch goes out and the child
      // comes back, instead of being suppressed by a marker describing a
      // window that no longer exists.
      expect(await pane.ensureSubagentChildren('anchor')).toBe(true);
      expect(calls()).toBe(3);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
    });

    it('re-arms a surviving anchor when a revert drops its hydrated child', async () => {
      const { pane, calls } = await paneWithExhaustedAnchor('remove-revert-exhaust');

      // The anchor turn's survivor list names the anchor only — the
      // hydrated child was never in the backend enumeration, so the
      // kept-set formulation removes it while the anchor stays.
      const removed = pane.removeRevertedItems(1, ['anchor']);
      expect(removed.map((it) => it.id)).toEqual(['child-1']);
      expect(pane.items.map((it) => it.id)).toEqual(['pre', 'anchor']);

      expect(await pane.ensureSubagentChildren('anchor')).toBe(true);
      expect(calls()).toBe(3);
    });

    it('keeps unrelated exhausted-hydration markers across evictions', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          launchItem('fold-exhaust', { id: 'anchor-a', turnIndex: 0 }),
          launchItem('fold-exhaust', { id: 'anchor-b', turnIndex: 1 }),
        ],
        oldestTurnIndex: 0,
        newestTurnIndex: 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      let listCalls = 0;
      setBindingMock('ListSubagentDescendants', async () => {
        listCalls += 1;
        return [];
      });
      await pane.switchThread(makeThread({ id: 'fold-exhaust' }));

      // Anchor A fetches nothing → marked exhausted; repeats skip the wire.
      await pane.ensureSubagentChildren('anchor-a');
      await pane.ensureSubagentChildren('anchor-a');
      expect(listCalls).toBe(1);

      // Evicting a child of anchor B clears only B's marker. A wholesale
      // clear here would re-arm A into a refetch per eviction.
      pane.upsertItem(
        childItem('fold-exhaust', { parentId: 'anchor-b', turnIndex: 1 }),
      );
      expect(pane.subagentLiveAggregate('anchor-b')?.evictedCount).toBe(1);
      await pane.ensureSubagentChildren('anchor-a');
      expect(listCalls).toBe(1);

      // B's own transcript changed, so its fetch goes through.
      await pane.ensureSubagentChildren('anchor-b');
      expect(listCalls).toBe(2);
    });

    it('defers the recent-window prune while a turn is active and runs it on settle', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
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
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-defer' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: 800, startedAt: 1 });

      // Mid-turn growth past the cap: a head-drop here repaints the
      // visible timeline (incident 2026-06-10), so the prune waits.
      pane.upsertItem(
        makeItem({ id: 't800', threadId: 'fold-defer', turnIndex: 800, itemIndex: 0 }),
      );
      expect(pane.items).toHaveLength(801);

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: 800,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });

      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe('t301');
      expect(pane.hasMoreHistory).toBe(true);
    });

    it('records the settle prune as pending and runs it inside the transaction on retry', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
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
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'prune-rebase' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: 800, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: 't800',
          threadId: 'prune-rebase',
          turnIndex: 800,
          itemIndex: 0,
        }),
      );

      let itemCountDuringGuard = 0;
      const canPreserveTimelineWindow = vi.fn((keepsItem: (itemId: string) => boolean) => {
        itemCountDuringGuard = pane.items.length;
        expect(keepsItem('t300')).toBe(false);
        expect(keepsItem('t301')).toBe(true);
        return true;
      });
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: 800,
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
      expect(pane.items).toHaveLength(801);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.retryDeferredRecentWindowPrune();

      expect(canPreserveTimelineWindow).toHaveBeenCalledTimes(1);
      expect(itemCountDuringGuard).toBe(801);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe('t301');
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('a retry landing while the next turn already streams keeps the prune pending', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
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
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'prune-next-turn' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: 800, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: 't800',
          threadId: 'prune-next-turn',
          turnIndex: 800,
          itemIndex: 0,
        }),
      );

      const canPreserveTimelineWindow = vi.fn(() => true);
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: 800,
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
      pane.setActiveTurn({ turnId: 'turn-801', turnIndex: 801, startedAt: 3 });
      pane.retryDeferredRecentWindowPrune();

      expect(canPreserveTimelineWindow).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(801);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.settleTurn({
        turnId: 'turn-801',
        turnIndex: 801,
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
      const initial = Array.from({ length: 800 }, (_, index) =>
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
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'prune-veto' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: 800, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: 't800',
          threadId: 'prune-veto',
          turnIndex: 800,
          itemIndex: 0,
        }),
      );

      const canPreserveTimelineWindow = vi.fn(() => false);
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: 800,
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
      expect(pane.items).toHaveLength(801);
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
      expect(pane.items[0].id).toBe('t301');
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('keeps prune mutation ownership in the pane after the viewport guard approves it', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
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
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'prune-missing-run' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: 800, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: 't800',
          threadId: 'prune-missing-run',
          turnIndex: 800,
          itemIndex: 0,
        }),
      );

      const canPreserveTimelineWindow = vi.fn(() => true);
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: 800,
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
      expect(pane.items[0].id).toBe('t301');
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('prunes mid-turn anyway once the hard ceiling is exceeded', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
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
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-ceiling' }));
      pane.setActiveTurn({ turnId: 'turn-x', turnIndex: 800, startedAt: 1 });

      // Grow to exactly the ceiling — still deferred.
      pane.upsertItems(
        Array.from({ length: 800 }, (_, index) =>
          makeItem({
            id: `t${800 + index}`,
            threadId: 'fold-ceiling',
            turnIndex: 800 + index,
            itemIndex: 0,
          }),
        ),
      );
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS);

      // One more row breaches the ceiling: memory wins over the repaint.
      pane.upsertItem(
        makeItem({ id: 't1600', threadId: 'fold-ceiling', turnIndex: 1600, itemIndex: 0 }),
      );
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items.at(-1)?.id).toBe('t1600');
      expect(pane.hasMoreHistory).toBe(true);
    });

    it('forces the hard-ceiling prune when visible-anchor preservation vetoes it', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
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
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-ceiling-veto' }));
      pane.setActiveTurn({ turnId: 'turn-x', turnIndex: 800, startedAt: 1 });

      const canPreserveTimelineWindow = vi.fn(() => false);
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow }),
      );

      pane.upsertItems(
        Array.from({ length: 800 }, (_, index) =>
          makeItem({
            id: `t${800 + index}`,
            threadId: 'fold-ceiling-veto',
            turnIndex: 800 + index,
            itemIndex: 0,
          }),
        ),
      );
      expect(canPreserveTimelineWindow).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS);

      pane.upsertItem(
        makeItem({
          id: 't1600',
          threadId: 'fold-ceiling-veto',
          turnIndex: 1600,
          itemIndex: 0,
        }),
      );

      expect(canPreserveTimelineWindow).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe('t1101');
      expect(pane.items.at(-1)?.id).toBe('t1600');
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('forces the hard-ceiling prune after a settled deferred prune keeps growing', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
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
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'settled-ceiling-veto' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: 800, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: 't800',
          threadId: 'settled-ceiling-veto',
          turnIndex: 800,
          itemIndex: 0,
        }),
      );

      const canPreserveTimelineWindow = vi.fn(() => false);
      pane.attachScrollController(
        stubScrollController({ canPreserveTimelineWindow }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: 800,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });
      expect(canPreserveTimelineWindow).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(801);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.upsertItem(
        makeItem({
          id: 't801',
          threadId: 'settled-ceiling-veto',
          turnIndex: 801,
          itemIndex: 0,
        }),
      );
      expect(canPreserveTimelineWindow).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(802);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.upsertItems(
        Array.from({ length: 799 }, (_, index) =>
          makeItem({
            id: `t${802 + index}`,
            threadId: 'settled-ceiling-veto',
            turnIndex: 802 + index,
            itemIndex: 0,
          }),
        ),
      );

      expect(canPreserveTimelineWindow).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe('t1101');
      expect(pane.items.at(-1)?.id).toBe('t1600');
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });
  });
});
