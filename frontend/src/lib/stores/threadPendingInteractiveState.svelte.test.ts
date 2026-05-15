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
  it('applies backend snapshots before existing local requests and de-dupes by request id', () => {
    const state = createThreadPendingInteractiveState();
    state.addApproval(makeApproval({ requestId: 'local-only', title: 'Local only' }));
    state.addApproval(makeApproval({ requestId: 'overlap', title: 'Local overlap' }));

    const filtered = state.registrySnapshotFor({
      approvals: [
        makeApproval({ requestId: 'overlap', title: 'Snapshot overlap' }),
        makeApproval({ requestId: 'snapshot-only', title: 'Snapshot only' }),
      ],
      userInputs: [],
    });
    state.applySnapshot({
      approvals: [
        makeApproval({ requestId: 'overlap', title: 'Snapshot overlap' }),
        makeApproval({ requestId: 'snapshot-only', title: 'Snapshot only' }),
      ],
      userInputs: [],
    });

    expect(filtered.approvals.map((approval) => approval.requestId)).toEqual([
      'overlap',
      'snapshot-only',
    ]);
    expect(state.approvals.map((approval) => approval.requestId)).toEqual([
      'overlap',
      'snapshot-only',
      'local-only',
    ]);
    expect(state.approvals[0]?.title).toBe('Snapshot overlap');
  });

  it('does not revive requests resolved while a snapshot is in flight', () => {
    const state = createThreadPendingInteractiveState();

    state.removeApproval('approval-1');
    state.removeUserInput('input-1');
    const filtered = state.registrySnapshotFor({
      approvals: [makeApproval({ requestId: 'approval-1' })],
      userInputs: [makeUserInput({ requestId: 'input-1' })],
    });
    state.applySnapshot({
      approvals: [makeApproval({ requestId: 'approval-1' })],
      userInputs: [makeUserInput({ requestId: 'input-1' })],
    });

    expect(filtered).toEqual({ approvals: [], userInputs: [] });
    expect(state.approvals).toEqual([]);
    expect(state.userInputs).toEqual([]);
  });

  it('lets a live add supersede a previously resolved request id', () => {
    const state = createThreadPendingInteractiveState();

    state.removeUserInput('input-1');
    state.addUserInput(makeUserInput({ requestId: 'input-1' }));

    expect(state.userInputs.map((request) => request.requestId)).toEqual(['input-1']);
  });

  it('can prepare for live-state hydration without forgetting resolved request ids', () => {
    const state = createThreadPendingInteractiveState();
    state.addApproval(makeApproval({ requestId: 'local-only' }));
    state.removeApproval('resolved-late');

    state.prepareForLiveStateHydration();
    const filtered = state.registrySnapshotFor({
      approvals: [
        makeApproval({ requestId: 'local-only' }),
        makeApproval({ requestId: 'resolved-late' }),
      ],
      userInputs: [],
    });
    state.applySnapshot({
      approvals: [
        makeApproval({ requestId: 'local-only' }),
        makeApproval({ requestId: 'resolved-late' }),
      ],
      userInputs: [],
    });

    expect(filtered.approvals.map((approval) => approval.requestId)).toEqual(['local-only']);
    expect(state.approvals.map((approval) => approval.requestId)).toEqual(['local-only']);
  });

  it('clear resets pending arrays and resolved request ids for a new thread', () => {
    const state = createThreadPendingInteractiveState();
    state.removeApproval('approval-1');

    state.clear();
    state.applySnapshot({
      approvals: [makeApproval({ requestId: 'approval-1' })],
      userInputs: [],
    });

    expect(state.approvals.map((approval) => approval.requestId)).toEqual(['approval-1']);
  });
});
