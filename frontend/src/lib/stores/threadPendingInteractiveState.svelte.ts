import type {
  ApprovalRequest,
  PendingInteractiveRequests,
  UserInputRequest,
} from '../types/events';

function mergePendingRequests<T extends { requestId: string }>(
  snapshot: T[],
  current: T[],
  atRequest: Set<T>,
  resolvedRequestIds: Set<string>,
): T[] {
  const merged: T[] = [];
  const seen = new Set<string>();
  // Only arrivals since the read began outrank its authoritative answer.
  for (const request of current) {
    if (atRequest.has(request)) continue;
    if (!request.requestId || resolvedRequestIds.has(request.requestId)) continue;
    merged.push(request);
    seen.add(request.requestId);
  }
  for (const request of snapshot) {
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
  beginSnapshot(): (snapshot: PendingInteractiveRequests | null | undefined) => PendingInteractiveRequests;
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

  function beginSnapshot(): (snapshot: PendingInteractiveRequests | null | undefined) => PendingInteractiveRequests {
    const approvalsAtRequest = new Set(approvals);
    const inputsAtRequest = new Set(userInputs);
    return (snapshot) => {
      approvals = mergePendingRequests(snapshot?.approvals ?? [], approvals, approvalsAtRequest, resolvedApprovalIds);
      userInputs = mergePendingRequests(snapshot?.userInputs ?? [], userInputs, inputsAtRequest, resolvedUserInputIds);
      // The pane and sidebar must project the same reconciled requests.
      return { approvals, userInputs };
    };
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
    beginSnapshot,
    addApproval,
    removeApproval,
    addUserInput,
    removeUserInput,
  };
}
