// The `user_message:reverted` fan-out, focused on the one branch the
// edit-and-resend saga added: whether the composer rehydrates. The
// truncation itself is covered end-to-end in events.test.ts.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  applyUserMessageReverted,
  consumeResendRevertMarker,
  resetResendRevertMarkersForTest,
} from './eventsMessageRevert';
import {
  registerComposerDraft,
  resetComposerDraftRegistryForTest,
} from './composerDraftRegistry.svelte';
import type { ComposerDraftStore } from './composerDraft.svelte';
import { resetPanesForTest } from './panes.svelte';
import { resetForTest as resetThreadStatuses } from './threadStatuses.svelte';
import {
  isThreadWorking,
  projectSendStarted,
} from './threadStatuses.svelte';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { resetBindingMocks } from '../../test/mocks/bindings-app';
import {
  __resetThreadHistoryStampsForTest,
  getThreadHistoryStamp,
  recordAttestedStamp,
} from './threadHistoryStamps';
import { threadItemCache } from './threadItemCache';
import type { ThreadPane } from './thread.svelte';
import {
  beginThreadInterrupt,
  finishThreadInterrupt,
  isThreadInterruptPending,
  resetThreadInterruptStateForTest,
} from './threadInterruptState.svelte';

function stubDraft(): { draft: ComposerDraftStore; reloadFromBackend: ReturnType<typeof vi.fn> } {
  const reloadFromBackend = vi.fn(async () => {});
  return {
    reloadFromBackend,
    draft: { reloadFromBackend } as unknown as ComposerDraftStore,
  };
}

async function seedPane(): Promise<ThreadPane> {
  const pane = await buildPane(makeThread({ id: 'thread-a' }));
  pane.upsertItems([
    makeItem({ id: 'u:0', threadId: 'thread-a', turnIndex: 0, kind: 'user_text', role: 'user' }),
    makeItem({ id: 'u:1', threadId: 'thread-a', turnIndex: 1, kind: 'user_text', role: 'user' }),
  ]);
  return pane;
}

describe('applyUserMessageReverted', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetThreadStatuses();
    resetComposerDraftRegistryForTest();
    resetResendRevertMarkersForTest();
    resetThreadInterruptStateForTest();
    __resetThreadHistoryStampsForTest();
  });

  it('drops every cached copy of the window and adopts the post-cut stamp', async () => {
    await seedPane();
    // A window cached under a PRE-cut stamp plus a POST-cut stamp is the
    // one shape that could answer `fresh` over rows the backend removed.
    recordAttestedStamp('thread-a', 1, 5);
    threadItemCache.set('thread-a', {
      items: [makeItem({ id: 'u:1', threadId: 'thread-a', turnIndex: 1 })],
      oldestLoadedTurnIndex: 0,
      newestLoadedTurnIndex: 1,
      hasMoreHistory: false,
      hasMoreNewer: false,
      latestSettledTurn: null,
      historyStamp: { epoch: 1, rev: 5, attested: true },
    });

    applyUserMessageReverted({
      threadId: 'thread-a',
      userItemId: 'u:1',
      turnIndex: 1,
      historyEpoch: 2,
      historyRev: 9,
    });

    expect(threadItemCache.get('thread-a')).toBeNull();
    // Adopted in memory only — never attested, so it can never be
    // persisted into the replica.
    expect(getThreadHistoryStamp('thread-a')).toEqual({ epoch: 2, rev: 9, attested: false });
  });

  it('records a consumable marker only for a pending-resend revert', async () => {
    await seedPane();
    // Ordinary un-send: no marker — a later saga failure on this thread
    // must not be misread as committed.
    applyUserMessageReverted({ threadId: 'thread-a', userItemId: 'u:1', turnIndex: 1 });
    expect(consumeResendRevertMarker('thread-a', 'u:1')).toBe(false);

    applyUserMessageReverted({
      threadId: 'thread-a',
      userItemId: 'u:1',
      turnIndex: 1,
      draftPendingResend: true,
    });
    // Keyed by thread AND item: consuming a DIFFERENT anchor's marker
    // answers false and leaves this one intact. (Under the old
    // thread-wide slot it would have stolen it, and the flow that was
    // actually reverting would then classify its own committed revert as
    // "nothing happened".)
    expect(consumeResendRevertMarker('thread-a', 'u:0')).toBe(false);
    expect(consumeResendRevertMarker('thread-a', 'u:1')).toBe(true);
    // Consumed: a second read answers false.
    expect(consumeResendRevertMarker('thread-a', 'u:1')).toBe(false);
  });

  it('retires a thread\'s older markers when any newer revert lands', async () => {
    await seedPane();
    applyUserMessageReverted({
      threadId: 'thread-a',
      userItemId: 'u:1',
      turnIndex: 1,
      draftPendingResend: true,
    });
    // A newer revert on the same thread — here an ordinary un-send, which
    // records nothing itself. The older saga's marker describes a
    // conversation shape that no longer exists, and nothing local will
    // ever consume a marker set by ANOTHER connected client's revert, so
    // the sweep is what keeps it from answering a later, unrelated
    // failure on the same anchor.
    applyUserMessageReverted({ threadId: 'thread-a', userItemId: 'u:0', turnIndex: 0 });
    expect(consumeResendRevertMarker('thread-a', 'u:1')).toBe(false);
  });

  it('keeps markers on different threads independent', async () => {
    await seedPane();
    for (const threadId of ['thread-a', 'thread-b']) {
      applyUserMessageReverted({
        threadId,
        userItemId: 'u:1',
        turnIndex: 1,
        draftPendingResend: true,
      });
    }
    // thread-b's revert must not sweep thread-a's marker: the sweep is
    // per-thread, and two panes on two threads are the ordinary case.
    expect(consumeResendRevertMarker('thread-a', 'u:1')).toBe(true);
    expect(consumeResendRevertMarker('thread-b', 'u:1')).toBe(true);
  });

  it('rehydrates the composer draft for an ordinary revert', async () => {
    const pane = await seedPane();
    const { draft, reloadFromBackend } = stubDraft();
    registerComposerDraft(pane.paneId, draft);

    applyUserMessageReverted({ threadId: 'thread-a', userItemId: 'u:1', turnIndex: 1 });

    expect(pane.items.map((it) => it.id)).toEqual(['u:0']);
    expect(reloadFromBackend).toHaveBeenCalledWith('thread-a');
  });

  it('ignores duplicate and stale cuts after Send reopens', async () => {
    const pane = await seedPane();
    const token = beginThreadInterrupt('thread-a')!;
    const cut = {
      threadId: 'thread-a',
      userItemId: 'u:1',
      turnIndex: 1,
      historyEpoch: 3,
      historyRev: 12,
    };

    // The RPC response applies the committed cut and releases Send.
    applyUserMessageReverted(cut);
    finishThreadInterrupt('thread-a', token);
    expect(isThreadInterruptPending('thread-a')).toBe(false);

    // A new optimistic send legitimately reuses the reverted identity.
    pane.trackOptimisticItem('u:1');
    pane.upsertItem(makeItem({
      id: 'u:1',
      threadId: 'thread-a',
      turnIndex: 1,
      kind: 'user_text',
      role: 'user',
      summary: 'resent prompt',
    }));
    projectSendStarted('thread-a');

    // The coalesced event for the same cut can arrive after that click. Its
    // history stamp identifies it as the already-applied mutation.
    applyUserMessageReverted(cut);

    expect(pane.items.find((item) => item.id === 'u:1')?.summary).toBe('resent prompt');
    expect(isThreadWorking('thread-a')).toBe(true);

    // A response from an older operation can also arrive after a newer cut.
    // Its lower rev belongs to history this client has already passed.
    applyUserMessageReverted({
      ...cut,
      historyEpoch: 2,
      historyRev: 11,
    });

    expect(pane.items.find((item) => item.id === 'u:1')?.summary).toBe('resent prompt');
    expect(isThreadWorking('thread-a')).toBe(true);
  });

  it('leaves the composer alone while a resend is pending', async () => {
    const pane = await seedPane();
    const { draft, reloadFromBackend } = stubDraft();
    registerComposerDraft(pane.paneId, draft);

    applyUserMessageReverted({
      threadId: 'thread-a',
      userItemId: 'u:1',
      turnIndex: 1,
      draftPendingResend: true,
    });

    // The cut still happens — only the draft row is off-limits, because
    // it is the saga's transient crash copy and not composer content.
    expect(pane.items.map((it) => it.id)).toEqual(['u:0']);
    expect(reloadFromBackend).not.toHaveBeenCalled();
  });

  it('cuts every pane on the thread while a resend is pending, and no other', async () => {
    // The draftPendingResend skip is per-EVENT, not per-pane: a second
    // pane on the same thread must still be truncated (its timeline
    // backs the same SQLite rows), and a pane on another thread must be
    // left entirely alone.
    const paneA = await buildPane(makeThread({ id: 'thread-a' }), [], 'pane-a');
    const paneB = await buildPane(makeThread({ id: 'thread-a' }), [], 'pane-b');
    const paneOther = await buildPane(makeThread({ id: 'thread-b' }), [], 'pane-other');
    for (const pane of [paneA, paneB]) {
      pane.upsertItems([
        makeItem({ id: 'u:0', threadId: 'thread-a', turnIndex: 0, kind: 'user_text', role: 'user' }),
        makeItem({ id: 'u:1', threadId: 'thread-a', turnIndex: 1, kind: 'user_text', role: 'user' }),
      ]);
    }
    paneOther.upsertItems([
      makeItem({ id: 'o:1', threadId: 'thread-b', turnIndex: 1, kind: 'user_text', role: 'user' }),
    ]);
    const drafts = [paneA, paneB, paneOther].map((pane) => {
      const stub = stubDraft();
      registerComposerDraft(pane.paneId, stub.draft);
      return stub;
    });

    applyUserMessageReverted({
      threadId: 'thread-a',
      userItemId: 'u:1',
      turnIndex: 1,
      draftPendingResend: true,
    });

    expect(paneA.items.map((it) => it.id)).toEqual(['u:0']);
    expect(paneB.items.map((it) => it.id)).toEqual(['u:0']);
    expect(paneOther.items.map((it) => it.id)).toEqual(['o:1']);
    for (const { reloadFromBackend } of drafts) {
      expect(reloadFromBackend).not.toHaveBeenCalled();
    }
  });

  it('keeps the anchor turn prefix the backend kept, on every pane', async () => {
    // Claude's item-granular cut keeps rows that share the anchor's turn
    // at a lower item index. The event names them; re-deriving the rule
    // in UI code is what this pins against.
    const paneA = await buildPane(makeThread({ id: 'thread-a' }), [], 'pane-a');
    const paneB = await buildPane(makeThread({ id: 'thread-a' }), [], 'pane-b');
    for (const pane of [paneA, paneB]) {
      pane.upsertItems([
        makeItem({ id: 'u:0', threadId: 'thread-a', turnIndex: 0, itemIndex: 0, kind: 'user_text', role: 'user' }),
        makeItem({ id: 'k:0', threadId: 'thread-a', turnIndex: 1, itemIndex: 0, kind: 'user_text', role: 'user' }),
        makeItem({ id: 'k:1', threadId: 'thread-a', turnIndex: 1, itemIndex: 1, kind: 'assistant_text', role: 'assistant' }),
        makeItem({ id: 'u:1', threadId: 'thread-a', turnIndex: 1, itemIndex: 2, kind: 'user_text', role: 'user' }),
      ]);
    }

    applyUserMessageReverted({
      threadId: 'thread-a',
      userItemId: 'u:1',
      turnIndex: 1,
      keptAnchorTurnItemIds: ['k:0', 'k:1'],
      draftPendingResend: true,
    });

    expect(paneA.items.map((it) => it.id)).toEqual(['u:0', 'k:0', 'k:1']);
    expect(paneB.items.map((it) => it.id)).toEqual(['u:0', 'k:0', 'k:1']);
  });
});
