import { describe, expect, it, beforeEach } from 'vitest';
import {
  getThreadStatus,
  projectApprovalRequest,
  projectApprovalResolution,
  projectSendResolved,
  projectSendStarted,
  projectThreadItem,
  projectTurnCompleted,
  projectTurnStarted,
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
      projectTurnStarted('thread-1', 'turn-1');
      // Even if the send flag is cleared later, the turn keeps running.
      projectSendResolved('thread-1');
      expect(getThreadStatus('thread-1')).toBe('running');
    });
  });

  describe('turn lifecycle', () => {
    it('projectTurnStarted flips to running and turn_completed flips to idle', () => {
      expect(getThreadStatus('thread-1')).toBe('idle');
      projectTurnStarted('thread-1', 'turn-1');
      expect(getThreadStatus('thread-1')).toBe('running');

      projectTurnCompleted('thread-1', 'turn-1');
      expect(getThreadStatus('thread-1')).toBe('idle');
    });

    it('aborted turn flips to error', () => {
      projectTurnStarted('thread-1', 'turn-1');
      projectTurnCompleted('thread-1', 'turn-1', { aborted: true });
      expect(getThreadStatus('thread-1')).toBe('error');
    });

    it('turn with errorMessage flips to error', () => {
      projectTurnStarted('thread-1', 'turn-1');
      projectTurnCompleted('thread-1', 'turn-1', { errorMessage: 'boom' });
      expect(getThreadStatus('thread-1')).toBe('error');
    });

    it('starting a fresh turn clears a prior error', () => {
      projectTurnStarted('thread-1', 'turn-1');
      projectTurnCompleted('thread-1', 'turn-1', { aborted: true });
      expect(getThreadStatus('thread-1')).toBe('error');

      projectTurnStarted('thread-1', 'turn-2');
      expect(getThreadStatus('thread-1')).toBe('running');
    });

    it('multiple concurrent turns keep running until both complete', () => {
      projectTurnStarted('thread-1', 'turn-a');
      projectTurnStarted('thread-1', 'turn-b');
      expect(getThreadStatus('thread-1')).toBe('running');

      projectTurnCompleted('thread-1', 'turn-a');
      expect(getThreadStatus('thread-1')).toBe('running');

      projectTurnCompleted('thread-1', 'turn-b');
      expect(getThreadStatus('thread-1')).toBe('idle');
    });

    it('pending approval dominates an in-flight turn', () => {
      projectTurnStarted('thread-1', 'turn-1');
      projectApprovalRequest('thread-1', 'req-1');
      expect(getThreadStatus('thread-1')).toBe('pending-approval');

      projectApprovalResolution('thread-1', 'req-1');
      expect(getThreadStatus('thread-1')).toBe('running');
    });
  });
});
