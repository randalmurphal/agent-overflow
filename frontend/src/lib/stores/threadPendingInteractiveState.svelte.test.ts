import { describe, expect, it } from 'vitest';
import type {
  ApprovalRequest,
  UserInputRequest,
} from '../types/events';
import { createThreadPendingInteractiveState } from './threadPendingInteractiveState.svelte';

function makeApproval(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    requestId: 'approval-1',
    threadId: 'thread-1',
    toolName: 'Bash',
    description: 'Run command',
    input: { command: 'pwd' },
    title: 'Approve command',
    ...overrides,
  };
}

function makeUserInput(overrides: Partial<UserInputRequest> = {}): UserInputRequest {
  return {
    requestId: 'input-1',
    threadId: 'thread-1',
    toolName: 'user_input',
    title: 'User Input Required',
    questions: [{
      id: 'scope',
      header: 'Scope',
      question: 'Choose a scope',
      options: [{ label: 'turn', description: 'Apply only to this turn' }],
    }],
    ...overrides,
  };
}

describe('createThreadPendingInteractiveState', () => {
  it('removes old requests absent from the snapshot and prefers arrivals during the read', () => {
    const state = createThreadPendingInteractiveState();
    state.addApproval(makeApproval({ requestId: 'stale' }));
    state.addApproval(makeApproval({ requestId: 'overlap', title: 'Before read' }));
    const apply = state.beginSnapshot();
    state.addApproval(makeApproval({ requestId: 'overlap', title: 'During read' }));
    state.addUserInput(makeUserInput());
    const projected = apply({
      approvals: [makeApproval({ requestId: 'overlap', title: 'Snapshot' }), makeApproval({ requestId: 'snapshot-only' })],
      userInputs: [],
    });
    expect(projected).toEqual({ approvals: state.approvals, userInputs: state.userInputs });
    expect(state.approvals.map(request => request.requestId)).toEqual(['overlap', 'snapshot-only']);
    expect(state.approvals[0].title).toBe('During read');
    expect(state.userInputs.map(request => request.requestId)).toEqual(['input-1']);
  });

  it('does not revive requests resolved while a snapshot is in flight', () => {
    const state = createThreadPendingInteractiveState();
    const apply = state.beginSnapshot();
    state.removeApproval('approval-1');
    state.removeUserInput('input-1');
    expect(apply({ approvals: [makeApproval()], userInputs: [makeUserInput()] }))
      .toEqual({ approvals: [], userInputs: [] });
    expect(state.approvals).toEqual([]);
    expect(state.userInputs).toEqual([]);
  });

  it('lets a live add supersede a previously resolved request id', () => {
    const state = createThreadPendingInteractiveState();

    state.removeUserInput('input-1');
    state.addUserInput(makeUserInput({ requestId: 'input-1' }));

    expect(state.userInputs.map((request) => request.requestId)).toEqual(['input-1']);
  });

  it('keeps requests visible if the snapshot read fails', () => {
    const state = createThreadPendingInteractiveState();
    state.addApproval(makeApproval());
    state.beginSnapshot();
    expect(state.approvals.map(request => request.requestId)).toEqual(['approval-1']);
  });

  it('clear resets pending arrays and resolved request ids for a new thread', () => {
    const state = createThreadPendingInteractiveState();
    state.removeApproval('approval-1');

    state.clear();
    state.beginSnapshot()({
      approvals: [makeApproval({ requestId: 'approval-1' })],
      userInputs: [],
    });

    expect(state.approvals.map((approval) => approval.requestId)).toEqual(['approval-1']);
  });
});
