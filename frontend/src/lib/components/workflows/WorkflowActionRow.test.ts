import { render } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import type { WorkflowItemDetail } from '../../types/workflow';
import { resetWorkflowsPane } from '../../stores/workflowsPane.svelte';
import WorkflowActionRow from './WorkflowActionRow.svelte';

function detail(state: string, reason = '', disposition = ''): WorkflowItemDetail {
  return {
    item: { id: `run-${state}-${reason}`, state, reason, disposition },
    phases: [], artifacts: [], usage: { costUsd: 1 },
  } as unknown as WorkflowItemDetail;
}

describe('WorkflowActionRow', () => {
  beforeEach(resetWorkflowsPane);

  it.each([
    [detail('needs-human', 'gate'), 'wf-approve'],
    [detail('needs-human', 'question'), 'wf-answer-input'],
    [detail('needs-human', 'stuck'), 'wf-parked-continue'],
    [detail('failed', 'check-failed-genuine'), 'wf-continue-agent'],
    [detail('done'), 'wf-merge'],
    [detail('running'), 'wf-open-phase'],
    [detail('queued'), 'wf-remove-queued'],
    [detail('cancelled'), 'wf-discard-worktree'],
  ])('renders the action row for %#', (value, testId) => {
    const view = render(WorkflowActionRow, { detail: value });
    expect(view.getByTestId(testId)).toBeInTheDocument();
  });

  it('renders a persisted disposition receipt instead of manual disposition buttons', () => {
    const view = render(WorkflowActionRow, {
      detail: detail('done', '', '{"action":"merged","mode":"ff","sha":"abc","policy":"manual","at":1}'),
    });
    expect(view.getByTestId('wf-disposition-receipt')).toHaveTextContent('Merged to main');
    expect(view.getByTestId('wf-disposition-receipt')).toHaveTextContent('fast-forward · abc');
    expect(view.getByTestId('wf-disposition-receipt')).toHaveTextContent('policy · manual');
    expect(view.queryByTestId('wf-merge')).not.toBeInTheDocument();
  });
});
