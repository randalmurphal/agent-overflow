import { beforeEach, describe, expect, it } from 'vitest';
import { createThreadPane } from './thread.svelte';
import type { Item } from '../types/models';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { makeItem, makeThread } from '../../test/helpers/chat';

describe('createThreadPane', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('SwitchThread', async () => {});
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
    expect(pane.isTurnActive).toBe(false);
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
    });
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

    await pane.switchThread(makeThread({ id: 'thread-b' }));

    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.generalError).toBeNull();
    expect(pane.showTerminal).toBe(false);
    expect(pane.showPlanSidebar).toBe(false);
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

  it('derives isTurnActive strictly from activeTurn (invariant 22)', () => {
    // Post-refactor, isTurnActive comes solely from the wire-pushed
    // activeTurn slot. Item state (streaming text, running tool_calls,
    // pending approvals) no longer leaks into the flag.
    const pane = createThreadPane();

    expect(pane.isTurnActive).toBe(false);

    // A streaming assistant item alone doesn't flip the flag.
    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'streaming',
    }));
    expect(pane.isTurnActive).toBe(false);

    // A running foreground tool_call alone doesn't flip the flag either.
    pane.upsertItem(makeItem({
      id: 'tool-1',
      kind: 'tool_call',
      status: 'running',
      isBackground: false,
    }));
    expect(pane.isTurnActive).toBe(false);

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
    expect(pane.isTurnActive).toBe(false);

    // Wire-push flips it on.
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 1 });
    expect(pane.isTurnActive).toBe(true);

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
    expect(pane.isTurnActive).toBe(false);
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
    expect(pane.turnDiffViews.size).toBe(0);
  });

  it('upsertItem builds turnDiffViews incrementally per affected turn', () => {
    const pane = createThreadPane();

    // Non-diff items don't seed a turn entry.
    pane.upsertItem(makeItem({ id: 'user:0', turnIndex: 0, kind: 'user_text', summary: 'hi' }));
    expect(pane.turnDiffViews.size).toBe(0);

    pane.upsertItem(makeItem({
      id: 'diff-0',
      turnIndex: 0,
      itemIndex: 1,
      kind: 'tool_call',
      payloadId: 'p0',
      payloadKind: 'diff',
      payloadMeta: JSON.stringify({
        filePath: 'a.ts',
        changeKind: 'modified',
        insertions: 3,
        deletions: 1,
        preview: '',
      }),
    }));

    expect(pane.turnDiffViews.get(0)).toEqual({
      files: [{
        path: 'a.ts',
        insertions: 3,
        deletions: 1,
        kind: 'modified',
        payloadId: 'p0',
      }],
      summary: { insertions: 3, deletions: 1, fileCount: 1 },
    });

    // Turn 1 entry is independent.
    pane.upsertItem(makeItem({
      id: 'tool-1',
      turnIndex: 1,
      itemIndex: 0,
      kind: 'tool_call',
      payloadId: 'p1',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({
        inlineDiff: {
          files: [
            { path: 'b.ts', insertions: 5, deletions: 2, kind: 'modified' },
          ],
        },
      }),
    }));

    expect(pane.turnDiffViews.get(1)?.summary).toEqual({
      insertions: 5,
      deletions: 2,
      fileCount: 1,
    });
    // Turn 0 untouched by turn 1's upsert.
    expect(pane.turnDiffViews.get(0)?.summary).toEqual({
      insertions: 3,
      deletions: 1,
      fileCount: 1,
    });
  });

  it('upsertItem refreshes the affected turn on replace', () => {
    const pane = createThreadPane();

    pane.upsertItem(makeItem({
      id: 'diff-0',
      turnIndex: 0,
      kind: 'tool_call',
      payloadId: 'p0',
      payloadKind: 'diff',
      payloadMeta: JSON.stringify({
        filePath: 'a.ts',
        changeKind: 'modified',
        insertions: 1,
        deletions: 0,
        preview: '',
      }),
    }));
    expect(pane.turnDiffViews.get(0)?.summary.insertions).toBe(1);

    // Replace the same id with a new payload meta (e.g. completion swap).
    pane.upsertItem(makeItem({
      id: 'diff-0',
      turnIndex: 0,
      kind: 'tool_call',
      payloadId: 'p0',
      payloadKind: 'diff',
      payloadMeta: JSON.stringify({
        filePath: 'a.ts',
        changeKind: 'modified',
        insertions: 9,
        deletions: 2,
        preview: '',
      }),
    }));
    expect(pane.turnDiffViews.get(0)?.summary).toEqual({
      insertions: 9,
      deletions: 2,
      fileCount: 1,
    });
  });

  it('clears the turnDiffViews entry when replace removes the turn\'s last diff', () => {
    const pane = createThreadPane();

    pane.upsertItem(makeItem({
      id: 'diff-0',
      turnIndex: 0,
      kind: 'tool_call',
      payloadId: 'p0',
      payloadKind: 'diff',
      payloadMeta: JSON.stringify({
        filePath: 'a.ts',
        changeKind: 'modified',
        insertions: 3,
        deletions: 1,
        preview: '',
      }),
    }));
    expect(pane.turnDiffViews.has(0)).toBe(true);

    // Replace the diff with a plain non-diff item under the same id.
    pane.upsertItem(makeItem({
      id: 'diff-0',
      turnIndex: 0,
      kind: 'assistant_text',
      summary: 'changed shape',
    }));

    expect(pane.turnDiffViews.has(0)).toBe(false);
  });

  it('switchThread seeds turnDiffViews from the loaded items', async () => {
    const pane = createThreadPane();
    const items = [
      makeItem({
        id: 'tool-0',
        turnIndex: 0,
        kind: 'tool_call',
        payloadId: 'p0',
        payloadKind: 'diff',
        payloadMeta: JSON.stringify({
          filePath: 'a.ts',
          changeKind: 'modified',
          insertions: 2,
          deletions: 0,
          preview: '',
        }),
      }),
    ];
    setBindingMock('ListRecentThreadItems', async () => ({
      items,
      oldestTurnIndex: 0,
      hasMore: false,
    }));

    await pane.switchThread(makeThread({ id: 'thread-a' }));

    expect(pane.turnDiffViews.get(0)?.summary).toEqual({
      insertions: 2,
      deletions: 0,
      fileCount: 1,
    });
  });

  describe('windowed history', () => {
    it('upsertItem drops new items below the window floor', async () => {
      const pane = createThreadPane();
      const seed: Item[] = [
        makeItem({ id: 'at-floor', turnIndex: 5, itemIndex: 0 }),
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
      pane.upsertItem(makeItem({ id: 'below', turnIndex: 2, itemIndex: 0 }));
      expect(pane.items.map((it) => it.id)).toEqual(['at-floor']);
    });

    it('upsertItem still accepts replacements for known ids below the floor', async () => {
      const pane = createThreadPane();
      const seed: Item[] = [
        makeItem({ id: 'known', turnIndex: 5, itemIndex: 0, summary: 'old' }),
      ];
      setBindingMock('ListRecentThreadItems', async () => ({
        items: seed,
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));

      // Known id, turn below floor — cross-turn correction path. Must
      // still replace because the id is clearly in-window already.
      pane.upsertItem(makeItem({ id: 'known', turnIndex: 2, itemIndex: 0, summary: 'new' }));
      expect(pane.items.find((it) => it.id === 'known')?.summary).toBe('new');
    });

    it('loadOlder prepends older items and updates the floor + hasMore', async () => {
      const pane = createThreadPane();
      const tail: Item[] = [
        makeItem({ id: 't5', turnIndex: 5, itemIndex: 0 }),
      ];
      setBindingMock('ListRecentThreadItems', async () => ({
        items: tail,
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [
          makeItem({ id: 't3', turnIndex: 3, itemIndex: 0 }),
          makeItem({ id: 't4', turnIndex: 4, itemIndex: 0 }),
        ],
        oldestTurnIndex: 3,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      await pane.loadOlder();

      expect(pane.items.map((it) => it.id)).toEqual(['t3', 't4', 't5']);
      expect(pane.oldestLoadedTurnIndex).toBe(3);
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.loadingOlder).toBe(false);
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
      await pane.loadOlder();
      expect(calls).toBe(0);
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
        items: [makeItem({ id: 'here', turnIndex: 5 })],
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
        items: [makeItem({ id: 't5', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('GetThreadItem', async (_threadId: string, itemId: string) =>
        itemId === 'target' ? makeItem({ id: 'target', turnIndex: 1 }) : null,
      );
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [
          makeItem({ id: 'target', turnIndex: 1 }),
          makeItem({ id: 't2', turnIndex: 2 }),
          makeItem({ id: 't3', turnIndex: 3 }),
          makeItem({ id: 't4', turnIndex: 4 }),
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
      pane.requestScrollToItem('b');
      expect(pane.scrollToItemRequest.nonce).toBeGreaterThan(second);
      expect(pane.scrollToItemRequest.itemId).toBe('b');
    });
  });

  // --- Turn-lifecycle pane state (Wave 2) -----------------------------------

  it('setActiveTurn populates activeTurn and flips isTurnActive on', () => {
    const pane = createThreadPane();
    expect(pane.activeTurn).toBeNull();
    expect(pane.isTurnActive).toBe(false);

    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });

    expect(pane.activeTurn).toEqual({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });
    expect(pane.isTurnActive).toBe(true);
  });

  it('setActiveTurn is idempotent by turnId — preserves startedAt on re-emit', () => {
    // A Claude re-init / interrupt can re-send EventTurnStart for the same
    // (thread, turn). The pane must not rewind startedAt — otherwise the
    // working indicator's elapsed-seconds counter would jump backward each
    // time the provider re-initialises.
    const pane = createThreadPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 9999 });
    expect(pane.activeTurn?.startedAt).toBe(1000);
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

    expect(pane.activeTurn).toBeNull();
    expect(pane.isTurnActive).toBe(false);
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
    expect(pane.activeTurn).toBeNull();
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
    expect(pane.activeTurn).toBeNull();
    expect(pane.isTurnActive).toBe(false);
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
    expect(pane.activeTurn).toBeNull();
    expect(pane.isTurnActive).toBe(false);
    // But the prior settled turn IS rehydrated so the completion divider
    // can still render.
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
    expect(pane.activeTurn).toBeNull();
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

    expect(pane.activeTurn).toBeNull();
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
