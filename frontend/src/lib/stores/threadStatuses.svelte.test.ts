import { describe, expect, it, beforeEach } from 'vitest';
import {
  getThreadStatus,
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
} from './threadStatuses.svelte';
import { makeItem } from '../../test/helpers/chat';

describe('threadStatuses store', () => {
  beforeEach(() => {
    resetForTest();
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
});
