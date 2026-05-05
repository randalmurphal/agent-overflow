import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createThreadPane } from './thread.svelte';
import { getActiveTurn } from './threadStatuses.svelte';
import type { Item } from '../types/models';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { makeItem, makeThread } from '../../test/helpers/chat';

function nextFrame(): Promise<void> {
  return new Promise((resolve) => {
    requestAnimationFrame(() => resolve());
  });
}

describe('createThreadPane', () => {
  beforeEach(() => {
    Object.defineProperty(window, 'innerWidth', {
      configurable: true,
      writable: true,
      value: 1400,
    });
    resetBindingMocks();
    setBindingMock('SwitchThread', async (threadId: unknown) =>
      makeThread({ id: typeof threadId === 'string' ? threadId : 'thread-1' }));
    // switchThread loads items via ListRecentThreadItems. Tests override
    // the mock to supply specific items; the default is an empty thread
    // so unrelated tests don't have to plumb it.
    setBindingMock('ListRecentThreadItems', async () => ({
      items: [] as Item[],
      oldestTurnIndex: -1,
      hasMore: false,
    }));
    setBindingMock('ListItems', async () => [] as Item[]);
    // switchThread calls ListRecentTurns as part of rehydration. Default
    // to an empty list so tests that don't care about turn rehydration
    // don't need to plumb the mock themselves.
    setBindingMock('ListRecentTurns', async () => []);
  });

  it('starts empty', () => {
    const pane = createThreadPane();

    expect(pane.thread).toBeNull();
    expect(pane.threadId).toBeNull();
    expect(pane.items).toEqual([]);
    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.contextWindow).toBeNull();
    expect(pane.generalError).toBeNull();
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
  });

  it('loads items and seeds the context window from thread.lastTokenUsage', async () => {
    const pane = createThreadPane();
    const items = [
      makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
      makeItem({ id: 'text:0:0', itemIndex: 1, summary: 'hello back' }),
    ];
    setBindingMock('ListRecentThreadItems', async () => ({
      items,
      oldestTurnIndex: 0,
      hasMore: false,
    }));

    await pane.switchThread(makeThread({
      lastTokenUsage: JSON.stringify({
        usedTokens: 1200,
        maxTokens: 200000,
        contextPercent: 0.6,
      }),
    }));

    expect(pane.items).toEqual(items);
    expect(pane.contextWindow).toEqual({
      usedTokens: 1200,
      maxTokens: 200000,
      usedPercentage: 0.6,
      autoCompactPercent: 90,
      autoCompactTokenLimit: 180000,
    });
  });

  it('uses the backend-returned thread from switchThread', async () => {
    const pane = createThreadPane();
    const selected = makeThread({ id: 'thread-a', lastReadAt: 100 });
    setBindingMock('SwitchThread', async () => ({
      ...selected,
      lastReadAt: 300,
    }));

    await pane.switchThread(selected);

    expect(pane.thread?.id).toBe('thread-a');
    expect(pane.thread?.lastReadAt).toBe(300);
  });

  it('preserves provider-emitted max tokens when rehydrating the context meter', async () => {
    const pane = createThreadPane();
    const selected = makeThread({
      id: 'thread-a',
      provider: 'codex',
      model: 'gpt-5.5',
      contextWindow: 1050000,
      lastTokenUsage: JSON.stringify({
        usedTokens: 136000,
        maxTokens: 1050000,
        contextPercent: 12.95,
      }),
    });
    setBindingMock('SwitchThread', async () => ({
      ...selected,
      contextWindow: 272000,
      autoCompactStandardPercent: 80,
      autoCompactExtendedPercent: 88,
    }));

    await pane.switchThread(selected);

    expect(pane.contextWindow).toEqual({
      usedTokens: 136000,
      maxTokens: 1050000,
      usedPercentage: 12.95238095238095,
      autoCompactPercent: 88,
      autoCompactTokenLimit: 924000,
    });
  });

  it('preserves provider-emitted max tokens for live context snapshots', async () => {
    const pane = createThreadPane();
    const selected = makeThread({
      id: 'thread-a',
      provider: 'codex',
      model: 'gpt-5.5',
      contextWindow: 272000,
      autoCompactStandardPercent: 80,
      autoCompactExtendedPercent: 88,
    });
    setBindingMock('SwitchThread', async () => selected);

    await pane.switchThread(selected);
    pane.setContextWindow({
      usedTokens: 136000,
      maxTokens: 1050000,
      usedPercentage: 12.95,
      autoCompactPercent: 88,
      autoCompactTokenLimit: 924000,
    });

    expect(pane.contextWindow).toEqual({
      usedTokens: 136000,
      maxTokens: 1050000,
      usedPercentage: 12.95238095238095,
      autoCompactPercent: 88,
      autoCompactTokenLimit: 924000,
    });
  });

  it('drops wrong-thread rows from initial history hydration', async () => {
    const pane = createThreadPane();
    setBindingMock('ListRecentThreadItems', async () => ({
      items: [
        makeItem({ id: 'current', threadId: 'thread-a' }),
        makeItem({ id: 'leaked', threadId: 'thread-b' }),
      ],
      oldestTurnIndex: 0,
      hasMore: false,
    }));

    await pane.switchThread(makeThread({ id: 'thread-a' }));

    expect(pane.items.map((item) => item.id)).toEqual(['current']);
  });

  it('clears pane-local state on thread switch', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));
    pane.addApproval({
      requestId: 'req-1',
      threadId: 'thread-a',
      toolName: 'bash',
      description: 'Allow bash?',
      input: null,
      title: 'Approve bash',
    });
    pane.setGeneralError('boom');
    pane.setShowTerminal(true);
    pane.setShowPlanSidebar(true);
    pane.openDiffSidebar({ payloadId: 'p1' });

    await pane.switchThread(makeThread({ id: 'thread-b' }));

    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.generalError).toBeNull();
    expect(pane.showTerminal).toBe(false);
    expect(pane.showPlanSidebar).toBe(false);
    expect(pane.activeDiffPayload).toBeNull();
  });

  describe('right-side panel mutex', () => {
    it('opening plan sidebar closes diff panel and diff sidebar', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.setDiffPanelOpen(true);
      expect(pane.diffPanel.open).toBe(true);

      pane.setShowPlanSidebar(true);
      expect(pane.showPlanSidebar).toBe(true);
      expect(pane.diffPanel.open).toBe(false);
      expect(pane.activeDiffPayload).toBeNull();

      pane.openDiffSidebar({ payloadId: 'p1' });
      expect(pane.activeDiffPayload).toEqual({ payloadId: 'p1' });
      expect(pane.showPlanSidebar).toBe(false);
    });

    it('opening diff panel closes plan sidebar and diff sidebar', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.openDiffSidebar({ payloadId: 'p1', filePath: 'src/foo.ts' });
      pane.setShowPlanSidebar(true);
      pane.setDiffPanelOpen(true);

      expect(pane.diffPanel.open).toBe(true);
      expect(pane.showPlanSidebar).toBe(false);
      expect(pane.activeDiffPayload).toBeNull();
    });

    it('opening diff sidebar closes plan sidebar and diff panel', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.setShowPlanSidebar(true);
      pane.setDiffPanelOpen(true);
      pane.openDiffSidebar({ payloadId: 'p1' });

      expect(pane.activeDiffPayload).toEqual({ payloadId: 'p1' });
      expect(pane.showPlanSidebar).toBe(false);
      expect(pane.diffPanel.open).toBe(false);
    });

    it('togglePlanSidebar respects mutex when opening', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.setDiffPanelOpen(true);
      pane.togglePlanSidebar();

      expect(pane.showPlanSidebar).toBe(true);
      expect(pane.diffPanel.open).toBe(false);
    });

    it('toggleDiffPanel respects mutex when opening', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.openDiffSidebar({ payloadId: 'p1' });
      pane.toggleDiffPanel();

      expect(pane.diffPanel.open).toBe(true);
      expect(pane.activeDiffPayload).toBeNull();
    });

    it('closeActivePanel clears all three panel flags', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.openDiffSidebar({ payloadId: 'p1' });
      pane.closeActivePanel();
      expect(pane.activeDiffPayload).toBeNull();

      pane.setShowPlanSidebar(true);
      pane.closeActivePanel();
      expect(pane.showPlanSidebar).toBe(false);

      pane.setDiffPanelOpen(true);
      pane.closeActivePanel();
      expect(pane.diffPanel.open).toBe(false);
    });

    it('closeActivePanel drops the diff-sidebar snapshot when the diff sidebar was the active panel', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.openDiffSidebar({ payloadId: 'pa', filePath: 'src/foo.ts' });
      pane.recordDiffSidebarUI({
        viewMode: 'split',
        wordWrap: false,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 50,
      });

      // Close while the sidebar is the active panel — explicit close.
      pane.closeActivePanel();

      // Snapshot was dropped: switching away and back should not restore.
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.activeDiffPayload).toBeNull();
      expect(pane.consumeDiffSidebarRestoreState()).toBeNull();
    });

    it('closeActivePanel keeps the thread width but clears the restore target', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.openDiffSidebar({ payloadId: 'pa', filePath: 'src/foo.ts' });
      pane.setRhsSidebarWidthLive(620);
      pane.recordDiffSidebarUI({
        viewMode: 'split',
        wordWrap: false,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 50,
      });
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      await pane.switchThread(makeThread({ id: 'thread-a' }));

      pane.closeActivePanel();

      await pane.switchThread(makeThread({ id: 'thread-b' }));
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.activeDiffPayload).toBeNull();
      expect(pane.showPlanSidebar).toBe(false);
      expect(pane.rhsSidebarWidth).toBe(620);
    });
  });

  describe('right-side panel per-thread persistence', () => {
    it('restores the plan sidebar when switching back to its thread', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.setShowPlanSidebar(true);

      await pane.switchThread(makeThread({ id: 'thread-b' }));
      expect(pane.showPlanSidebar).toBe(false);

      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.showPlanSidebar).toBe(true);
      expect(pane.activeRhsPanel).toEqual({ kind: 'plan' });
    });

    it('restores the checkpoint diff panel when switching back to its thread', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.setDiffPanelOpen(true);

      await pane.switchThread(makeThread({ id: 'thread-b' }));
      expect(pane.diffPanel.open).toBe(false);

      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.diffPanel.open).toBe(true);
      expect(pane.activeRhsPanel).toEqual({ kind: 'diff-checkpoint' });
    });

    it('restores right-sidebar width per thread', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.setShowPlanSidebar(true);
      pane.setRhsSidebarWidthLive(620);
      await pane.switchThread(makeThread({ id: 'thread-b' }));

      pane.setDiffPanelOpen(true);
      pane.setRhsSidebarWidthLive(590);

      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.rhsSidebarWidth).toBe(620);
      expect(pane.showPlanSidebar).toBe(true);

      await pane.switchThread(makeThread({ id: 'thread-b' }));
      expect(pane.rhsSidebarWidth).toBe(590);
      expect(pane.diffPanel.open).toBe(true);
    });

    it('restores activeDiffPayload when switching back to a previously-open thread', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.openDiffSidebar({ payloadId: 'pa', filePath: 'src/foo.ts' });
      pane.recordDiffSidebarUI({
        viewMode: 'split',
        wordWrap: true,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 120,
      });

      // Switch away — snapshot is captured.
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      expect(pane.activeDiffPayload).toBeNull();

      // Switch back — sidebar re-arms with the saved payload + UI state.
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.activeDiffPayload).toEqual({ payloadId: 'pa', filePath: 'src/foo.ts' });

      const restored = pane.consumeDiffSidebarRestoreState();
      expect(restored).toEqual({
        viewMode: 'split',
        wordWrap: true,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 120,
      });
      // Consume is one-shot — second call returns null.
      expect(pane.consumeDiffSidebarRestoreState()).toBeNull();
    });

    it('reopening the active payload preserves recorded UI for switch-back restore', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.openDiffSidebar({ payloadId: 'pa', filePath: 'src/foo.ts' });
      pane.recordDiffSidebarUI({
        viewMode: 'split',
        wordWrap: true,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 180,
      });

      pane.openDiffSidebar({ payloadId: 'pa', filePath: 'src/foo.ts' });
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      await pane.switchThread(makeThread({ id: 'thread-a' }));

      expect(pane.activeDiffPayload).toEqual({ payloadId: 'pa', filePath: 'src/foo.ts' });
      expect(pane.consumeDiffSidebarRestoreState()).toEqual({
        viewMode: 'split',
        wordWrap: true,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 180,
      });
    });

    it('does not restore on switch-back if user explicitly closed the sidebar', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.openDiffSidebar({ payloadId: 'pa' });
      pane.recordDiffSidebarUI({
        viewMode: 'stacked',
        wordWrap: false,
        expandedFiles: [],
        scrollTop: 0,
      });
      pane.closeDiffSidebar();

      await pane.switchThread(makeThread({ id: 'thread-b' }));
      await pane.switchThread(makeThread({ id: 'thread-a' }));

      expect(pane.activeDiffPayload).toBeNull();
      expect(pane.consumeDiffSidebarRestoreState()).toBeNull();
    });

    it('LRU-evicts oldest entries past the cap', async () => {
      // The pane's cap is 20. Open + switch 22 distinct threads, then
      // switch back to the first — its snapshot should have evicted.
      const pane = createThreadPane();
      const threadCount = 22;
      const threads = Array.from({ length: threadCount }, (_, i) => makeThread({ id: `t${i}` }));

      for (let i = 0; i < threadCount; i += 1) {
        const next = threads[i];
        if (next === undefined) continue;
        await pane.switchThread(next);
        pane.openDiffSidebar({ payloadId: `p${i}` });
        pane.recordDiffSidebarUI({
          viewMode: 'stacked',
          wordWrap: false,
          expandedFiles: [],
          scrollTop: i * 10,
        });
      }

      // Switch one more time to flush the last open into the map.
      await pane.switchThread(makeThread({ id: 'flush' }));

      // Switching away from the flush thread records it too, so t2 is evicted
      // before restore. t3 is still retained.
      await pane.switchThread(threads[3]!);
      expect(pane.activeDiffPayload).toEqual({ payloadId: 'p3' });

      await pane.switchThread(threads[0]!);
      expect(pane.activeDiffPayload).toBeNull();
    });

    it('clear() wipes the per-thread snapshot map', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.openDiffSidebar({ payloadId: 'pa' });
      pane.recordDiffSidebarUI({
        viewMode: 'stacked',
        wordWrap: false,
        expandedFiles: [],
        scrollTop: 0,
      });

      pane.clear();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.activeDiffPayload).toBeNull();
    });
  });

  it('ignores stale ListRecentThreadItems resolutions after a second thread switch', async () => {
    const pane = createThreadPane();
    type Paged = { items: Item[]; oldestTurnIndex: number; hasMore: boolean };
    let resolveA!: (paged: Paged) => void;
    let resolveB!: (paged: Paged) => void;
    const listA = new Promise<Paged>((resolve) => { resolveA = resolve; });
    const listB = new Promise<Paged>((resolve) => { resolveB = resolve; });

    setBindingMock('ListRecentThreadItems', (threadId: string) => (
      threadId === 'thread-a' ? listA : listB
    ));

    const switchA = pane.switchThread(makeThread({ id: 'thread-a' }));
    const switchB = pane.switchThread(makeThread({ id: 'thread-b' }));

    resolveB({
      items: [makeItem({ id: 'b', threadId: 'thread-b', summary: 'from b' })],
      oldestTurnIndex: 0,
      hasMore: false,
    });
    await switchB;
    resolveA({
      items: [makeItem({ id: 'a', threadId: 'thread-a', summary: 'from a' })],
      oldestTurnIndex: 0,
      hasMore: false,
    });
    await switchA;

    expect(pane.threadId).toBe('thread-b');
    expect(pane.items.map((item) => item.id)).toEqual(['b']);
  });

  it('upsertItem inserts in turn/item order and replaces rows in place', () => {
    const pane = createThreadPane();

    pane.upsertItem(makeItem({ id: 'late', turnIndex: 1, itemIndex: 0 }));
    pane.upsertItem(makeItem({ id: 'early', turnIndex: 0, itemIndex: 1 }));
    pane.upsertItem(makeItem({ id: 'first', turnIndex: 0, itemIndex: 0 }));

    expect(pane.items.map((item) => item.id)).toEqual(['first', 'early', 'late']);

    pane.upsertItem(makeItem({ id: 'early', turnIndex: 0, itemIndex: 1, summary: 'updated' }));

    expect(pane.items.map((item) => item.id)).toEqual(['first', 'early', 'late']);
    expect(pane.items.find((item) => item.id === 'early')?.summary).toBe('updated');
  });

  it('allows upsertItem to be used as an unbound callback', () => {
    const pane = createThreadPane();
    const { upsertItem } = pane;

    upsertItem(makeItem({ id: 'unbound', turnIndex: 0, itemIndex: 0 }));

    expect(pane.items.map((item) => item.id)).toEqual(['unbound']);
  });

  it('upsertItems merges bursts in order and bumps timeline revision once', () => {
    const pane = createThreadPane();

    pane.upsertItems([
      makeItem({ id: 'late', turnIndex: 1, itemIndex: 0 }),
      makeItem({ id: 'early', turnIndex: 0, itemIndex: 1 }),
      makeItem({ id: 'first', turnIndex: 0, itemIndex: 0 }),
    ]);

    expect(pane.items.map((item) => item.id)).toEqual(['first', 'early', 'late']);
    expect(pane.timelineRevision).toBe(1);

    pane.upsertItems([
      makeItem({ id: 'late', turnIndex: 0, itemIndex: 2, summary: 'moved' }),
      makeItem({ id: 'early', turnIndex: 0, itemIndex: 1, summary: 'updated' }),
    ]);

    expect(pane.items.map((item) => item.id)).toEqual(['first', 'early', 'late']);
    expect(pane.items.find((item) => item.id === 'late')?.summary).toBe('moved');
    expect(pane.timelineRevision).toBe(2);
  });

  it('collapses same-batch wait-row enrichment into one final row', () => {
    const pane = createThreadPane();

    pane.upsertItems([
      makeItem({
        id: 'wait:pid-1:0',
        kind: 'terminal_interaction',
        summary: 'Waited for background terminal',
      }),
      makeItem({
        id: 'wait:pid-1:0',
        kind: 'terminal_interaction',
        summary: 'Background terminal completed',
        payloadId: 'payload-1',
        payloadKind: 'command_output',
        payloadMeta: JSON.stringify({ exitCode: 0 }),
      }),
    ]);

    expect(pane.items).toHaveLength(1);
    expect(pane.items[0].payloadKind).toBe('command_output');
    expect(pane.items[0].payloadId).toBe('payload-1');
    expect(pane.timelineRevision).toBe(1);
  });

  it('preserves arrival order for rows with the same turn and item position', () => {
    const pane = createThreadPane();

    pane.upsertItems([
      makeItem({ id: 'later-position', turnIndex: 1, itemIndex: 0 }),
      makeItem({ id: 'first-arrived', turnIndex: 0, itemIndex: 0, createdAt: 200 }),
      makeItem({ id: 'second-arrived', turnIndex: 0, itemIndex: 0, createdAt: 100 }),
    ]);

    expect(pane.items.map((item) => item.id)).toEqual([
      'first-arrived',
      'second-arrived',
      'later-position',
    ]);
  });

  it('keeps streaming deltas out of the timeline item array', async () => {
    const pane = createThreadPane();
    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'hello',
    }));
    const initialItems = pane.items;
    const initialRevision = pane.timelineRevision;

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: ' world',
      updatedAt: 123,
    });
    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: '!',
      updatedAt: 124,
    });
    await nextFrame();

    expect(pane.items).toBe(initialItems);
    expect(pane.timelineRevision).toBe(initialRevision);
    expect(pane.liveItemSummaries['text:0:0']).toBe('hello world!');
  });

  it('bumps liveDeltaRevision once per coalesced delta flush so auto-follow re-fires while streaming', async () => {
    // Why: auto-follow (`MessageTimeline.svelte`) tracks length /
    // timelineRevision / activeTurn. None of those tick during a pure
    // streaming row growing in place — only `liveItemSummaries` does.
    // Without `liveDeltaRevision`, the controller never re-pins to the
    // new bottom and the user drifts off-screen until the row settles.
    const pane = createThreadPane();
    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'hello',
    }));
    const initialDeltaRevision = pane.liveDeltaRevision;

    // Two deltas in the same frame coalesce into one flush → one bump.
    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: ' world',
      updatedAt: 123,
    });
    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: '!',
      updatedAt: 124,
    });
    await nextFrame();

    expect(pane.liveDeltaRevision).toBe(initialDeltaRevision + 1);

    // A second flush in a later frame bumps again.
    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: ' more',
      updatedAt: 125,
    });
    await nextFrame();
    expect(pane.liveDeltaRevision).toBe(initialDeltaRevision + 2);
  });

  it('does not bump liveDeltaRevision when no chunks are pending', async () => {
    const pane = createThreadPane();
    const initial = pane.liveDeltaRevision;
    // flushLiveDeltaChunks short-circuits when liveDeltaChunks is empty;
    // no bump should happen on a fresh pane that hasn't received deltas.
    await nextFrame();
    expect(pane.liveDeltaRevision).toBe(initial);
  });

  it('clears live summary buffers when a streaming item settles', async () => {
    const pane = createThreadPane();
    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'hello',
    }));
    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: ' world',
      updatedAt: 123,
    });
    await nextFrame();

    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'completed',
      summary: 'hello world',
    }));

    expect(pane.liveItemSummaries['text:0:0']).toBeUndefined();
    expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe('hello world');
  });

  it('ignores stale deltas for an item that already settled', async () => {
    const pane = createThreadPane();
    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'completed',
      summary: 'yield timeouts',
    }));

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: 'outs',
      updatedAt: 124,
    });
    await nextFrame();

    expect(pane.liveItemSummaries['text:0:0']).toBeUndefined();
    expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe('yield timeouts');
  });

  it('expansionStateFor returns the same handle across calls (survives row remount)', () => {
    // Why: virtua's overscan eviction unmounts a row component when it
    // scrolls past the buffer; remounting reconstructs the snippet's
    // closure-scoped $state from scratch. The pane registry returns
    // the SAME handle reference for the same itemId, so toggle state
    // and loaded chunks survive the round-trip.
    const pane = createThreadPane();
    const item = makeItem({ id: 'tool:5:0', kind: 'tool_call', payloadId: 'p-foo' });
    pane.upsertItem(item);

    const h1 = pane.expansionStateFor(item);
    const h2 = pane.expansionStateFor(item);
    expect(h2).toBe(h1);

    // Even when the Item reference is replaced (e.g. enrichment), the
    // handle stays stable because the cache key is item.id.
    const itemRefBumped = { ...pane.items[0], updatedAt: 999 } as Item;
    const h3 = pane.expansionStateFor(itemRefBumped);
    expect(h3).toBe(h1);
  });

  it('expansionStateForPayload returns the same handle for the same payloadId', () => {
    const pane = createThreadPane();
    const h1 = pane.expansionStateForPayload('p-foo', 'thread-1');
    const h2 = pane.expansionStateForPayload('p-foo', 'thread-1');
    expect(h2).toBe(h1);
  });

  it('payload-keyed expansion handles reload when their version changes', async () => {
    let version = 1;
    const preview = setBindingMock('GetPayloadPreview', async () => ({
      data: version === 1 ? 'payload v1' : 'payload v2',
      nextOffset: 10,
      totalSize: 10,
      isComplete: true,
    }));

    const pane = createThreadPane();
    const first = pane.expansionStateForPayload('p-versioned', 'thread-1', version);
    await first.expand();
    expect(first.displayData).toBe('payload v1');

    version = 2;
    const second = pane.expansionStateForPayload('p-versioned', 'thread-1', version);
    expect(second).toBe(first);

    await second.ensureLoaded();
    expect(second.displayData).toBe('payload v2');
    expect(preview).toHaveBeenCalledTimes(2);
  });

  it('subagent group expansion state is keyed by groupKey and survives lookup', () => {
    const pane = createThreadPane();
    expect(pane.isSubagentGroupExpanded('group-1')).toBe(false);
    pane.toggleSubagentGroupExpanded('group-1');
    expect(pane.isSubagentGroupExpanded('group-1')).toBe(true);
    expect(pane.isSubagentGroupExpanded('group-2')).toBe(false);
    pane.toggleSubagentGroupExpanded('group-1');
    expect(pane.isSubagentGroupExpanded('group-1')).toBe(false);
  });

  it('attachmentCacheFor returns a stable view per itemId; survives lookup', () => {
    // Why: pre-rebuild, UserMessage.svelte allocated blob URLs in its
    // own onDestroy-revoking factory. virtua's overscan eviction would
    // unmount + remount the row on a back-scroll, refetching every
    // attachment from Go. The pane-owned cache survives remount; the
    // factory seeds from it and writes loaded previews back.
    const pane = createThreadPane();
    const cacheA = pane.attachmentCacheFor('item-1');
    cacheA.set('att-1', { id: 'att-1', filename: 'a.png', mimeType: 'image/png', size: 1, url: 'data:img' });
    const cacheA2 = pane.attachmentCacheFor('item-1');
    expect(cacheA2.get('att-1')).toBeTruthy();
    expect(cacheA2.get('att-1')?.url).toBe('data:img');
    // Different itemId = isolated cache.
    const cacheB = pane.attachmentCacheFor('item-2');
    expect(cacheB.get('att-1')).toBeUndefined();
  });

  it('clears row UI state on switchThread', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));
    pane.upsertItem(makeItem({ id: 'tool:0:0', kind: 'tool_call', payloadId: 'p-1', threadId: 'thread-a' }));
    expect(pane.items.length).toBe(1);
    const h1 = pane.expansionStateFor(pane.items[0]);
    pane.toggleSubagentGroupExpanded('parent-x');
    expect(pane.isSubagentGroupExpanded('parent-x')).toBe(true);

    await pane.switchThread(makeThread({ id: 'thread-b' }));
    pane.upsertItem(makeItem({ id: 'tool:0:0', kind: 'tool_call', payloadId: 'p-2', threadId: 'thread-b' }));
    const h2 = pane.expansionStateFor(pane.items[0]);
    // Different thread → different handle (the previous one was cleared).
    expect(h2).not.toBe(h1);
    // SubagentGroup state was cleared too.
    expect(pane.isSubagentGroupExpanded('parent-x')).toBe(false);
  });

  it('merges deltas that arrive before the initial streaming row', async () => {
    const pane = createThreadPane();

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: ' world',
      updatedAt: 123,
    });
    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'hello',
    }));
    await nextFrame();

    expect(pane.liveItemSummaries['text:0:0']).toBe('hello world');
  });

  it('drops wrong-thread upserts for an active pane', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));

    pane.upsertItem(makeItem({ id: 'leaked', threadId: 'thread-b' }));
    pane.upsertItem(makeItem({ id: 'current', threadId: 'thread-a' }));

    expect(pane.items.map((item) => item.id)).toEqual(['current']);
  });

  it('derives isTurnActive strictly from activeTurn (invariant 22)', async () => {
    // Post-refactor, isTurnActive comes solely from the wire-pushed
    // activeTurn slot. Item state (streaming text, running tool_calls,
    // pending approvals) no longer leaks into the flag. The active-
    // turn registry is keyed by threadId, so the pane needs a thread
    // loaded before set/clear can route through to the global store.
    const pane = createThreadPane();
    await pane.switchThread(makeThread());

    expect(getActiveTurn(pane.threadId) !== null).toBe(false);

    // A streaming assistant item alone doesn't flip the flag.
    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'streaming',
    }));
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);

    // A running foreground tool_call alone doesn't flip the flag either.
    pane.upsertItem(makeItem({
      id: 'tool-1',
      kind: 'tool_call',
      status: 'running',
      isBackground: false,
    }));
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);

    // Pending approvals no longer count on their own — they live INSIDE
    // an active turn (see invariant 22 rationale).
    pane.addApproval({
      requestId: 'req-1',
      threadId: 'thread-1',
      toolName: 'edit',
      description: 'Allow edit?',
      input: null,
      title: 'Approve edit',
    });
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);

    // Wire-push flips it on.
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 1 });
    expect(getActiveTurn(pane.threadId) !== null).toBe(true);

    // settleTurn clears it even if streaming items / approvals remain.
    pane.settleTurn({
      turnId: 't1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
      assistantMessageId: null,
      tokenUsage: null,
      aborted: false,
      errorMessage: '',
    });
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
  });

  it('clear resets the pane completely', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread());
    pane.upsertItem(makeItem({ id: 'x' }));
    pane.setGeneralError('boom');
    pane.addApproval({
      requestId: 'req-1',
      threadId: 'thread-1',
      toolName: 'bash',
      description: 'Allow bash?',
      input: null,
      title: 'Approve bash',
    });

    pane.clear();

    expect(pane.thread).toBeNull();
    expect(pane.items).toEqual([]);
    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.contextWindow).toBeNull();
    expect(pane.generalError).toBeNull();
  });

  describe('windowed history', () => {
    it('upsertItem drops new items below the window floor', async () => {
      const pane = createThreadPane();
      const seed: Item[] = [
        makeItem({ id: 'at-floor', threadId: 'thread-windowed', turnIndex: 5, itemIndex: 0 }),
      ];
      setBindingMock('ListRecentThreadItems', async () => ({
        items: seed,
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 'thread-windowed' }));
      expect(pane.oldestLoadedTurnIndex).toBe(5);

      // Upsert for a turn below the floor (e.g. interrupt-queue replay
      // of an older tool_completion). Must NOT land in the window — the
      // canonical row stays in SQLite and surfaces via loadOlder later.
      pane.upsertItem(makeItem({ id: 'below', threadId: 'thread-windowed', turnIndex: 2, itemIndex: 0 }));
      expect(pane.items.map((it) => it.id)).toEqual(['at-floor']);
    });

    it('upsertItem still accepts replacements for known ids below the floor', async () => {
      const pane = createThreadPane();
      const seed: Item[] = [
        makeItem({ id: 'known', threadId: 't', turnIndex: 5, itemIndex: 0, summary: 'old' }),
      ];
      setBindingMock('ListRecentThreadItems', async () => ({
        items: seed,
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));

      // Known id, turn below floor — cross-turn correction path. Must
      // still replace because the id is clearly in-window already.
      pane.upsertItem(makeItem({ id: 'known', threadId: 't', turnIndex: 2, itemIndex: 0, summary: 'new' }));
      expect(pane.items.find((it) => it.id === 'known')?.summary).toBe('new');
    });

    it('upsertItem drops live buffers for new streaming rows below the floor', async () => {
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [makeItem({ id: 'at-floor', threadId: 't', turnIndex: 5, itemIndex: 0 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));

      pane.upsertItem(makeItem({
        id: 'below-streaming',
        threadId: 't',
        turnIndex: 2,
        itemIndex: 0,
        status: 'streaming',
        summary: 'old output',
      }));

      expect(pane.items.map((it) => it.id)).toEqual(['at-floor']);
      expect(pane.liveItemSummaries['below-streaming']).toBeUndefined();
    });

    it('loadOlder prepends older items and updates the floor + hasMore', async () => {
      const pane = createThreadPane();
      const tail: Item[] = [
        makeItem({ id: 't5', threadId: 't', turnIndex: 5, itemIndex: 0 }),
      ];
      setBindingMock('ListRecentThreadItems', async () => ({
        items: tail,
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [
          makeItem({ id: 't3', threadId: 't', turnIndex: 3, itemIndex: 0 }),
          makeItem({ id: 't4', threadId: 't', turnIndex: 4, itemIndex: 0 }),
        ],
        oldestTurnIndex: 3,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      const result = await pane.loadOlder();

      expect(pane.items.map((it) => it.id)).toEqual(['t3', 't4', 't5']);
      expect(pane.oldestLoadedTurnIndex).toBe(3);
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.loadingOlder).toBe(false);
      expect(result).toEqual({ status: 'loaded', insertedBeforeWindow: true, insertedRows: true });
    });

    it('loadOlder is a no-op when hasMoreHistory is false', async () => {
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [makeItem({ id: 'a', turnIndex: 0, itemIndex: 0 })],
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      let calls = 0;
      setBindingMock('ListItemsBeforeTurn', async () => {
        calls += 1;
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't' }));
      const result = await pane.loadOlder();
      expect(calls).toBe(0);
      expect(result).toEqual({ status: 'noop', insertedBeforeWindow: false, insertedRows: false });
    });

    it('loadOlder guards against a thread swap mid-fetch', async () => {
      const pane = createThreadPane();
      let resolveOlder!: (v: {
        items: Item[]; oldestTurnIndex: number; hasMore: boolean;
      }) => void;
      const olderPromise = new Promise<{
        items: Item[]; oldestTurnIndex: number; hasMore: boolean;
      }>((r) => { resolveOlder = r; });
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [makeItem({ id: 'tail', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeTurn', () => olderPromise);

      await pane.switchThread(makeThread({ id: 'thread-a' }));
      const olderPending = pane.loadOlder();
      // Swap before the older fetch resolves.
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      resolveOlder({
        items: [makeItem({ id: 'stale', turnIndex: 3, threadId: 'thread-a' })],
        oldestTurnIndex: 3,
        hasMore: true,
      });
      await olderPending;

      // thread-b has its own fresh window; the stale thread-a older
      // fetch must not leak into it.
      expect(pane.threadId).toBe('thread-b');
      expect(pane.items.some((it) => it.id === 'stale')).toBe(false);
    });

    it('loadUntilItem returns true when the item is already in-window', async () => {
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [makeItem({ id: 'here', threadId: 't', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      let fetched = 0;
      setBindingMock('GetThreadItem', async () => {
        fetched += 1;
        return makeItem({ id: 'here', turnIndex: 5 });
      });
      await pane.switchThread(makeThread({ id: 't' }));
      const ok = await pane.loadUntilItem('here');
      expect(ok).toBe(true);
      expect(fetched).toBe(0);
    });

    it('loadUntilItem replaces the window to cover a below-floor item', async () => {
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [makeItem({ id: 't5', threadId: 't', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('GetThreadItem', async (_threadId: string, itemId: string) =>
        itemId === 'target'
          ? makeItem({ id: 'target', threadId: 't', turnIndex: 1 })
          : null,
      );
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [
          makeItem({ id: 'target', threadId: 't', turnIndex: 1 }),
          makeItem({ id: 't2', threadId: 't', turnIndex: 2 }),
          makeItem({ id: 't3', threadId: 't', turnIndex: 3 }),
          makeItem({ id: 't4', threadId: 't', turnIndex: 4 }),
        ],
        oldestTurnIndex: 1,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      const ok = await pane.loadUntilItem('target');

      expect(ok).toBe(true);
      expect(pane.oldestLoadedTurnIndex).toBe(1);
      expect(pane.items.map((it) => it.id)).toEqual(['target', 't2', 't3', 't4', 't5']);
    });

    it('loadUntilItem returns false when the item is unknown to the backend', async () => {
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [makeItem({ id: 't5', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('GetThreadItem', async () => makeItem({ id: '' }));
      await pane.switchThread(makeThread({ id: 't' }));
      const ok = await pane.loadUntilItem('ghost');
      expect(ok).toBe(false);
    });

    it('requestScrollToItem bumps the nonce observed by the timeline', () => {
      const pane = createThreadPane();
      const first = pane.scrollToItemRequest.nonce;
      pane.requestScrollToItem('a');
      const second = pane.scrollToItemRequest.nonce;
      expect(second).toBeGreaterThan(first);
      expect(pane.scrollToItemRequest.itemId).toBe('a');
      expect(pane.scrollToItemRequest.behavior).toBe('instant');
      expect(pane.scrollToItemRequest.flash).toBe(false);
      pane.requestScrollToItem('b');
      expect(pane.scrollToItemRequest.nonce).toBeGreaterThan(second);
      expect(pane.scrollToItemRequest.itemId).toBe('b');
    });

    it('requestScrollToItem carries animation and flash options', () => {
      const pane = createThreadPane();
      pane.requestScrollToItem('checkpoint-user-message', {
        behavior: 'animated',
        flash: true,
      });

      expect(pane.scrollToItemRequest.itemId).toBe('checkpoint-user-message');
      expect(pane.scrollToItemRequest.behavior).toBe('animated');
      expect(pane.scrollToItemRequest.flash).toBe(true);
    });

    it('scrollToItemRequest nonce stays monotonic across switchThread', async () => {
      // The timeline tracks `lastHandledScrollNonce` locally. If a pane
      // reset the nonce to 0 on switch, a follow-up intent with nonce=1
      // would compare against the lingering higher handled value and
      // silently not dispatch. Keep the nonce monotonic.
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      pane.requestScrollToItem('before-switch');
      const beforeSwitch = pane.scrollToItemRequest.nonce;
      expect(beforeSwitch).toBeGreaterThan(0);

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.scrollToItemRequest.nonce).toBe(beforeSwitch);

      pane.requestScrollToItem('after-switch');
      expect(pane.scrollToItemRequest.nonce).toBeGreaterThan(beforeSwitch);
    });

    it('loadUntilItem loads the target turn when the pane has no floor yet', async () => {
      // An empty-thread pane (or one whose switchThread returned 0 items)
      // has `oldestLoadedTurnIndex = null`. The loader must still be able
      // to pull in history when the user triggers scroll-to-item from
      // search — not skip the fetch and short-circuit to `true`.
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 3 }),
      );
      let beforeTurnCalled: number | null = null;
      setBindingMock('ListItemsBeforeTurn', async (_id, beforeTurn) => {
        beforeTurnCalled = beforeTurn as number;
        return {
          items: [makeItem({ id: 'deep', threadId: 't', turnIndex: 3 })],
          oldestTurnIndex: 3,
          hasMore: false,
        };
      });

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.oldestLoadedTurnIndex).toBeNull();

      const ok = await pane.loadUntilItem('deep');
      expect(ok).toBe(true);
      expect(beforeTurnCalled).toBe(4);
      expect(pane.items.some((it) => it.id === 'deep')).toBe(true);
      expect(pane.oldestLoadedTurnIndex).toBe(3);
    });

    it('loadUntilItem rejects an item whose threadId does not match the current pane', async () => {
      // Defense-in-depth: a mislayered binding or stale cache that
      // returns a row from a different thread should never cross-pollute
      // a pane. loadUntilItem must treat the mismatch as "not found"
      // rather than trying to page an item that doesn't belong here.
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [makeItem({ id: 'tail', threadId: 't', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'wrong', threadId: 'other-thread', turnIndex: 1 }),
      );
      let paged = 0;
      setBindingMock('ListItemsBeforeTurn', async () => {
        paged += 1;
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't' }));

      const ok = await pane.loadUntilItem('wrong');
      expect(ok).toBe(false);
      expect(paged).toBe(0);
    });

    it('loadOlder disables hasMoreHistory when the backend cannot advance the floor', async () => {
      // Pathological scenario: turns table claims more history exists
      // but the item range [newFloor, beforeTurn) is empty (a sparse
      // turn row with no items). Without a progress guard the Load
      // Older button would keep firing the same query. The store must
      // break the loop by forcing hasMoreHistory=false when no rows
      // were returned AND the floor did not decrease.
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [makeItem({ id: 'tail', threadId: 't', turnIndex: 10 })],
        oldestTurnIndex: 10,
        hasMore: true,
      }));
      let calls = 0;
      setBindingMock('ListItemsBeforeTurn', async () => {
        calls += 1;
        // Backend cooperates: no items, floor unchanged, but still
        // claims more exists. Common when a turn row has zero items.
        return { items: [], oldestTurnIndex: 10, hasMore: true };
      });

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.hasMoreHistory).toBe(true);
      await pane.loadOlder();
      expect(calls).toBe(1);
      expect(pane.hasMoreHistory).toBe(false);
      // Second invocation should short-circuit; no network call.
      await pane.loadOlder();
      expect(calls).toBe(1);
    });

    it('loadOlder clears loadingOlder even when a concurrent loadUntilItem bumps the paging generation', async () => {
      // Regression pin: `loadingOlder` is a UI-only flag. If a
      // concurrent `loadUntilItem` increments `pagingGeneration`
      // while `loadOlder` is mid-fetch, the generation-guarded
      // finally block used to skip clearing the flag, greying out
      // the Load Older button forever. The fix resets the flag
      // unconditionally.
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [makeItem({ id: 'tail', threadId: 't', turnIndex: 10 })],
        oldestTurnIndex: 10,
        hasMore: true,
      }));
      let releaseOlder!: (v: {
        items: ReturnType<typeof makeItem>[]; oldestTurnIndex: number; hasMore: boolean;
      }) => void;
      const olderPending = new Promise<{
        items: ReturnType<typeof makeItem>[]; oldestTurnIndex: number; hasMore: boolean;
      }>((r) => { releaseOlder = r; });
      setBindingMock('ListItemsBeforeTurn', () => olderPending);

      await pane.switchThread(makeThread({ id: 't' }));
      const olderPromise = pane.loadOlder();
      expect(pane.loadingOlder).toBe(true);

      // Kick off loadUntilItem, which increments pagingGeneration and
      // takes its own path. It must not deadlock loadOlder's cleanup.
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'tail', threadId: 't', turnIndex: 10 }),
      );
      await pane.loadUntilItem('tail');

      releaseOlder({ items: [], oldestTurnIndex: 10, hasMore: false });
      await olderPromise;

      expect(pane.loadingOlder).toBe(false);
    });

    it('loadUntilItem uses the default batch size when the pane floor is null', async () => {
      // Regression pin for the MAX_SAFE_INTEGER turnSpan bug: when
      // currentFloor is null (empty window), the request must pass a
      // bounded turnLimit rather than a sentinel number. Check that
      // the actual turnLimit argument is the default batch size.
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 3 }),
      );
      let capturedBeforeTurn: number | null = null;
      let capturedLimit: number | null = null;
      setBindingMock('ListItemsBeforeTurn', async (_id, beforeTurn, limit) => {
        capturedBeforeTurn = beforeTurn as number;
        capturedLimit = limit as number;
        return {
          items: [makeItem({ id: 'deep', threadId: 't', turnIndex: 3 })],
          oldestTurnIndex: 3,
          hasMore: false,
        };
      });

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.oldestLoadedTurnIndex).toBeNull();
      const ok = await pane.loadUntilItem('deep');
      expect(ok).toBe(true);
      // The default batch (LOAD_OLDER_TURN_BATCH=50) — not a sentinel
      // like Number.MAX_SAFE_INTEGER.
      expect(capturedLimit).toBeLessThanOrEqual(200);
      expect(capturedLimit).toBeGreaterThan(0);
      expect(capturedBeforeTurn).toBe(4);
    });

    it('pagingGeneration stays monotonic across switchThread', async () => {
      // Regression: earlier the reset to 0 on swap meant a stale
      // in-flight paging fetch could see its captured generation
      // match the freshly-reset counter and proceed to clobber
      // state. The switchGeneration guard catches the common case
      // but pinning the monotonicity invariant here prevents a
      // future refactor from reintroducing the reset.
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [makeItem({ id: 'a', threadId: 't', turnIndex: 0 })],
        oldestTurnIndex: 0,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'x', threadId: 't', turnIndex: 0 }),
      );

      await pane.switchThread(makeThread({ id: 't' }));
      // Trigger a paging call so pagingGeneration advances to 1.
      await pane.loadOlder();
      await pane.switchThread(makeThread({ id: 't2' }));
      // After switch, another paging call should advance the counter
      // further — never regress to a prior value. We observe by
      // chaining two calls and ensuring the second still makes a
      // network call (i.e. the guards remain accurate).
      let postSwitchCalls = 0;
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [makeItem({ id: 'b', threadId: 't2', turnIndex: 3 })],
        oldestTurnIndex: 3,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeTurn', async () => {
        postSwitchCalls += 1;
        return { items: [], oldestTurnIndex: 2, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't3' }));
      await pane.loadOlder();
      expect(postSwitchCalls).toBe(1);
    });

    it('loadOlder dedupes by id when the backend re-returns an ancestor', async () => {
      // Backend contract: `ListItemsBeforeTurn` can legitimately
      // return an ancestor row that was already in the window (pulled
      // in by the initial load via `ListRecentItemsWithAncestors`'s
      // ancestor CTE). The store must not duplicate the row in
      // `items` — the dedup happens via `mergeItemsById`.
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'child', threadId: 't', turnIndex: 5 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeTurn', async () => ({
        // Backend legitimately returns the ancestor again (it sits
        // below the new paging floor and the recursive CTE pulls it
        // in for any child-in-range query).
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'between', threadId: 't', turnIndex: 3 }),
        ],
        oldestTurnIndex: 3,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.items.map((it) => it.id)).toEqual(['ancestor', 'child']);

      await pane.loadOlder();
      // 'ancestor' appears once — duplicates are filtered out.
      const ancestors = pane.items.filter((it) => it.id === 'ancestor');
      expect(ancestors.length).toBe(1);
      // Ordering: the newly prepended 'between' sits before the
      // existing tail. The duplicate ancestor row was dropped so the
      // original position is preserved.
      expect(pane.items.map((it) => it.id)).toEqual(['ancestor', 'between', 'child']);
    });

    it('loadOlder replaces duplicate rows with enriched backend copies', async () => {
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [
          makeItem({
            id: 'ancestor',
            threadId: 't',
            turnIndex: 0,
            summary: 'summary-only',
          }),
          makeItem({ id: 'child', threadId: 't', turnIndex: 5 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [
          makeItem({
            id: 'ancestor',
            threadId: 't',
            turnIndex: 0,
            summary: 'enriched',
            payloadId: 'payload-ancestor',
          }),
          makeItem({ id: 'between', threadId: 't', turnIndex: 3 }),
        ],
        oldestTurnIndex: 3,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      await pane.loadOlder();

      const ancestor = pane.items.find((it) => it.id === 'ancestor');
      expect(ancestor?.summary).toBe('enriched');
      expect(ancestor?.payloadId).toBe('payload-ancestor');
      expect(pane.items.filter((it) => it.id === 'ancestor')).toHaveLength(1);
    });

    it('loadOlder reports rows inserted after an ancestor above the floor', async () => {
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'child', threadId: 't', turnIndex: 5 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [makeItem({ id: 'between', threadId: 't', turnIndex: 3 })],
        oldestTurnIndex: 3,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      const result = await pane.loadOlder();

      expect(pane.items.map((it) => it.id)).toEqual(['ancestor', 'between', 'child']);
      expect(result).toEqual({
        status: 'loaded',
        insertedBeforeWindow: false,
        insertedRows: true,
      });
    });

    it('loadUntilItem dedupes by id when pulling in a below-floor target', async () => {
      // Same contract as loadOlder's dedup, but via the
      // scroll-to-item entry point. If `ListItemsBeforeTurn` returns
      // a row already present by id (e.g. the subagent ancestor), no
      // duplicate should land in the window.
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'tail', threadId: 't', turnIndex: 5 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 2 }),
      );
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'deep', threadId: 't', turnIndex: 2 }),
        ],
        oldestTurnIndex: 2,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      const ok = await pane.loadUntilItem('deep');
      expect(ok).toBe(true);
      expect(pane.items.filter((it) => it.id === 'ancestor').length).toBe(1);
      expect(pane.items.some((it) => it.id === 'deep')).toBe(true);
    });

    it('upsertItem accepts new items when the pane floor is null (empty thread)', async () => {
      // Regression: the floor guard short-circuits when
      // `oldestLoadedTurnIndex` is null so streamed upserts on a
      // fresh pane still land. Without the null check, every first
      // item on a brand-new thread would be dropped.
      const pane = createThreadPane();
      setBindingMock('ListRecentThreadItems', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.oldestLoadedTurnIndex).toBeNull();
      pane.upsertItem(makeItem({ id: 'first', threadId: 't', turnIndex: 0, itemIndex: 0 }));
      expect(pane.items.map((it) => it.id)).toEqual(['first']);
    });
  });

  // --- Turn-lifecycle pane state (Wave 2) -----------------------------------

  it('setActiveTurn populates activeTurn and flips isTurnActive on', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread());
    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);

    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });

    expect(getActiveTurn(pane.threadId)).toEqual({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });
    expect(getActiveTurn(pane.threadId) !== null).toBe(true);
  });

  it('setActiveTurn is idempotent by turnId — preserves startedAt on re-emit', async () => {
    // A Claude re-init / interrupt can re-send EventTurnStart for the same
    // (thread, turn). The pane must not rewind startedAt — otherwise the
    // working indicator's elapsed-seconds counter would jump backward each
    // time the provider re-initialises.
    const pane = createThreadPane();
    await pane.switchThread(makeThread());
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 9999 });
    expect(getActiveTurn(pane.threadId)?.startedAt).toBe(1000);
  });

  it('settleTurn clears activeTurn and writes latestSettledTurn', () => {
    const pane = createThreadPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });

    pane.settleTurn({
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1000,
      completedAt: 2000,
      stopReason: 'end_turn',
      assistantMessageId: 'text:0:3',
      tokenUsage: { inputTokens: 100, outputTokens: 50 },
      aborted: false,
      errorMessage: '',
    });

    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
    expect(pane.latestSettledTurn).toEqual({
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1000,
      completedAt: 2000,
      stopReason: 'end_turn',
      assistantMessageId: 'text:0:3',
      tokenUsage: { inputTokens: 100, outputTokens: 50 },
      aborted: false,
      errorMessage: '',
    });
  });

  it('clearTurnState resets both slots without rehydrating', () => {
    const pane = createThreadPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    pane.settleTurn({
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
      assistantMessageId: null,
      tokenUsage: null,
      aborted: false,
      errorMessage: '',
    });
    expect(pane.latestSettledTurn).not.toBeNull();

    pane.clearTurnState();
    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(pane.latestSettledTurn).toBeNull();
  });

  it('switchThread rehydrates latestSettledTurn from the most recent completed row', async () => {
    setBindingMock('ListRecentTurns', async () => [
      {
        turnId: 'turn-1',
        threadId: 'thread-a',
        turnIndex: 1,
        startedAt: 1000,
        completedAt: 2000,
        stopReason: 'end_turn',
        assistantMessageId: 'text:1:4',
        tokenUsageJson: JSON.stringify({
          inputTokens: 150,
          outputTokens: 75,
          totalCostUsd: 0.012,
        }),
      },
    ]);

    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));

    expect(pane.latestSettledTurn).toEqual({
      turnId: 'turn-1',
      turnIndex: 1,
      startedAt: 1000,
      completedAt: 2000,
      stopReason: 'end_turn',
      assistantMessageId: 'text:1:4',
      tokenUsage: {
        inputTokens: 150,
        outputTokens: 75,
        totalCostUsd: 0.012,
      },
      aborted: false,
      errorMessage: '',
    });
    // activeTurn stays null even though rehydration ran — invariant 22.
    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
  });

  it('switchThread does NOT promote an in-flight historical turn to activeTurn', async () => {
    // Most-recent row has completedAt=null → a crashed / interrupted
    // turn that was never settled. The frontend MUST leave activeTurn
    // alone; only a fresh `provider:turn_started` push can light up the
    // working indicator (invariant 22).
    setBindingMock('ListRecentTurns', async () => [
      {
        turnId: 'turn-crashed',
        threadId: 'thread-a',
        turnIndex: 1,
        startedAt: 1000,
        completedAt: null,
      },
      {
        turnId: 'turn-settled',
        threadId: 'thread-a',
        turnIndex: 0,
        startedAt: 500,
        completedAt: 900,
        stopReason: 'end_turn',
        assistantMessageId: 'text:0:2',
        tokenUsageJson: '',
      },
    ]);

    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));

    // Not lit up.
    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
    // But the prior settled turn IS rehydrated for read-state and trace/debug
    // consumers.
    expect(pane.latestSettledTurn?.turnId).toBe('turn-settled');
  });

  it('switchThread tolerates malformed tokenUsageJson without crashing', async () => {
    setBindingMock('ListRecentTurns', async () => [
      {
        turnId: 'turn-1',
        threadId: 'thread-a',
        turnIndex: 0,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: '',
        tokenUsageJson: '{not valid json',
      },
    ]);

    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));

    expect(pane.latestSettledTurn?.tokenUsage).toBeNull();
  });

  it('switchThread tolerates a ListRecentTurns rejection', async () => {
    setBindingMock('ListRecentTurns', async () => {
      throw new Error('rpc down');
    });

    const pane = createThreadPane();
    // switchThread swallows the rehydration error so the thread still
    // renders its items.
    await pane.switchThread(makeThread({ id: 'thread-a' }));

    expect(pane.latestSettledTurn).toBeNull();
    expect(getActiveTurn(pane.threadId)).toBeNull();
    // Items path was not touched.
    expect(pane.thread?.id).toBe('thread-a');
  });

  it('switchThread clears turn state between threads', async () => {
    const pane = createThreadPane();
    pane.setActiveTurn({ turnId: 'turn-a', turnIndex: 0, startedAt: 1 });
    pane.settleTurn({
      turnId: 'turn-a-prev',
      turnIndex: -1,
      startedAt: 0,
      completedAt: 0,
      stopReason: 'end_turn',
      assistantMessageId: null,
      tokenUsage: null,
      aborted: false,
      errorMessage: '',
    });

    // Switching to a new thread with no recent turns must clear both
    // slots so the prior thread's state doesn't bleed over.
    await pane.switchThread(makeThread({ id: 'thread-b' }));

    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(pane.latestSettledTurn).toBeNull();
  });

  it('appendSubagentNotification records pass-through payloads, bounded', () => {
    const pane = createThreadPane();
    for (let i = 0; i < 40; i++) {
      pane.appendSubagentNotification({
        threadId: 'thread-1',
        meta: JSON.stringify({ agentId: `agent-${i}`, status: 'completed' }),
      });
    }
    // Bound should cap at 32 (subagentNotificationLimit). The newest
    // entry is at the tail; oldest entries have fallen off.
    expect(pane.subagentNotifications.length).toBe(32);
    expect(pane.subagentNotifications[pane.subagentNotifications.length - 1].meta)
      .toContain('agent-39');
    expect(pane.subagentNotifications[0].meta).toContain('agent-8');
  });
});
