import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import type { WorkflowItemDetail, WorkItem } from '../../types/workflow';
import WorkflowEvidence from './WorkflowEvidence.svelte';

function detail(overrides: Partial<WorkflowItemDetail> = {}): WorkflowItemDetail {
  return {
    item: {
      id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run',
      state: 'needs-human', reason: 'setup-failed', sortPosition: 0, createdAt: 1,
    },
    phases: [],
    checkPhaseIds: [],
    callPhaseIds: [],
    units: [],
    children: [],
    outputs: {},
    artifacts: [],
    usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    ...overrides,
  } as unknown as WorkflowItemDetail;
}

function props(view: WorkflowItemDetail) {
  return { item: view.item as unknown as WorkItem, detail: view, kind: 'blocked' as const, failedUnit: null, expandFirstDiff: false };
}

describe('WorkflowEvidence park cause', () => {
  // An engine-diagnosed park ran no turn, so every other evidence block on the
  // page has nothing to show for it. Before the cause was persisted, this state
  // rendered as a parked attempt and no explanation anywhere in the app.
  it('renders the cause of the attempt the run is resting on', () => {
    const view = render(WorkflowEvidence, props(detail({
      phases: [
        { phaseId: 'plan', attempt: 1, status: 'completed', startedAt: 1, endedAt: 2 },
        { phaseId: 'implement', attempt: 1, status: 'parked', startedAt: 3, endedAt: 3, cause: 'provision worktree: branch "ao/wave-3" already exists' },
      ],
    } as unknown as Partial<WorkflowItemDetail>)));

    expect(view.getByTestId('workflow-park-cause'))
      .toHaveTextContent('provision worktree: branch "ao/wave-3" already exists');
  });

  it('renders nothing when no attempt carries an engine-diagnosed cause', () => {
    const view = render(WorkflowEvidence, props(detail({
      phases: [
        { phaseId: 'ask', attempt: 1, status: 'parked', startedAt: 1, endedAt: 2, outputEnvelope: JSON.stringify({ status: 'question', question: 'which base branch?' }) },
      ],
    } as unknown as Partial<WorkflowItemDetail>)));

    expect(view.queryByTestId('workflow-park-cause')).toBeNull();
  });

  // A `--phase` repair leaves the old parked row behind with its cause intact.
  // Only the attempt the run is resting on may supply the diagnosis — scanning
  // back for any attempt with a cause resurrected the repaired park.
  it('does not resurrect an earlier repaired park as the current diagnosis', () => {
    const view = render(WorkflowEvidence, props(detail({
      phases: [
        { phaseId: 'plan', attempt: 1, status: 'parked', startedAt: 1, endedAt: 2, cause: 'provision worktree: branch "ao/wave-3" already exists' },
        { phaseId: 'review', attempt: 1, status: 'parked', startedAt: 3, endedAt: 4, outputEnvelope: JSON.stringify({ status: 'question', question: 'merge order?' }) },
      ],
    } as unknown as Partial<WorkflowItemDetail>)));

    expect(view.queryByTestId('workflow-park-cause')).toBeNull();
  });
});

describe('WorkflowEvidence retries-exhausted (D70)', () => {
  // The reason resolves on the `paused` row because a bare resume continues
  // its session — but the run stopped on a failure, so the receipt must not
  // claim a human paused it, and the diagnosis block must survive the move.
  it('says the run ran out of retries rather than "paused by you", and keeps the failure evidence', () => {
    const view = detail({ phases: [] });
    view.item.reason = 'retries-exhausted';
    const rendered = render(WorkflowEvidence, { ...props(view), kind: 'paused' as const });

    expect(rendered.getByTestId('workflow-paused-receipt')).toHaveTextContent('ran out of retries');
    expect(rendered.getByTestId('workflow-paused-receipt')).not.toHaveTextContent('paused by you');
    expect(rendered.getByTestId('wf-failure-evidence')).toBeTruthy();
  });

  it('still reads a genuine pause as one', () => {
    const view = detail({ phases: [] });
    view.item.reason = 'paused';
    const rendered = render(WorkflowEvidence, { ...props(view), kind: 'paused' as const });

    expect(rendered.getByTestId('workflow-paused-receipt')).toHaveTextContent('paused by you');
    expect(rendered.queryByTestId('wf-failure-evidence')).toBeNull();
  });
});
