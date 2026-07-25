import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import type { WorkflowItemDetail } from '../../types/workflow';
import WorkflowFailureEvidence from './WorkflowFailureEvidence.svelte';

function detail(overrides: Partial<WorkflowItemDetail> = {}): WorkflowItemDetail {
  return {
    item: {
      id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run',
      state: 'failed', reason: 'check-failed-genuine', sortPosition: 0, createdAt: 1,
    },
    phases: [],
    checkPhaseIds: [],
    outputs: {},
    artifacts: [],
    usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    ...overrides,
  } as unknown as WorkflowItemDetail;
}

describe('WorkflowFailureEvidence', () => {
  it('renders the latest failed check and the latest diagnosis attempt', () => {
    const view = render(WorkflowFailureEvidence, {
      detail: detail({
        checkPhaseIds: ['build'],
        phases: [
          { phaseId: 'build', attempt: 1, status: 'completed', startedAt: 1, endedAt: 2, outputEnvelope: JSON.stringify({ status: 'done', outputs: { passed: false, details: 'Old failure' } }) },
          { phaseId: 'build', attempt: 3, status: 'completed', startedAt: 3, endedAt: 4, outputEnvelope: JSON.stringify({ status: 'done', outputs: { passed: false, details: 'TestParallelDispatch' } }) },
          { phaseId: 'diagnose', attempt: 3, status: 'completed', startedAt: 5, endedAt: 6, outputEnvelope: JSON.stringify({ status: 'done', outputs: { classification: 'genuine', diagnosis: 'claim ordering drops one event' } }) },
        ],
      } as unknown as Partial<WorkflowItemDetail>),
    });

    expect(view.getByTestId('wf-failure-check')).toHaveTextContent('✗ build — TestParallelDispatch ×3 · genuine');
    expect(view.getByTestId('wf-failure-diagnosis')).toHaveTextContent('diagnosis #3: “claim ordering drops one event”');
  });

  it('falls back to the item reason when no check phase failed', () => {
    const view = render(WorkflowFailureEvidence, { detail: detail({ item: { ...detail().item, reason: 'agent-error' } } as unknown as Partial<WorkflowItemDetail>) });

    expect(view.getByTestId('wf-failure-check')).toHaveTextContent('✗ agent-error');
    expect(view.queryByTestId('wf-failure-diagnosis')).not.toBeInTheDocument();
  });

  it('ignores an unparseable envelope instead of throwing', () => {
    const view = render(WorkflowFailureEvidence, {
      detail: detail({
        checkPhaseIds: ['build'],
        phases: [{ phaseId: 'build', attempt: 1, status: 'failed', startedAt: 1, endedAt: 2, outputEnvelope: '{not json' }],
      } as unknown as Partial<WorkflowItemDetail>),
    });

    expect(view.getByTestId('wf-failure-check')).toHaveTextContent('✗ build — check failed ×1 · genuine');
  });
});
