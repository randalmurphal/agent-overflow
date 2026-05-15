import { describe, expect, it, beforeEach } from 'vitest';
import {
  clearPendingSend,
  clearThreadStatus,
  getActiveTurn,
  getThreadStatus,
  hasPendingSend,
  projectApprovalRequest,
  projectApprovalResolution,
  projectPlanReady,
  projectPlanResolved,
  projectSendResolved,
  projectSendStarted,
  projectThreadViewed,
  projectThreadItem,
  projectTurnCompleted,
  projectTurnStarted,
  projectUserInputRequest,
  projectUserInputResolution,
  resetForTest,
  sameActiveTurn,
} from './threadStatuses.svelte';
import {
  getQueueForThread,
  replaceQueueForThread,
  resetForTest as resetSendQueueForTest,
  type QueueItem,
} from './sendQueue.svelte';

// Test-only convenience helper: build a QueueItem from a partial
// shape and replace the thread's Zone 1 mirror with it. Test code
// used to call `enqueue(threadId, draft)` to drive the old
// frontend-owned queue; in the new architecture Zone 1 mirrors
// backend state, so tests seed it via replaceQueueForThread directly.
function seedQueueItem(threadId: string, partial: Partial<QueueItem> & { message: string }): void {
  const item: QueueItem = {
    id: partial.id ?? `queue:${Math.random().toString(36).slice(2)}`,
    threadId,
    message: partial.message,
    attachmentIds: partial.attachmentIds ?? [],
    sourceProposedPlan: partial.sourceProposedPlan ?? null,
    revisionSourceProposedPlan: partial.revisionSourceProposedPlan ?? null,
    revisionSourceCommentIds: partial.revisionSourceCommentIds,
    enqueuedAt: partial.enqueuedAt ?? Date.now(),
  };
  const current = getQueueForThread(threadId);
  replaceQueueForThread(threadId, [...current, item]);
}
import { makeItem } from '../../test/helpers/chat';

describe('threadStatuses store', () => {
  beforeEach(() => {
    resetForTest();
    resetSendQueueForTest();
  });

  it('compares active turns by identity fields', () => {
    expect(sameActiveTurn(null, null)).toBe(true);
    expect(sameActiveTurn(null, { turnId: 't', turnIndex: 1, startedAt: 1 })).toBe(false);
    expect(sameActiveTurn(
      { turnId: 't', turnIndex: 1, startedAt: 1 },
      { turnId: 't', turnIndex: 1, startedAt: 1 },
    )).toBe(true);
    expect(sameActiveTurn(
      { turnId: 't', turnIndex: 1, startedAt: 1 },
      { turnId: 't', turnIndex: 2, startedAt: 1 },
    )).toBe(false);
  });

  it('tracks multiple active items before returning to idle', () => {
    projectThreadItem(makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
    }));
    projectThreadItem(makeItem({
      id: 'tool-1',
      kind: 'tool_call',
      status: 'running',
      isBackground: false,
    }));

    expect(getThreadStatus('thread-1')).toBe('running');

    projectThreadItem(makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'completed',
    }));
    expect(getThreadStatus('thread-1')).toBe('running');

    projectThreadItem(makeItem({
      id: 'tool-1',
      kind: 'tool_call',
      status: 'completed',
      isBackground: false,
    }));
    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('lets pending approvals dominate and resolves by requestId fallback', () => {
    projectThreadItem(makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
    }));
    expect(getThreadStatus('thread-1')).toBe('running');

    projectApprovalRequest('thread-1', 'req-1');
    expect(getThreadStatus('thread-1')).toBe('pending-approval');

    projectApprovalResolution(undefined, 'req-1');
    expect(getThreadStatus('thread-1')).toBe('running');

    projectThreadItem(makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'completed',
    }));
    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('holds error until a new turn signal supersedes it', () => {
    projectThreadItem(makeItem({
      id: 'error-1',
      kind: 'error',
      role: 'system',
      status: 'completed',
    }));
    expect(getThreadStatus('thread-1')).toBe('error');

    projectThreadItem(makeItem({
      id: 'user-1',
      kind: 'user_text',
      role: 'user',
      status: 'completed',
    }));
    expect(getThreadStatus('thread-1')).toBe('idle');

    projectThreadItem(makeItem({
      id: 'tool-1',
      kind: 'tool_call',
      status: 'running',
      isBackground: false,
    }));
    expect(getThreadStatus('thread-1')).toBe('running');
  });

  it('clears an error when the thread is viewed', () => {
    projectThreadItem(makeItem({
      id: 'error-1',
      kind: 'error',
      role: 'system',
      status: 'completed',
    }));
    expect(getThreadStatus('thread-1')).toBe('error');

    projectThreadViewed('thread-1');

    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('clears an interrupted turn when the thread is viewed', () => {
    projectTurnStarted('thread-1', 'turn-1', 0, 0);
    projectTurnCompleted('thread-1', 'turn-1', { aborted: true });
    expect(getThreadStatus('thread-1')).toBe('interrupted');

    projectThreadViewed('thread-1');

    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('viewing a thread does not clear blocking action states', () => {
    projectTurnStarted('thread-1', 'turn-1', 0, 0);
    projectApprovalRequest('thread-1', 'req-1', 'command');
    projectThreadViewed('thread-1');
    expect(getThreadStatus('thread-1')).toBe('pending-approval');

    projectApprovalResolution('thread-1', 'req-1');
    projectUserInputRequest('thread-1', 'req-2');
    projectThreadViewed('thread-1');
    expect(getThreadStatus('thread-1')).toBe('awaiting-input');

    projectUserInputResolution('thread-1', 'req-2');
    projectTurnCompleted('thread-1', 'turn-1');
    projectPlanReady('thread-1');
    projectThreadViewed('thread-1');
    expect(getThreadStatus('thread-1')).toBe('plan-ready');
  });

  it('clears interrupted even when a higher-priority visible status masks it', () => {
    projectTurnStarted('thread-1', 'turn-1', 0, 0);
    projectTurnCompleted('thread-1', 'turn-1', { aborted: true });
    projectPlanReady('thread-1');
    expect(getThreadStatus('thread-1')).toBe('plan-ready');

    projectThreadViewed('thread-1');
    expect(getThreadStatus('thread-1')).toBe('plan-ready');

    projectPlanResolved('thread-1');
    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  describe('optimistic send', () => {
    it('flips to running on projectSendStarted before any turn events', () => {
      projectSendStarted('thread-1');
      expect(getThreadStatus('thread-1')).toBe('running');
    });

    it('flips to error on projectSendResolved({error: true})', () => {
      projectSendStarted('thread-1');
      projectSendResolved('thread-1', { error: true });
      expect(getThreadStatus('thread-1')).toBe('error');
    });

    it('a new send clears a prior error state', () => {
      projectSendStarted('thread-1');
      projectSendResolved('thread-1', { error: true });
      expect(getThreadStatus('thread-1')).toBe('error');

      projectSendStarted('thread-1');
      expect(getThreadStatus('thread-1')).toBe('running');
    });

    it('turn_started supersedes the optimistic send flag', () => {
      projectSendStarted('thread-1');
      projectTurnStarted('thread-1', 'turn-1', 0, 0);
      // Even if the send flag is cleared later, the turn keeps running.
      projectSendResolved('thread-1');
      expect(getThreadStatus('thread-1')).toBe('running');
    });
  });

  describe('turn lifecycle', () => {
    it('projectTurnStarted flips to running and turn_completed flips to idle', () => {
      expect(getThreadStatus('thread-1')).toBe('idle');
      projectTurnStarted('thread-1', 'turn-1', 0, 0);
      expect(getThreadStatus('thread-1')).toBe('running');

      projectTurnCompleted('thread-1', 'turn-1');
      expect(getThreadStatus('thread-1')).toBe('idle');
    });

    it('ignores a late turn_started for a round that already completed', () => {
      projectTurnCompleted('thread-1', 'round-1');

      projectTurnStarted('thread-1', 'round-1', 0, 100);

      expect(getActiveTurn('thread-1')).toBeNull();
      expect(getThreadStatus('thread-1')).toBe('idle');
    });

    it('preserves a completed-before-started error state', () => {
      projectTurnCompleted('thread-1', 'round-1', { errorMessage: 'boom' });

      projectTurnStarted('thread-1', 'round-1', 0, 100);

      expect(getActiveTurn('thread-1')).toBeNull();
      expect(getThreadStatus('thread-1')).toBe('error');
    });

    it('aborted turn flips to interrupted', () => {
      projectTurnStarted('thread-1', 'turn-1', 0, 0);
      projectTurnCompleted('thread-1', 'turn-1', { aborted: true });
      expect(getThreadStatus('thread-1')).toBe('interrupted');
    });

    it('turn with errorMessage flips to error', () => {
      projectTurnStarted('thread-1', 'turn-1', 0, 0);
      projectTurnCompleted('thread-1', 'turn-1', { errorMessage: 'boom' });
      expect(getThreadStatus('thread-1')).toBe('error');
    });

    it('starting a fresh turn clears a prior interrupted state', () => {
      projectTurnStarted('thread-1', 'turn-1', 0, 0);
      projectTurnCompleted('thread-1', 'turn-1', { aborted: true });
      expect(getThreadStatus('thread-1')).toBe('interrupted');

      projectTurnStarted('thread-1', 'turn-2', 0, 0);
      expect(getThreadStatus('thread-1')).toBe('running');
    });

    it('a provider error supersedes interrupted', () => {
      projectTurnStarted('thread-1', 'turn-1', 0, 0);
      projectTurnCompleted('thread-1', 'turn-1', { aborted: true });
      expect(getThreadStatus('thread-1')).toBe('interrupted');

      projectTurnStarted('thread-1', 'turn-2', 0, 0);
      projectTurnCompleted('thread-1', 'turn-2', { errorMessage: 'boom' });
      expect(getThreadStatus('thread-1')).toBe('error');
    });

    it('multiple concurrent turns keep running until both complete', () => {
      projectTurnStarted('thread-1', 'turn-a', 0, 0);
      projectTurnStarted('thread-1', 'turn-b', 0, 0);
      expect(getThreadStatus('thread-1')).toBe('running');

      projectTurnCompleted('thread-1', 'turn-a');
      expect(getThreadStatus('thread-1')).toBe('running');

      projectTurnCompleted('thread-1', 'turn-b');
      expect(getThreadStatus('thread-1')).toBe('idle');
    });

    it('pending approval dominates an in-flight turn', () => {
      projectTurnStarted('thread-1', 'turn-1', 0, 0);
      projectApprovalRequest('thread-1', 'req-1');
      expect(getThreadStatus('thread-1')).toBe('pending-approval');

      projectApprovalResolution('thread-1', 'req-1');
      expect(getThreadStatus('thread-1')).toBe('running');
    });

    it('flips off between wire rounds and re-flips on round 2', () => {
      // Pins the per-wire-round emission cadence (see
      // internal/triage/AGENTS.md "Wire-round vs logical-turn"). The
      // backend emits one provider:turn_started/turn_completed pair per
      // wire `result` envelope; in Claude's multi-result-per-turn
      // cascade two pairs flow per logical turn. Each round generates
      // a distinct turnId — round 1 ends, getActiveTurn returns null
      // (composer enabled, indicator off), round 2 starts with a new
      // id and re-flips the indicator on.
      projectTurnStarted('thread-1', 'round-1', 0, 1_000);
      expect(getThreadStatus('thread-1')).toBe('running');
      expect(getActiveTurn('thread-1')?.turnId).toBe('round-1');

      // Round 1 completes — the model handed off to backgrounded work.
      // Frontend sees no active turn during the gap.
      projectTurnCompleted('thread-1', 'round-1');
      expect(getThreadStatus('thread-1')).toBe('idle');
      expect(getActiveTurn('thread-1')).toBeNull();

      // Round 2 begins (Claude system.init re-emit after a
      // task_notification provoked another model call). Fresh
      // turnId — frontend treats it as a distinct active turn,
      // resets startedAt anchor for the elapsed-time timer.
      projectTurnStarted('thread-1', 'round-2', 0, 5_000);
      expect(getThreadStatus('thread-1')).toBe('running');
      const round2 = getActiveTurn('thread-1');
      expect(round2?.turnId).toBe('round-2');
      expect(round2?.startedAt).toBe(5_000);

      projectTurnCompleted('thread-1', 'round-2');
      expect(getActiveTurn('thread-1')).toBeNull();
    });

    it('round 2 turn_started replaces the round 1 entry without leaking', () => {
      // Defensive case: if the round 1 turn_completed somehow gets
      // dropped (transport hiccup, observer race), a round 2
      // turn_started must still take over the slot rather than
      // silently no-op. The idempotency guard keys on turnId — a
      // different id replaces the prior entry.
      projectTurnStarted('thread-1', 'round-1', 0, 1_000);
      expect(getActiveTurn('thread-1')?.turnId).toBe('round-1');

      projectTurnStarted('thread-1', 'round-2', 0, 5_000);
      expect(getActiveTurn('thread-1')?.turnId).toBe('round-2');
      expect(getActiveTurn('thread-1')?.startedAt).toBe(5_000);
    });

    it('duplicate turn_started for the same round preserves startedAt', () => {
      // Claude `system.init` resend after interrupt or Codex
      // turn/started replay can re-emit for the SAME round. The
      // existing-turnId guard preserves the original startedAt so
      // the elapsed-seconds counter stays monotonic.
      projectTurnStarted('thread-1', 'round-1', 0, 1_000);
      projectTurnStarted('thread-1', 'round-1', 0, 9_999);
      expect(getActiveTurn('thread-1')?.startedAt).toBe(1_000);
    });

    it('turn_completed for a non-matching turnId leaves activeTurn untouched', () => {
      // A late complete for a stale round id (e.g. the synthetic
      // truncate-then-real-result race surviving a thread teardown
      // and rebirth) must not clobber the current round's entry.
      projectTurnStarted('thread-1', 'round-2', 0, 5_000);
      projectTurnCompleted('thread-1', 'round-1');
      expect(getActiveTurn('thread-1')?.turnId).toBe('round-2');
    });
  });

  describe('awaiting-input user-input requests', () => {
    it('flips to awaiting-input for structured user input', () => {
      projectUserInputRequest('thread-1', 'req-1');
      expect(getThreadStatus('thread-1')).toBe('awaiting-input');

      projectUserInputResolution('thread-1', 'req-1');
      expect(getThreadStatus('thread-1')).toBe('idle');
    });

    it('mcp-elicitation is a blocking approval', () => {
      projectApprovalRequest('thread-1', 'req-1', 'mcp-elicitation');
      expect(getThreadStatus('thread-1')).toBe('pending-approval');
    });

    it.each(['command', 'file-read', 'file-change', 'permission'] as const)(
      '%s kind still resolves to pending-approval',
      (kind) => {
        projectApprovalRequest('thread-1', 'req-1', kind);
        expect(getThreadStatus('thread-1')).toBe('pending-approval');
      },
    );

    it('missing kind defaults to pending-approval (safer fallback)', () => {
      projectApprovalRequest('thread-1', 'req-1');
      expect(getThreadStatus('thread-1')).toBe('pending-approval');
    });

    it('pending-approval dominates awaiting-input when both are outstanding', () => {
      projectUserInputRequest('thread-1', 'req-input');
      expect(getThreadStatus('thread-1')).toBe('awaiting-input');

      projectApprovalRequest('thread-1', 'req-cmd', 'command');
      expect(getThreadStatus('thread-1')).toBe('pending-approval');

      // Clearing the command approval drops back to awaiting-input.
      projectApprovalResolution('thread-1', 'req-cmd');
      expect(getThreadStatus('thread-1')).toBe('awaiting-input');
    });

    it('awaiting-input dominates a running turn', () => {
      projectTurnStarted('thread-1', 'turn-1', 0, 0);
      projectUserInputRequest('thread-1', 'req-1');
      expect(getThreadStatus('thread-1')).toBe('awaiting-input');
    });
  });

  describe('plan-ready', () => {
    it('flips to plan-ready on a completed proposed_plan item', () => {
      projectThreadItem(makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        status: 'completed',
      }));
      expect(getThreadStatus('thread-1')).toBe('plan-ready');
    });

    it('does NOT flip back to plan-ready on implemented proposed_plan upsert', () => {
      projectPlanReady('thread-1');
      projectTurnStarted('thread-1', 'turn-1', 0, 0);
      projectThreadItem(makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        status: 'completed',
        meta: '{"planImplementedAt":123}',
      }));
      projectTurnCompleted('thread-1', 'turn-1');

      expect(getThreadStatus('thread-1')).toBe('idle');
    });

    it('does NOT flip on an errored proposed_plan', () => {
      projectThreadItem(makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        status: 'errored',
      }));
      expect(getThreadStatus('thread-1')).toBe('idle');
    });

    it('does NOT flip on a user-authored proposed_plan payload', () => {
      projectThreadItem(makeItem({
        id: 'plan-1',
        kind: 'user_text',
        role: 'user',
        payloadKind: 'proposed_plan',
        status: 'completed',
      }));
      expect(getThreadStatus('thread-1')).toBe('idle');
    });

    it('does NOT flip on a streaming (in-progress) proposed_plan', () => {
      projectThreadItem(makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        status: 'streaming',
      }));
      // Streaming items land as active-item → running, not plan-ready.
      // The plan-ready flag only kicks in on terminal completed status.
      expect(getThreadStatus('thread-1')).not.toBe('plan-ready');
    });

    it('turn_started clears plan-ready (user accepted or rejected)', () => {
      projectPlanReady('thread-1');
      expect(getThreadStatus('thread-1')).toBe('plan-ready');

      projectTurnStarted('thread-1', 'turn-1', 0, 0);
      expect(getThreadStatus('thread-1')).toBe('running');
    });

    it('explicit projectPlanResolved clears the flag', () => {
      projectPlanReady('thread-1');
      expect(getThreadStatus('thread-1')).toBe('plan-ready');

      projectPlanResolved('thread-1');
      expect(getThreadStatus('thread-1')).toBe('idle');
    });

    it('running trumps plan-ready while a tool_call is active', () => {
      projectPlanReady('thread-1');
      projectThreadItem(makeItem({
        id: 'tool-1',
        kind: 'tool_call',
        status: 'running',
        isBackground: false,
      }));
      expect(getThreadStatus('thread-1')).toBe('running');
    });

    it('pending-approval and awaiting-input both trump plan-ready', () => {
      projectPlanReady('thread-1');
      projectUserInputRequest('thread-1', 'req-1');
      expect(getThreadStatus('thread-1')).toBe('awaiting-input');

      projectApprovalRequest('thread-1', 'req-2', 'command');
      expect(getThreadStatus('thread-1')).toBe('pending-approval');
    });

    it('error trumps plan-ready until the thread is viewed', () => {
      projectPlanReady('thread-1');
      expect(getThreadStatus('thread-1')).toBe('plan-ready');

      projectThreadItem(makeItem({
        id: 'error-1',
        kind: 'error',
        role: 'system',
        status: 'completed',
      }));
      expect(getThreadStatus('thread-1')).toBe('error');

      projectThreadViewed('thread-1');
      expect(getThreadStatus('thread-1')).toBe('plan-ready');
    });
  });

  describe('hasPendingSend / clearPendingSend', () => {
    it('hasPendingSend mirrors projectSendStarted / projectSendResolved', () => {
      expect(hasPendingSend('thread-1')).toBe(false);
      projectSendStarted('thread-1');
      expect(hasPendingSend('thread-1')).toBe(true);

      projectSendResolved('thread-1');
      expect(hasPendingSend('thread-1')).toBe(false);
    });

    it('hasPendingSend is false for null/undefined/empty thread ids', () => {
      projectSendStarted('thread-1');
      expect(hasPendingSend(null)).toBe(false);
      expect(hasPendingSend(undefined)).toBe(false);
      expect(hasPendingSend('')).toBe(false);
    });

    it('projectTurnStarted clears the pending-send flag (success drain path)', () => {
      projectSendStarted('thread-1');
      expect(hasPendingSend('thread-1')).toBe(true);

      projectTurnStarted('thread-1', 'turn-1', 0, 100);
      expect(hasPendingSend('thread-1')).toBe(false);
    });

    it('clearPendingSend drops the flag without flipping the thread to error', () => {
      // Mirrors the drain failure path: SendMessageWithOptions threw,
      // we want to stop advertising "running" but the thread should
      // not be flipped to a Failed pill state — the queue preview
      // still shows the user's restored item, and the error banner
      // carries the failure context.
      projectSendStarted('thread-1');
      expect(getThreadStatus('thread-1')).toBe('running');

      clearPendingSend('thread-1');
      expect(hasPendingSend('thread-1')).toBe(false);
      expect(getThreadStatus('thread-1')).toBe('idle');
    });

    it('clearPendingSend on a thread without the flag is a no-op', () => {
      // No throw, no error pill — defensive call from the drain
      // failure path even when projectTurnStarted may have already
      // arrived first.
      expect(() => clearPendingSend('thread-1')).not.toThrow();
      expect(getThreadStatus('thread-1')).toBe('idle');
    });
  });

  describe('clearThreadStatus sweeps the send queue', () => {
    it('drops queued items for the cleared thread only', () => {
      seedQueueItem('thread-1', { message: 'queued for 1' });
      seedQueueItem('thread-2', { message: 'queued for 2' });

      clearThreadStatus('thread-1');
      expect(getQueueForThread('thread-1')).toEqual([]);
      // thread-2's queue is untouched — the sweep is per-thread.
      expect(getQueueForThread('thread-2').map((item) => item.message)).toEqual(['queued for 2']);
    });
  });
});
