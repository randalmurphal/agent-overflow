import { isThreadWorking, projectSendStarted } from './threadStatuses.svelte';
import { beginUndoableSend, retireUndoableSend } from './composerSendUndo';
// Tests for the Stop-button revert-on-interrupt flow. The predicate
// is pure (in-memory state only); the helper drives the bindings
// mock to assert dispatch + rollback behavior.

import { describe, expect, it, beforeEach } from 'vitest';
import { createThreadPane } from './thread.svelte';
import {
  canRevertEarlyInterrupt,
  runInterrupt,
  runInterruptOrRevert,
} from './revertOnInterrupt.svelte';
import { TransportError } from '../transport/wsClient';
import { getActiveTurn, getThreadStatus, projectTurnCompleted } from './threadStatuses.svelte';
import {
  cancelBackgroundKillConfirmationForThread,
  pendingBackgroundKillConfirmation,
  resetForTest as resetBackgroundKillConfirmationForTest,
  resolveBackgroundKillConfirmation,
} from './backgroundKillConfirmation.svelte';
import { replaceSubagentRunStates, resetForTest as resetSubagentRunStatesForTest } from './subagentRunState.svelte';
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
      expect(result.reason).toBe('turn holds assistant_text');
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

describe('canRevertEarlyInterrupt turn contents', () => {
  beforeEach(() => {
    replaceQueueForThread('thread-1', []);
  });

  function rowOnTurn(kind: Item['kind'], overrides: Partial<Item> = {}): Item {
    return {
      id: `row:${kind}`,
      threadId: 'thread-1',
      turnIndex: 0,
      kind,
      role: 'assistant',
      status: 'completed',
      summary: '',
      payloadId: '',
      meta: '',
      createdAt: 0,
      updatedAt: 0,
      ...overrides,
    } as Item;
  }

  // Only the model's unfinished reasoning and request-level retry/error rows
  // may share a turn with the message; the backend predicate uses the same set.
  it.each([
    ['thinking', true],
    ['api_retry', true],
    ['api_error', true],
    ['error', true],
    ['assistant_text', false],
    ['tool_call', false],
    ['tool_completion', false],
    ['notification', false],
    ['compaction', false],
    ['compaction_reasoning', false],
    ['command_result', false],
    ['terminal_interaction', false],
  ] as const)('a %s row allows the un-send: %s', (kind, allowed) => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.upsertItem(rowOnTurn(kind));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    const result = canRevertEarlyInterrupt(pane, EMPTY_DRAFT);

    expect(result.canRevert).toBe(allowed);
    if (!result.canRevert) expect(result.reason).toBe(`turn holds ${kind}`);
  });

  it('rejects a turn holding a user row the reader did not send', () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.upsertItem(rowOnTurn('user_text', { id: 'echo:0', role: 'user', meta: '{"wire_only":true}' }));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    const result = canRevertEarlyInterrupt(pane, EMPTY_DRAFT);

    expect(result).toEqual({ canRevert: false, reason: 'turn holds user_text' });
  });

  // The reported sequence: Stop interrupted turn 0 while a background
  // command ran, the command finished, and Claude started a new round on
  // turn 0 to answer its notification. That round is active with only
  // thinking so far, but the message was committed when the turn settled.
  it('rejects a new round on a turn that already settled', () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'round-1', turnIndex: 0, startedAt: 1 });
    pane.settleTurn({
      turnId: 'round-1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'interrupted',
      assistantMessageId: null,
      tokenUsage: null,
      aborted: true,
      errorMessage: '',
    });
    pane.upsertItem(thinkingItem('think:0', 0));
    pane.setActiveTurn({ turnId: 'round-2', turnIndex: 0, startedAt: 3 });

    const result = canRevertEarlyInterrupt(pane, EMPTY_DRAFT);

    expect(result).toEqual({ canRevert: false, reason: 'turn already settled' });
  });

  it('allows the un-send when only an earlier turn has settled', () => {
    const pane = readyPane();
    pane.settleTurn({
      turnId: 'round-0',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
      assistantMessageId: null,
      tokenUsage: null,
      aborted: false,
      errorMessage: '',
    });
    pane.upsertItem(userItem('u:1', 1));
    pane.setActiveTurn({ turnId: 'round-1', turnIndex: 1, startedAt: 3 });

    const result = canRevertEarlyInterrupt(pane, EMPTY_DRAFT);

    expect(result.canRevert).toBe(true);
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

  it('ignores malformed attachments and scopes inherited draft attachments to this thread', async () => {
    const pane = readyPane();
    pane.upsertItem({
      ...userItem('u:0', 0),
      meta: JSON.stringify({
        attachments: [
          null,
          { id: 'inherited', threadId: 'source-thread', filename: 'x.png', mimeType: 'image/png', size: 1 },
          { id: '', threadId: 'thread-1', filename: 'x.png', mimeType: 'image/png', size: 1 },
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

    expect(draft.applied?.attachments.map((attachment) => [attachment.id, attachment.threadId]))
      .toEqual([['inherited', 'thread-1'], ['att-1', 'thread-1']]);
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
  it('restores draft, history and working presentation synchronously while preflight is unresolved', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'live', turnIndex: 0, startedAt: 1 });
    projectSendStarted('thread-1');
    const draft = optimisticDraftProbe();
    let resolveCount!: (count: number) => void;
    setBindingMock('CountRunningBackgroundTasks', () => new Promise<number>((resolve) => { resolveCount = resolve; }));
    setBindingMock('InterruptAndRevertIfClean', async () => successfulRevert());
    runInterruptOrRevert(pane, draft);
    expect(draft.applied?.content).toBe('hello');
    expect(pane.items).toHaveLength(0);
    expect(isThreadWorking('thread-1')).toBe(false);
    expect(isThreadInterruptPending('thread-1')).toBe(true);
    resolveCount(0);
    await flushInterruptFlow();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('cancels a prepared send before dispatch and restores its raw multiline draft', async () => {
    const pane = readyPane();
    const item = { ...userItem('optimistic:send', 0), meta: JSON.stringify({ sendId: 'send' }) };
    pane.upsertItem(item);
    const snapshot = { content: 'raw\n  text', attachments: [], terminalChips: [], sourceProposedPlan: null };
    const pending = beginUndoableSend('thread-1', 'send', snapshot);
    const draft = optimisticDraftProbe();
    runInterruptOrRevert(pane, draft);
    expect(pending.undoRequested).toBe(true);
    expect(draft.applied).toEqual(snapshot);
    expect(pane.items).toHaveLength(0);
    pending.finish('cancelled');
    await flushInterruptFlow();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
    retireUndoableSend('thread-1');
  });

  it('cannot restore removed rows into a pane switched during preflight', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'live', turnIndex: 0, startedAt: 1 });
    let resolveCount!: (count: number) => void;
    setBindingMock('CountRunningBackgroundTasks', () => new Promise<number>((resolve) => { resolveCount = resolve; }));
    runInterruptOrRevert(pane, EMPTY_DRAFT);
    const previous = pane.thread!;
    await pane.switchThread({ ...previous, id: 'thread-2' });
    resolveCount(1);
    await flushInterruptFlow();
    expect(pane.threadId).toBe('thread-2');
    expect(pane.items.some((item) => item.threadId === 'thread-1')).toBe(false);
  });

});

// A Claude interrupt kills every live async agent, so the backend refuses a
// Stop with `background_agents_running` until the person confirms
// (transport/backgroundKillRefusal.ts). The flow owns the optimistic clear:
// same tick when no listed agent is live, after the answer otherwise.
describe('runInterruptOrRevert with live background agents', () => {
  const agent = { launchItemId: 'tu-a', description: 'gate watcher', runState: 'parked', transcriptRootId: 'tu-a' };
  const refusal = () => new TransportError('background_agents_running', 'Stopping now would also stop 1 background agent.', {
    backgroundAgents: [agent],
  });

  beforeEach(() => {
    replaceQueueForThread('thread-1', []);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    resetThreadInterruptStateForTest();
    resetResendRevertMarkersForTest();
    resetBackgroundKillConfirmationForTest();
    resetSubagentRunStatesForTest();
    // The mounted pane hydrates live state; an unmocked read would land a
    // banner these tests assert stays empty.
    setBindingMock('GetThreadLiveState', async () => ({ threadId: 'thread-1', activeTurn: null }));
  });

  async function settle(): Promise<void> {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  }

  function stoppablePane() {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.upsertItem(assistantItem('a:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    return pane;
  }

  it('clears the working presentation in the same tick when no listed agent is live, and sends an unconfirmed interrupt', async () => {
    const pane = stoppablePane();
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => { calls.push(args); });

    expect(runInterruptOrRevert(pane, EMPTY_DRAFT)).toBe(false);
    expect(getActiveTurn('thread-1')).toBeNull();
    await settle();
    expect(calls).toEqual([['thread-1', false]]);
    expect(pendingBackgroundKillConfirmation()).toBeNull();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('keeps the turn running and asks when the backend refuses; "keep them" stops nothing', async () => {
    const pane = stoppablePane();
    replaceSubagentRunStates('thread-1', new Map([['tu-a', { state: 'parked', waitingOn: 1, report: null }]]));
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      if (args[1] === false) throw refusal();
    });

    expect(runInterruptOrRevert(pane, EMPTY_DRAFT)).toBe(false);
    // A listed live agent: no optimistic clear, the refusal is expected.
    expect(getActiveTurn('thread-1')?.turnId).toBe('turn-1');
    await settle();
    expect(calls).toEqual([['thread-1', false]]);
    expect(pendingBackgroundKillConfirmation()).toMatchObject({ threadId: 'thread-1', agents: [agent] });
    expect(getActiveTurn('thread-1')?.turnId).toBe('turn-1');
    // The question is not an interrupt in flight: the composer keeps its
    // Stop behind the dialog.
    expect(isThreadInterruptPending('thread-1')).toBe(false);

    resolveBackgroundKillConfirmation(false);
    await settle();
    expect(calls).toHaveLength(1);
    expect(getActiveTurn('thread-1')?.turnId).toBe('turn-1');
    expect(isThreadInterruptPending('thread-1')).toBe(false);
    expect(pane.generalError ?? '').toBe('');
  });

  it('"stop everything" clears the presentation and interrupts with the confirmation set', async () => {
    const pane = stoppablePane();
    replaceSubagentRunStates('thread-1', new Map([['tu-a', { state: 'running', waitingOn: 0, report: null }]]));
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      if (args[1] === false) throw refusal();
    });

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    await settle();
    expect(pendingBackgroundKillConfirmation()).not.toBeNull();
    resolveBackgroundKillConfirmation(true);
    await settle();
    expect(calls).toEqual([['thread-1', false], ['thread-1', true]]);
    expect(getActiveTurn('thread-1')).toBeNull();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
    expect(pane.generalError ?? '').toBe('');
  });

  it('puts the turn back when a Stop cleared early on a stale registry and was refused', async () => {
    const pane = stoppablePane();
    await settle();
    pane.clearGeneralError();
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      if (args[1] === false) throw refusal();
    });

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    expect(getActiveTurn('thread-1')).toBeNull();
    expect(getThreadStatus('thread-1')).toBe('interrupted');
    await settle();
    expect(getActiveTurn('thread-1')).toEqual({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    expect(getThreadStatus('thread-1')).toBe('running');
    expect(pane.generalError ?? '').toBe('');
    expect(pendingBackgroundKillConfirmation()?.threadId).toBe('thread-1');
    resolveBackgroundKillConfirmation(false);
    await settle();
    expect(calls).toEqual([['thread-1', false]]);
    expect(getActiveTurn('thread-1')?.turnId).toBe('turn-1');
  });

  it('asks nothing when the turn ended before the refusal arrived: there is nothing left to stop', async () => {
    const pane = stoppablePane();
    let reject!: (err: unknown) => void;
    setBindingMock('InterruptTurn', () => new Promise((_resolve, rejectRPC) => { reject = rejectRPC; }));

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    expect(getActiveTurn('thread-1')).toBeNull();
    projectTurnCompleted('thread-1', 'turn-1', { turnIndex: 0 });
    reject(refusal());
    await settle();

    expect(getActiveTurn('thread-1')).toBeNull();
    expect(getThreadStatus('thread-1')).toBe('idle');
    expect(pendingBackgroundKillConfirmation()).toBeNull();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('settles the question as "keep them" when the turn completes while it is open', async () => {
    const pane = stoppablePane();
    replaceSubagentRunStates('thread-1', new Map([['tu-a', { state: 'running', waitingOn: 0, report: null }]]));
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      if (args[1] === false) throw refusal();
    });
    runInterruptOrRevert(pane, EMPTY_DRAFT);
    await settle();
    expect(pendingBackgroundKillConfirmation()).not.toBeNull();

    projectTurnCompleted('thread-1', 'turn-1', { turnIndex: 0 });
    await settle();
    expect(pendingBackgroundKillConfirmation()).toBeNull();
    expect(calls).toHaveLength(1);
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('any other failure keeps the optimistic stop and reports it', async () => {
    const pane = stoppablePane();
    await settle();
    pane.clearGeneralError();
    setBindingMock('InterruptTurn', async () => { throw new Error('boom: provider crashed'); });

    runInterruptOrRevert(pane, EMPTY_DRAFT);
    await settle();

    expect(getActiveTurn('thread-1')).toBeNull();
    expect(pendingBackgroundKillConfirmation()).toBeNull();
    expect(pane.generalError ?? '').not.toBe('');
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('turns an un-send into a plain, confirmed Stop when the revert is refused: the message stays', async () => {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    const revertCalls: unknown[][] = [];
    setBindingMock('InterruptAndRevertIfClean', async (...args: unknown[]) => {
      revertCalls.push(args);
      throw refusal();
    });
    const interruptCalls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => { interruptCalls.push(args); });

    expect(runInterruptOrRevert(pane, EMPTY_DRAFT)).toBe(true);
    await Promise.resolve();
    expect(pane.items.find((i) => i.id === 'u:0')).toBeUndefined();
    await settle();
    expect(revertCalls).toHaveLength(1);
    expect(revertCalls[0][2]).toBe(false);
    // The row is back and the person is asked; the un-send is off the table.
    expect(pane.items.find((i) => i.id === 'u:0')).toBeDefined();
    expect(pendingBackgroundKillConfirmation()?.agents).toEqual([agent]);
    resolveBackgroundKillConfirmation(true);
    await settle();
    expect(interruptCalls).toEqual([['thread-1', true]]);
    expect(pane.items.find((i) => i.id === 'u:0')).toBeDefined();
    expect(getActiveTurn('thread-1')).toBeNull();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
    expect(pane.generalError ?? '').toBe('');
  });

  it('runInterrupt (the mid-turn cancel arms) gates the same way', async () => {
    const pane = stoppablePane();
    replaceSubagentRunStates('thread-1', new Map([['tu-a', { state: 'running', waitingOn: 0, report: null }]]));
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      if (args[1] === false) throw refusal();
    });
    runInterrupt(pane);
    expect(getActiveTurn('thread-1')?.turnId).toBe('turn-1');
    await settle();
    expect(pendingBackgroundKillConfirmation()?.threadId).toBe('thread-1');
    resolveBackgroundKillConfirmation(true);
    await settle();
    expect(calls).toEqual([['thread-1', false], ['thread-1', true]]);
    expect(getActiveTurn('thread-1')).toBeNull();
  });

  it('a torn-down thread settles its open question as "keep them" and sends nothing more', async () => {
    const pane = stoppablePane();
    replaceSubagentRunStates('thread-1', new Map([['tu-a', { state: 'running', waitingOn: 0, report: null }]]));
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      if (args[1] === false) throw refusal();
    });
    runInterruptOrRevert(pane, EMPTY_DRAFT);
    await settle();
    expect(pendingBackgroundKillConfirmation()).not.toBeNull();
    cancelBackgroundKillConfirmationForThread('thread-1');
    await settle();
    expect(calls).toHaveLength(1);
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });
});
