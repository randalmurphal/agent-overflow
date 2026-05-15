import type {
  ApprovalRequest,
  PendingInteractiveRequests,
  UserInputRequest,
} from '../types/events';

function mergePendingRequests<T extends { requestId: string }>(
  snapshot: T[],
  current: T[],
  resolvedRequestIds: Set<string>,
): T[] {
  const merged: T[] = [];
  const seen = new Set<string>();
  for (const request of snapshot) {
    if (!request.requestId || resolvedRequestIds.has(request.requestId)) continue;
    merged.push(request);
    seen.add(request.requestId);
  }
  for (const request of current) {
    if (!request.requestId || resolvedRequestIds.has(request.requestId)) continue;
    if (seen.has(request.requestId)) continue;
    merged.push(request);
    seen.add(request.requestId);
  }
  return merged;
}

export interface ThreadPendingInteractiveState {
  readonly approvals: ApprovalRequest[];
  readonly userInputs: UserInputRequest[];
  clear(): void;
  prepareForLiveStateHydration(): void;
  applySnapshot(snapshot: PendingInteractiveRequests | null | undefined): void;
  registrySnapshotFor(snapshot: PendingInteractiveRequests | null | undefined): PendingInteractiveRequests;
  addApproval(approval: ApprovalRequest): void;
  removeApproval(requestId: string): void;
  addUserInput(request: UserInputRequest): void;
  removeUserInput(requestId: string): void;
}

export function createThreadPendingInteractiveState(): ThreadPendingInteractiveState {
  let approvals: ApprovalRequest[] = $state([]);
  let userInputs: UserInputRequest[] = $state([]);
  const resolvedApprovalIds = new Set<string>();
  const resolvedUserInputIds = new Set<string>();

  function clear(): void {
    approvals = [];
    userInputs = [];
    resolvedApprovalIds.clear();
    resolvedUserInputIds.clear();
  }

  function prepareForLiveStateHydration(): void {
    approvals = [];
    userInputs = [];
  }

  function registrySnapshotFor(
    snapshot: PendingInteractiveRequests | null | undefined,
  ): PendingInteractiveRequests {
    return {
      approvals: (snapshot?.approvals ?? [])
        .filter((request) => request.requestId && !resolvedApprovalIds.has(request.requestId)),
      userInputs: (snapshot?.userInputs ?? [])
        .filter((request) => request.requestId && !resolvedUserInputIds.has(request.requestId)),
    };
  }

  function applySnapshot(
    snapshot: PendingInteractiveRequests | null | undefined,
  ): void {
    const filtered = registrySnapshotFor(snapshot);
    approvals = mergePendingRequests(filtered.approvals, approvals, resolvedApprovalIds);
    userInputs = mergePendingRequests(filtered.userInputs, userInputs, resolvedUserInputIds);
  }

  function addApproval(approval: ApprovalRequest): void {
    resolvedApprovalIds.delete(approval.requestId);
    approvals = [
      ...approvals.filter((existing) => existing.requestId !== approval.requestId),
      approval,
    ];
  }

  function removeApproval(requestId: string): void {
    resolvedApprovalIds.add(requestId);
    approvals = approvals.filter((approval) => approval.requestId !== requestId);
  }

  function addUserInput(request: UserInputRequest): void {
    resolvedUserInputIds.delete(request.requestId);
    userInputs = [
      ...userInputs.filter((existing) => existing.requestId !== request.requestId),
      request,
    ];
  }

  function removeUserInput(requestId: string): void {
    resolvedUserInputIds.add(requestId);
    userInputs = userInputs.filter((request) => request.requestId !== requestId);
  }

  return {
    get approvals() { return approvals; },
    get userInputs() { return userInputs; },
    clear,
    prepareForLiveStateHydration,
    applySnapshot,
    registrySnapshotFor,
    addApproval,
    removeApproval,
    addUserInput,
    removeUserInput,
  };
}
