// Tests for the Stop-button revert-on-interrupt flow. The predicate
// is pure (in-memory state only); the helper drives the bindings
// mock to assert dispatch + rollback behavior.

import { describe, expect, it, beforeEach } from 'vitest';
import { createThreadPane } from './thread.svelte';
import {
  canRevertEarlyInterrupt,
  runInterruptOrRevert,
} from './revertOnInterrupt.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { replaceQueueForThread } from './sendQueue.svelte';
import type { Item, Thread } from '../types/models';
import type { ComposerDraftSnapshot } from './composerDraftSnapshots';
import { resetResendRevertMarkersForTest } from './eventsMessageRevert';
import {
  isThreadInterruptPending,
  resetThreadInterruptStateForTest,
} from './threadInterruptState.svelte';

const EMPTY_DRAFT = {
  content: '',
  attachments: [] as { length: number },
  terminalChips: [] as { length: number },
};

function optimisticDraftProbe() {
  let applied: ComposerDraftSnapshot | null = null;
  let cleared: ComposerDraftSnapshot | null = null;
  return {
    get content() { return ''; },
    attachments: [] as { length: number },
    terminalChips: [] as { length: number },
    get applied() { return applied; },
    get cleared() { return cleared; },
    applyOptimisticRestoredDraft(_threadId: string, snapshot: ComposerDraftSnapshot): void {
      applied = snapshot;
    },
    clearOptimisticRestoredDraft(_threadId: string, snapshot: ComposerDraftSnapshot): void {
      cleared = snapshot;
    },
  };
}

function readyPane(threadId = 'thread-1'): ReturnType<typeof createThreadPane> {
  setBindingMock('SwitchThread', async (id: unknown) => ({
    id: typeof id === 'string' ? id : threadId,
  }));
  setBindingMock('ListRecentThreadItems', async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  setBindingMock('ListPendingInteractiveRequests', async () => ({
    approvals: [],
    userInputs: [],
  }));
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListThreadSliceAround', async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  const pane = createThreadPane();
  const thread: Thread = {
    id: threadId,
    title: 'Test thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
  void pane.switchThread(thread);
  return pane;
}

function userItem(id: string, turnIndex: number, threadId = 'thread-1'): Item {
  return {
    id,
    threadId,
    turnIndex,
    kind: 'user_text',
    role: 'user',
    status: 'completed',
    summary: 'hello',
    payloadId: '',
    meta: '',
    createdAt: 0,
    updatedAt: 0,
  } as Item;
}

function assistantItem(id: string, turnIndex: number, threadId = 'thread-1'): Item {
  return {
    id,
    threadId,
    turnIndex,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'streaming',
    summary: 'thinking aloud',
    payloadId: '',
    meta: '',
    createdAt: 0,
    updatedAt: 0,
  } as Item;
}

function thinkingItem(id: string, turnIndex: number, threadId = 'thread-1'): Item {
  return {
    id,
    threadId,
    turnIndex,
    kind: 'thinking',
    role: 'assistant',
    status: 'streaming',
    summary: 'thinking...',
    payloadId: '',
    meta: '',
    createdAt: 0,
    updatedAt: 0,
  } as Item;
}

function successfulRevert(userItemId = 'u:0', turnIndex = 0) {
  return {
    reverted: true,
    userItemId,
    turnIndex,
    historyEpoch: 1,
    historyRev: 1,
  };
}

describe('canRevertEarlyInterrupt', () => {
  beforeEach(() => {
    replaceQueueForThread('thread-1', []);
  });

  it('returns canRevert=true when the turn holds a single user_text', () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    const result = canRevertEarlyInterrupt(pane, EMPTY_DRAFT);

    expect(result.canRevert).toBe(true);
    if (result.canRevert) {
      expect(result.userItem.id).toBe('u:0');
    }
  });

  it('rejects when there is no active turn (Stop after settle)', () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));

    const result = canRevertEarlyInterrupt(pane, EMPTY_DRAFT);

    expect(result.canRevert).toBe(false);
    if (!result.canRevert) {
      expect(result.reason).toBe('no active turn');
    }
  });

  it('rejects when the composer carries new typing', () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    const result = canRevertEarlyInterrupt(pane, {
      content: 'next thought',
      attachments: { length: 0 },
      terminalChips: { length: 0 },
    });

    expect(result.canRevert).toBe(false);
    if (!result.canRevert) {
      expect(result.reason).toBe('composer not empty');
    }
  });

  it('rejects when the send queue has pending items (steer / follow-up)', () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    replaceQueueForThread('thread-1', [
      {
        id: 'queue:1',
        threadId: 'thread-1',
        message: 'follow-up',
        attachmentIds: [],
        enqueuedAt: 1,
      },
    ]);

    const result = canRevertEarlyInterrupt(pane, EMPTY_DRAFT);

    expect(result.canRevert).toBe(false);
    if (!result.canRevert) {
      expect(result.reason).toBe('queue has pending items');
    }
  });

  it('rejects when an assistant_text row exists in the active turn', () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.upsertItem(assistantItem('a:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    const result = canRevertEarlyInterrupt(pane, EMPTY_DRAFT);

    expect(result.canRevert).toBe(false);
    if (!result.canRevert) {
      expect(result.reason).toBe('agent has responded');
    }
  });

  it('allows revert when only a thinking row sits with the user_text', () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.upsertItem(thinkingItem('think:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    const result = canRevertEarlyInterrupt(pane, EMPTY_DRAFT);

    expect(result.canRevert).toBe(true);
    if (result.canRevert) {
      expect(result.userItem.id).toBe('u:0');
    }
  });
});

describe('runInterruptOrRevert', () => {
  beforeEach(() => {
    replaceQueueForThread('thread-1', []);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    resetThreadInterruptStateForTest();
    resetResendRevertMarkersForTest();
  });

  async function flushInterruptFlow(): Promise<void> {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  }

  it('dispatches InterruptAndRevertIfClean and leaves the row removed on success', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    let resolveRevert: (() => void) | undefined;
    const revertCalls: string[] = [];
    setBindingMock('InterruptAndRevertIfClean', (id: unknown) => {
      revertCalls.push(id as string);
      return new Promise((resolve) => {
        resolveRevert = () => resolve(successfulRevert());
      });
    });
    setBindingMock('InterruptTurn', async () => {
      throw new Error('InterruptTurn should not be called on the revert path');
    });

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    expect(isThreadInterruptPending('thread-1')).toBe(true);
    await Promise.resolve();
    expect(pane.items.find((i) => i.id === 'u:0')).toBeUndefined();
    expect(isThreadInterruptPending('thread-1')).toBe(true);
    resolveRevert?.();
    await flushInterruptFlow();

    expect(revertCalls).toEqual(['thread-1']);
    // Row stays removed on Reverted=true (event handler refreshes draft).
    expect(pane.items.find((i) => i.id === 'u:0')).toBeUndefined();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('restores the interrupted user message into the draft after the background preflight', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    const draft = optimisticDraftProbe();

    setBindingMock('InterruptAndRevertIfClean', async () => successfulRevert());

    runInterruptOrRevert(pane, draft);
    await flushInterruptFlow();

    expect(pane.items.find((i) => i.id === 'u:0')).toBeUndefined();
    expect(draft.applied?.content).toBe('hello');
    expect(draft.applied?.attachments).toEqual([]);
  });

  it('restores attachment and source-plan metadata from the interrupted user item', async () => {
    const pane = readyPane();
    pane.upsertItem({
      ...userItem('u:0', 0),
      summary: 'implement this',
      meta: JSON.stringify({
        attachments: [
          {
            id: 'att-1',
            threadId: 'thread-1',
            filename: 'shot.png',
            mimeType: 'image/png',
            size: 123,
          },
        ],
        sourceProposedPlan: {
          threadId: 'src-thread',
          itemId: 'plan-1',
          payloadId: 'payload-1',
          title: 'Plan',
        },
      }),
    });
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    const draft = optimisticDraftProbe();

    setBindingMock('InterruptAndRevertIfClean', async () => successfulRevert());

    runInterruptOrRevert(pane, draft);
    await flushInterruptFlow();

    expect(draft.applied?.content).toBe('implement this [Image #1]');
    expect(draft.applied?.attachments).toEqual([
      expect.objectContaining({
        id: 'att-1',
        threadId: 'thread-1',
        filename: 'shot.png',
        mimeType: 'image/png',
        size: 123,
      }),
    ]);
    expect(draft.applied?.sourceProposedPlan).toEqual({
      threadId: 'src-thread',
      itemId: 'plan-1',
      payloadId: 'payload-1',
      title: 'Plan',
    });
  });

  it('ignores malformed attachment metadata during optimistic draft restore', async () => {
    const pane = readyPane();
    pane.upsertItem({
      ...userItem('u:0', 0),
      meta: JSON.stringify({
        attachments: [
          null,
          { id: 'cross-thread', threadId: 'other-thread', filename: 'x.png', mimeType: 'image/png', size: 1 },
          { id: 'bad-mime', threadId: 'thread-1', filename: 'x.txt', mimeType: 'text/plain', size: 1 },
          { id: 'att-1', threadId: 'thread-1', filename: 'shot.png', mimeType: 'image/png', size: 123 },
        ],
      }),
    });
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    const draft = optimisticDraftProbe();

    setBindingMock('InterruptAndRevertIfClean', async () => successfulRevert());

    runInterruptOrRevert(pane, draft);
    await flushInterruptFlow();

    expect(draft.applied?.attachments.map((attachment) => attachment.id)).toEqual(['att-1']);
  });

  it('restores the optimistic row removal when the backend declines the revert', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    setBindingMock('InterruptAndRevertIfClean', async () => ({
      reverted: false,
      reason: 'agent content present',
    }));

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    await Promise.resolve();
    // Optimistic remove still happens after the background preflight; the test exercises
    // the rollback that lands after the RPC resolves with Reverted=false.
    expect(pane.items.find((i) => i.id === 'u:0')).toBeUndefined();
    await flushInterruptFlow();

    expect(pane.items.find((i) => i.id === 'u:0')).toBeDefined();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('clears the optimistic draft restore when the backend declines the revert', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    const draft = optimisticDraftProbe();

    setBindingMock('InterruptAndRevertIfClean', async () => ({
      reverted: false,
      reason: 'agent content present',
    }));

    runInterruptOrRevert(pane, draft);
    await flushInterruptFlow();

    expect(draft.applied?.content).toBe('hello');
    expect(draft.cleared).toEqual(draft.applied);
  });

  it('falls back to InterruptTurn when the frontend predicate fails', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.upsertItem(assistantItem('a:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    const interruptCalls: string[] = [];
    setBindingMock('InterruptTurn', async (id: unknown) => {
      interruptCalls.push(id as string);
    });
    setBindingMock('InterruptAndRevertIfClean', async () => {
      throw new Error('InterruptAndRevertIfClean should not be called when predicate is false');
    });

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    await flushInterruptFlow();

    expect(interruptCalls).toEqual(['thread-1']);
    expect(isThreadInterruptPending('thread-1')).toBe(false);
    // The user_text + assistant_text rows are untouched on the
    // fallback path.
    expect(pane.items.find((i) => i.id === 'u:0')).toBeDefined();
    expect(pane.items.find((i) => i.id === 'a:0')).toBeDefined();
  });

  it('restores the row when the backend RPC rejects', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    setBindingMock('InterruptAndRevertIfClean', async () => {
      throw new Error('boom: provider crashed');
    });

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    await Promise.resolve();
    expect(pane.items.find((i) => i.id === 'u:0')).toBeUndefined();
    await flushInterruptFlow();

    expect(pane.items.find((i) => i.id === 'u:0')).toBeDefined();
    // userFacingError rewrites the raw "boom" message into a friendlier
    // surface; just assert that *something* lands so the user sees the
    // failure rather than silently rolling back.
    expect(pane.generalError ?? '').not.toBe('');
  });

  it('clears the optimistic draft restore when the backend RPC rejects', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    const draft = optimisticDraftProbe();

    setBindingMock('InterruptAndRevertIfClean', async () => {
      throw new Error('boom: provider crashed');
    });

    runInterruptOrRevert(pane, draft);
    await flushInterruptFlow();

    expect(draft.applied?.content).toBe('hello');
    expect(draft.cleared).toEqual(draft.applied);
  });

  it('keeps Send closed when a successful response cannot identify its committed cut', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    setBindingMock('InterruptAndRevertIfClean', async () => ({
      ...successfulRevert(),
      historyEpoch: 0,
      historyRev: 0,
    }));

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    await flushInterruptFlow();

    expect(isThreadInterruptPending('thread-1')).toBe(true);
    expect(pane.generalError ?? '').not.toBe('');
  });

  // The user_text isn't the only kind on the latest turn — thinking,
  // api_retry, and error rows can sit there too (they don't block the
  // predicate). Backend `DeleteConversationFromTurn` is inclusive, so
  // the optimistic remove must wipe ALL of them; otherwise they strand
  // in pane.items without a backing SQLite row and re-appear stamped
  // " — interrupted" after the truncated turn-complete fires.
  it('optimistically truncates every item on the active turn, not just the user row', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.upsertItem(thinkingItem('think:0:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    setBindingMock('InterruptAndRevertIfClean', async () => successfulRevert());

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    await flushInterruptFlow();

    expect(pane.items.find((i) => i.id === 'u:0')).toBeUndefined();
    expect(pane.items.find((i) => i.id === 'think:0:0')).toBeUndefined();
  });

  it('rolls back every truncated item when the backend declines the revert', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.upsertItem(thinkingItem('think:0:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    setBindingMock('InterruptAndRevertIfClean', async () => ({
      reverted: false,
      reason: 'agent content present',
    }));

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    await Promise.resolve();
    expect(pane.items.find((i) => i.id === 'u:0')).toBeUndefined();
    expect(pane.items.find((i) => i.id === 'think:0:0')).toBeUndefined();
    await flushInterruptFlow();

    expect(pane.items.find((i) => i.id === 'u:0')).toBeDefined();
    expect(pane.items.find((i) => i.id === 'think:0:0')).toBeDefined();
  });

  it('does not touch items on earlier turns when truncating', async () => {
    const pane = readyPane();
    // Prior settled turn — must survive the revert.
    pane.upsertItem(userItem('u:0', 0));
    pane.upsertItem(assistantItem('a:0', 0));
    // Active turn with a thinking sibling.
    pane.upsertItem(userItem('u:1', 1));
    pane.upsertItem(thinkingItem('think:1:0', 1));
    pane.setActiveTurn({ turnId: 'turn-2', turnIndex: 1, startedAt: 2 });

    setBindingMock('InterruptAndRevertIfClean', async () => successfulRevert('u:1', 1));

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    await flushInterruptFlow();

    expect(pane.items.find((i) => i.id === 'u:0')).toBeDefined();
    expect(pane.items.find((i) => i.id === 'a:0')).toBeDefined();
    expect(pane.items.find((i) => i.id === 'u:1')).toBeUndefined();
    expect(pane.items.find((i) => i.id === 'think:1:0')).toBeUndefined();
  });

  it('falls back to InterruptTurn without reverting when a tray task is running', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    setBindingMock('CountRunningBackgroundTasks', async () => 1);
    const interruptCalls: string[] = [];
    setBindingMock('InterruptTurn', async (id: unknown) => {
      interruptCalls.push(id as string);
    });
    const revert = setBindingMock('InterruptAndRevertIfClean', async () => ({
      reverted: true,
      userItemId: 'u:0',
      turnIndex: 0,
    }));

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    await flushInterruptFlow();

    expect(interruptCalls).toEqual(['thread-1']);
    expect(revert).not.toHaveBeenCalled();
    expect(pane.items.find((i) => i.id === 'u:0')).toBeDefined();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });
});
