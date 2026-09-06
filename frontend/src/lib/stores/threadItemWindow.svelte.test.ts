// stores/threadItemWindow.svelte.test.ts
//
// threadItemWindow.svelte.ts through the pane: upsert ordering and in-place
// replacement, what does and does not bump `timelineRevision`, the removal
// paths (by id, by turn, by revert kept-set) and the two whole-window
// signals — `rowUiRetentionRevision` and `activityRuns.wholesaleGeneration`
// — that a per-item write chokepoint structurally cannot see.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createThreadPane } from './thread.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import {
  buildPane,
  installThreadSwitchMocks,
  makeItem,
  makeThread,
} from '../../test/helpers/chat';
import { installThreadPaneTestEnv, nextFrame } from '../../test/helpers/threadPane';
import {
  clearAllThreadSizePriorsForTest,
  peekThreadSizePriorsForTest,
  setThreadSizePriors,
} from '../utils/virtual/priors';

describe('threadItemWindow', () => {
  beforeEach(installThreadPaneTestEnv);

  it('upsertItem inserts in turn/item order and replaces rows in place', () => {
    const pane = createThreadPane();

    pane.upsertItem(makeItem({ id: 'late', turnIndex: 1, itemIndex: 0 }));
    pane.upsertItem(makeItem({ id: 'early', turnIndex: 0, itemIndex: 1 }));
    pane.upsertItem(makeItem({ id: 'first', turnIndex: 0, itemIndex: 0 }));

    expect(pane.items.map((item) => item.id)).toEqual([
      'first',
      'early',
      'late',
    ]);

    pane.upsertItem(
      makeItem({ id: 'early', turnIndex: 0, itemIndex: 1, summary: 'updated' }),
    );

    expect(pane.items.map((item) => item.id)).toEqual([
      'first',
      'early',
      'late',
    ]);
    expect(pane.items.find((item) => item.id === 'early')?.summary).toBe(
      'updated',
    );
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

    expect(pane.items.map((item) => item.id)).toEqual([
      'first',
      'early',
      'late',
    ]);
    expect(pane.timelineRevision).toBe(1);

    pane.upsertItems([
      makeItem({ id: 'late', turnIndex: 0, itemIndex: 2, summary: 'moved' }),
      makeItem({ id: 'early', turnIndex: 0, itemIndex: 1, summary: 'updated' }),
    ]);

    expect(pane.items.map((item) => item.id)).toEqual([
      'first',
      'early',
      'late',
    ]);
    expect(pane.items.find((item) => item.id === 'late')?.summary).toBe(
      'moved',
    );
    expect(pane.timelineRevision).toBe(2);
  });

  it('bumps timeline revision when switchThread installs the initial item window', async () => {
    const pane = createThreadPane();
    setBindingMock('ListThreadSliceAround', async () => ({
      items: [
        makeItem({ id: 'loaded', threadId: 't', turnIndex: 0, itemIndex: 0 }),
      ],
      oldestTurnIndex: 0,
      hasMore: false,
    }));
    const initialRevision = pane.timelineRevision;

    await pane.switchThread(makeThread({ id: 't' }));

    expect(pane.items.map((item) => item.id)).toEqual(['loaded']);
    expect(pane.timelineRevision).toBeGreaterThan(initialRevision);
  });

  it('bumps timeline revision when switchThread restores a cached item window', async () => {
    const pane = createThreadPane();
    const loadCalls: string[] = [];
    setBindingMock('ListThreadSliceAround', async (threadId: unknown) => {
      const id = String(threadId);
      loadCalls.push(id);
      return {
        items: [
          makeItem({
            id: `${id}-row`,
            threadId: id,
            turnIndex: 0,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: 0,
        hasMore: false,
      };
    });

    await pane.switchThread(makeThread({ id: 't' }));
    await pane.switchThread(makeThread({ id: 'other' }));
    const revisionBeforeCacheRestore = pane.timelineRevision;

    await pane.switchThread(makeThread({ id: 't' }));

    // Three loads, not two: the cache hit paints synchronously and then
    // still asks SyncThreadWindow whether the window moved. The old
    // skip-on-cache-hit was a staleness hole (another attached client can
    // rewrite history while a thread sits in the LRU).
    expect(loadCalls).toEqual(['t', 'other', 't']);
    expect(pane.items.map((item) => item.id)).toEqual(['t-row']);
    expect(pane.timelineRevision).toBeGreaterThan(revisionBeforeCacheRestore);
  });

  it('keeps the incoming thread, cached window, and cursors paired when post-commit bookkeeping fails', async () => {
    const pane = createThreadPane();
    setBindingMock('ListThreadSliceAround', async (threadId: unknown) => {
      const id = String(threadId);
      return {
        items: [
          makeItem({
            id: `${id}-row`,
            threadId: id,
            turnIndex: id === 'cached' ? 7 : 2,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: id === 'cached' ? 7 : 2,
        newestTurnIndex: id === 'cached' ? 7 : 2,
        hasMore: id === 'cached',
        hasMoreOlder: id === 'cached',
        hasMoreNewer: false,
      };
    });
    await pane.switchThread(makeThread({ id: 'cached' }));
    await pane.switchThread(makeThread({ id: 'other' }));
    vi.spyOn(pane.activityRuns, 'noteWholesaleReplace')
      .mockImplementationOnce(() => {
        throw new Error('activity replacement bookkeeping failed');
      });

    await expect(
      pane.switchThread(makeThread({ id: 'cached' })),
    ).rejects.toThrow('timeline window replacement finalization failed');

    expect(pane.threadId).toBe('cached');
    expect(pane.items.map((item) => item.id)).toEqual(['cached-row']);
    expect(pane.oldestLoadedTurnIndex).toBe(7);
    expect(pane.newestLoadedTurnIndex).toBe(7);
    expect(pane.hasMoreHistory).toBe(true);
    expect(pane.hasMoreNewer).toBe(false);
    expect(pane.loading).toBe(false);
  });

  it('retains the committed window and cursor metadata when sync bookkeeping fails', async () => {
    const pane = createThreadPane();
    setBindingMock('ListThreadSliceAround', async () => ({
      items: [
        makeItem({
          id: 'loaded-row',
          threadId: 't',
          turnIndex: 4,
          itemIndex: 0,
        }),
      ],
      oldestTurnIndex: 4,
      newestTurnIndex: 4,
      hasMore: true,
      hasMoreOlder: true,
      hasMoreNewer: false,
    }));
    let replacements = 0;
    vi.spyOn(pane.activityRuns, 'noteWholesaleReplace').mockImplementation(
      () => {
        replacements += 1;
        if (replacements === 2) {
          throw new Error('synced page bookkeeping failed');
        }
      },
    );
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});

    await pane.switchThread(makeThread({ id: 't' }));

    expect(replacements).toBe(2);
    expect(pane.items.map((item) => item.id)).toEqual(['loaded-row']);
    expect(pane.oldestLoadedTurnIndex).toBe(4);
    expect(pane.newestLoadedTurnIndex).toBe(4);
    expect(pane.hasMoreHistory).toBe(true);
    expect(pane.hasMoreNewer).toBe(false);
    expect(consoleError).toHaveBeenCalled();

    consoleError.mockRestore();
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

  it('does not bump timeline revision for same-row Bash completion chrome', () => {
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'bash',
        kind: 'tool_call',
        status: 'running',
        toolName: 'Bash',
        summary: 'Bash: sleep 1',
        meta: JSON.stringify({ input: { command: 'sleep 1' } }),
      }),
    );
    const revision = pane.timelineRevision;

    pane.upsertItem(
      makeItem({
        id: 'bash',
        kind: 'tool_call',
        status: 'completed',
        toolName: 'Bash',
        summary: 'Bash: sleep 1',
        payloadId: 'payload-bash',
        payloadKind: 'command_output',
        payloadMeta: JSON.stringify({ command: 'sleep 1', exitCode: 0 }),
        meta: JSON.stringify({ input: { command: 'sleep 1' } }),
        updatedAt: 1,
      }),
    );

    expect(pane.items[0].status).toBe('completed');
    expect(pane.items[0].payloadKind).toBe('command_output');
    expect(pane.timelineRevision).toBe(revision);
  });

  it('does not bump timeline revision for collab-agent status-only chrome', () => {
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'agent',
        kind: 'tool_call',
        status: 'running',
        toolName: 'collab_agent',
        meta: JSON.stringify({
          input: { tool: 'spawn_agent', receiverThreadIds: ['child-1'] },
        }),
        payloadMeta: JSON.stringify({
          input: { newAgentNickname: 'Reviewer' },
        }),
      }),
    );
    const revision = pane.timelineRevision;

    pane.upsertItem(
      makeItem({
        id: 'agent',
        kind: 'tool_call',
        status: 'completed',
        toolName: 'collab_agent',
        meta: JSON.stringify({
          input: { tool: 'spawn_agent', receiverThreadIds: ['child-1'] },
        }),
        payloadMeta: JSON.stringify({
          input: { newAgentNickname: 'Reviewer' },
        }),
        updatedAt: 1,
      }),
    );

    expect(pane.items[0].status).toBe('completed');
    expect(pane.timelineRevision).toBe(revision);
  });

  it('bumps timeline revision when an upsert changes timeline structure', () => {
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'read',
        kind: 'tool_call',
        toolName: 'Read',
      }),
    );
    const revision = pane.timelineRevision;

    pane.upsertItem(
      makeItem({
        id: 'read',
        kind: 'tool_call',
        toolName: 'Edit',
      }),
    );

    expect(pane.timelineRevision).toBe(revision + 1);
  });

  it('preserves arrival order for rows with the same turn and item position', () => {
    const pane = createThreadPane();

    pane.upsertItems([
      makeItem({ id: 'later-position', turnIndex: 1, itemIndex: 0 }),
      makeItem({
        id: 'first-arrived',
        turnIndex: 0,
        itemIndex: 0,
        createdAt: 200,
      }),
      makeItem({
        id: 'second-arrived',
        turnIndex: 0,
        itemIndex: 0,
        createdAt: 100,
      }),
    ]);

    expect(pane.items.map((item) => item.id)).toEqual([
      'first-arrived',
      'second-arrived',
      'later-position',
    ]);
  });

  describe('size-priors eviction on item mutation', () => {
    // With the self-validating per-row nodeSignature key these evictions are
    // memory housekeeping (a stale row is refused on its own signature mismatch
    // anyway), but they free the entry immediately instead of waiting for the
    // LRU. Guard each call site so a future edit that drops one is caught.
    const seedEntry = {
      width: 0,
      expansionSig: '',
      rows: new Map([['seed', 42]]),
    };

    beforeEach(() => {
      clearAllThreadSizePriorsForTest();
    });

    it('evicts the priors when an item is removed by id', async () => {
      const pane = await buildPane(makeThread({ id: 't' }), [makeItem({ id: 'x', threadId: 't' })]);
      setThreadSizePriors('t', { ...seedEntry });
      expect(peekThreadSizePriorsForTest('t')).toBeTruthy();
      pane.removeItemById('x', 't');
      expect(peekThreadSizePriorsForTest('t')).toBeUndefined();
    });

    it('refuses a removal aimed at a thread the pane no longer holds', async () => {
      // Every caller of removeItemById reaches it across an await or an
      // event hop (the composer's failed-send rollback, the queue-restored
      // event), and `user:<n>` ids collide across threads by construction
      // — the same id names a different row in whatever thread is mounted
      // now. Without the expected-thread guard the rollback lands on the
      // wrong conversation and takes that thread's cached window with it.
      const pane = await buildPane(makeThread({ id: 't' }), [
        makeItem({ id: 'user:1', threadId: 't' }),
      ]);
      installThreadSwitchMocks(makeThread({ id: 'other' }), [
        makeItem({ id: 'user:1', threadId: 'other' }),
      ]);
      await pane.switchThread(makeThread({ id: 'other' }));
      setThreadSizePriors('other', { ...seedEntry });
      expect(pane.items.map((it) => it.id)).toEqual(['user:1']);

      expect(pane.removeItemById('user:1', 't')).toBeNull();

      expect(pane.items.map((it) => it.id)).toEqual(['user:1']);
      expect(peekThreadSizePriorsForTest('other')).toBeTruthy();
    });

    it('evicts the priors when a turn is truncated', async () => {
      const pane = await buildPane(makeThread({ id: 't' }), [
        makeItem({ id: 'x', threadId: 't', turnIndex: 1 }),
      ]);
      setThreadSizePriors('t', { ...seedEntry });
      pane.removeItemsFromTurn(1);
      expect(peekThreadSizePriorsForTest('t')).toBeUndefined();
    });

    it('evicts the priors on a same-thread reswitch', async () => {
      const pane = await buildPane(makeThread({ id: 't' }));
      setThreadSizePriors('t', { ...seedEntry });
      await pane.switchThread(makeThread({ id: 't' }));
      expect(peekThreadSizePriorsForTest('t')).toBeUndefined();
    });
  });

  describe('removeRevertedItems', () => {
    // Mirrors the `user_message:reverted` contract: turns after the anchor
    // turn always go; within the anchor turn only the event's kept-set
    // survives. See eventsMessageRevert.ts and DeleteConversationFromItem.
    const revertItems = (threadId: string) => [
      makeItem({ id: 'u0', threadId, turnIndex: 0, itemIndex: 0, kind: 'user_text', role: 'user' }),
      makeItem({ id: 'a0', threadId, turnIndex: 0, itemIndex: 1 }),
      makeItem({ id: 'prompt', threadId, turnIndex: 1, itemIndex: 0, kind: 'user_text', role: 'user' }),
      makeItem({ id: 'pre', threadId, turnIndex: 1, itemIndex: 1 }),
      makeItem({ id: 'anchor', threadId, turnIndex: 1, itemIndex: 2, kind: 'user_text', role: 'user' }),
      makeItem({ id: 'tail', threadId, turnIndex: 1, itemIndex: 3 }),
      makeItem({ id: 'u2', threadId, turnIndex: 2, itemIndex: 0, kind: 'user_text', role: 'user' }),
    ];

    it('degenerates to whole-turn removal when the kept-set is empty', async () => {
      const pane = await buildPane(makeThread({ id: 't-rr-empty' }), revertItems('t-rr-empty'));
      const removed = pane.removeRevertedItems(1, []);
      expect(removed.map((it) => it.id)).toEqual(['prompt', 'pre', 'anchor', 'tail', 'u2']);
      expect(pane.items.map((it) => it.id)).toEqual(['u0', 'a0']);
    });

    it('keeps exactly the listed anchor-turn survivors and drops later turns', async () => {
      const pane = await buildPane(makeThread({ id: 't-rr-kept' }), revertItems('t-rr-kept'));
      // Non-contiguous kept-set — the promoted-anchor shape: prompt + the
      // interrupted tail survive, the anchor between them goes.
      const removed = pane.removeRevertedItems(1, ['prompt', 'pre', 'tail']);
      expect(removed.map((it) => it.id)).toEqual(['anchor', 'u2']);
      expect(pane.items.map((it) => it.id)).toEqual(['u0', 'a0', 'prompt', 'pre', 'tail']);
    });

    it('removes pane-only anchor-turn rows absent from the kept-set', async () => {
      // A streamed row the backend never persisted (e.g. an in-flight
      // thinking block) cannot appear in any backend enumeration; the
      // kept-set formulation still removes it.
      const pane = await buildPane(makeThread({ id: 't-rr-ephemeral' }), [
        ...revertItems('t-rr-ephemeral'),
        makeItem({ id: 'ephemeral', threadId: 't-rr-ephemeral', turnIndex: 1, itemIndex: 4, kind: 'thinking' }),
      ]);
      const removed = pane.removeRevertedItems(1, ['prompt', 'pre']);
      expect(removed.map((it) => it.id)).toEqual(['anchor', 'tail', 'ephemeral', 'u2']);
      expect(pane.items.map((it) => it.id)).toEqual(['u0', 'a0', 'prompt', 'pre']);
    });

    it('is idempotent: a second application removes nothing', async () => {
      const pane = await buildPane(makeThread({ id: 't-rr-idem' }), revertItems('t-rr-idem'));
      pane.removeRevertedItems(1, ['prompt']);
      expect(pane.removeRevertedItems(1, ['prompt'])).toEqual([]);
    });
  });

  // The offscreen row-UI prune bails on an unchanged signature, and this
  // revision is the whole active-row leg of it. A missed bump strands
  // expansion state on rows the prune should have released; a gratuitous
  // one puts the retention collection back on every streamed row, which
  // is what it was extracted from. Transitions, not states — a pane that
  // bumps on entering a status but not on leaving it passes any
  // state-only suite.
  describe('rowUiRetentionRevision', () => {
    function streamingPane() {
      const pane = createThreadPane();
      pane.upsertItem(
        makeItem({ id: 'text', kind: 'assistant_text', status: 'streaming', summary: 'hel' }),
      );
      return pane;
    }

    it('starts at zero and bumps once for an appended active row', () => {
      const pane = createThreadPane();
      expect(pane.rowUiRetentionRevision).toBe(0);
      pane.upsertItem(makeItem({ id: 'settled', status: 'completed' }));
      expect(pane.rowUiRetentionRevision).toBe(0);
      pane.upsertItem(
        makeItem({ id: 'live', turnIndex: 1, kind: 'tool_call', status: 'running' }),
      );
      expect(pane.rowUiRetentionRevision).toBe(1);
    });

    it('holds still across streamed text on a live row', async () => {
      const pane = streamingPane();
      const revision = pane.rowUiRetentionRevision;

      pane.upsertItem(
        makeItem({
          id: 'text',
          kind: 'assistant_text',
          status: 'streaming',
          summary: 'hello',
          updatedAt: 1,
        }),
      );
      pane.applyItemDelta({
        threadId: 'thread-1',
        itemId: 'text',
        kind: 'assistant_text',
        delta: ' world',
        updatedAt: 2,
      });
      pane.__flushItemSmoothersForTest();
      await nextFrame();
      pane.applyItemMeta({
        threadId: 'thread-1',
        itemId: 'text',
        kind: 'assistant_text',
        meta: JSON.stringify({ pathRefs: [] }),
        updatedAt: 3,
      });

      expect(pane.items[0].summary).toContain('world');
      expect(pane.rowUiRetentionRevision).toBe(revision);
    });

    it('holds still across a non-smooth streamed delta', () => {
      const pane = createThreadPane();
      pane.upsertItem(
        makeItem({ id: 'out', kind: 'tool_call', status: 'streaming', summary: 'line' }),
      );
      const revision = pane.rowUiRetentionRevision;

      pane.applyItemDelta({
        threadId: 'thread-1',
        itemId: 'out',
        kind: 'tool_call',
        delta: '\nmore',
        updatedAt: 2,
      });

      expect(pane.items[0].summary).toBe('line\nmore');
      expect(pane.rowUiRetentionRevision).toBe(revision);
    });

    it('bumps when an upsert attaches a payload to a live row, and again when it settles', () => {
      const pane = createThreadPane();
      pane.upsertItem(makeItem({ id: 'bash', kind: 'tool_call', status: 'running' }));
      const revision = pane.rowUiRetentionRevision;

      pane.upsertItem(
        makeItem({
          id: 'bash',
          kind: 'tool_call',
          status: 'running',
          payloadId: 'payload-bash',
          payloadKind: 'command_output',
          updatedAt: 1,
        }),
      );
      expect(pane.rowUiRetentionRevision).toBe(revision + 1);

      pane.upsertItem(
        makeItem({
          id: 'bash',
          kind: 'tool_call',
          status: 'completed',
          payloadId: 'payload-bash',
          payloadKind: 'command_output',
          updatedAt: 2,
        }),
      );
      expect(pane.rowUiRetentionRevision).toBe(revision + 2);
    });

    it('bumps when a field patch settles a live row but not for a summary-only patch', () => {
      const pane = createThreadPane();
      pane.upsertItem(makeItem({ id: 'bash', kind: 'tool_call', status: 'running' }));
      const revision = pane.rowUiRetentionRevision;

      pane.applyItemPatch({
        threadId: 'thread-1',
        itemId: 'bash',
        kind: 'tool_call',
        patch: { summary: 'Bash: still working', updatedAt: 1 },
      });
      expect(pane.rowUiRetentionRevision).toBe(revision);

      pane.applyItemPatch({
        threadId: 'thread-1',
        itemId: 'bash',
        kind: 'tool_call',
        patch: { status: 'completed', updatedAt: 2 },
      });
      expect(pane.items[0].status).toBe('completed');
      expect(pane.rowUiRetentionRevision).toBe(revision + 1);
    });

    it('bumps when a settled row goes live again', () => {
      const pane = createThreadPane();
      pane.upsertItem(makeItem({ id: 'row', kind: 'tool_call', status: 'completed' }));
      const revision = pane.rowUiRetentionRevision;

      pane.applyItemPatch({
        threadId: 'thread-1',
        itemId: 'row',
        kind: 'tool_call',
        patch: { status: 'running', updatedAt: 1 },
      });

      expect(pane.rowUiRetentionRevision).toBe(revision + 1);
    });

    it('bumps on a wholesale replacement that evicts a live row', async () => {
      const pane = await buildPane(makeThread({ id: 't-retention' }), [
        makeItem({ id: 'u0', threadId: 't-retention', turnIndex: 0, itemIndex: 0, kind: 'user_text', role: 'user' }),
        makeItem({ id: 'live', threadId: 't-retention', turnIndex: 1, itemIndex: 0, status: 'running' }),
      ]);
      const revision = pane.rowUiRetentionRevision;

      expect(pane.removeItemsFromTurn(1).map((it) => it.id)).toEqual(['live']);

      expect(pane.rowUiRetentionRevision).toBeGreaterThan(revision);
    });
  });

  // The activity-run headers' third signal. Same shape as the retention
  // revision above and for the same reason: the per-item write chokepoint
  // cannot see a wholesale replacement, and a replacement can rewrite every
  // summary-relevant field under identical run membership.
  describe('activityRuns.wholesaleGeneration', () => {
    it('holds across per-item writes and moves on a wholesale replacement', async () => {
      const pane = await buildPane(makeThread({ id: 't-wholesale' }), [
        makeItem({
          id: 'u0',
          threadId: 't-wholesale',
          turnIndex: 0,
          itemIndex: 0,
          kind: 'user_text',
          role: 'user',
        }),
        makeItem({
          id: 'tool',
          threadId: 't-wholesale',
          turnIndex: 1,
          itemIndex: 0,
          kind: 'tool_call',
          status: 'running',
        }),
      ]);
      const generation = pane.activityRuns.wholesaleGeneration;

      // A per-item write feeds the per-run signal, not this one.
      pane.applyItemPatch({
        threadId: 't-wholesale',
        itemId: 'tool',
        kind: 'tool_call',
        patch: { status: 'completed', updatedAt: 1 },
      });
      expect(pane.activityRuns.wholesaleGeneration).toBe(generation);

      expect(pane.removeItemsFromTurn(1).map((it) => it.id)).toEqual(['tool']);

      expect(pane.activityRuns.wholesaleGeneration).toBeGreaterThan(generation);
    });
  });
});
