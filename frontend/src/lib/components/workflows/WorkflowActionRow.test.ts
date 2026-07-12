import { render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { WorkflowItemDetail } from '../../types/workflow';
import { resetWorkflowsPane } from '../../stores/workflowsPane.svelte';
import WorkflowActionRow from './WorkflowActionRow.svelte';
import { __resetRunModeForTest } from '../../transport/runMode';

function detail(state: string, reason = '', disposition = ''): WorkflowItemDetail {
  return {
    item: { id: `run-${state}-${reason}`, state, reason, disposition },
    phases: [], artifacts: [], usage: { costUsd: 1 },
  } as unknown as WorkflowItemDetail;
}

describe('WorkflowActionRow', () => {
  beforeEach(() => {
    resetWorkflowsPane();
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
  });

  afterEach(() => {
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
  });

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

  it('disables mutating action controls but keeps plain phase navigation live remotely', () => {
    (globalThis as { __AO_BOOTSTRAP__?: { remote: boolean } }).__AO_BOOTSTRAP__ = { remote: true };
    __resetRunModeForTest();

    const gate = render(WorkflowActionRow, { detail: detail('needs-human', 'gate') });
    for (const testId of ['wf-approve', 'wf-request-changes', 'wf-take-over']) {
      const control = gate.getByTestId(testId) as HTMLButtonElement;
      expect(control.disabled).toBe(true);
      expect(control.title).toBe('Local only');
    }
    gate.unmount();

    const running = render(WorkflowActionRow, { detail: detail('running') });
    expect((running.getByTestId('wf-open-phase') as HTMLButtonElement).disabled).toBe(false);
    expect((running.getByTestId('wf-stop-run') as HTMLButtonElement).disabled).toBe(true);
  });
});
