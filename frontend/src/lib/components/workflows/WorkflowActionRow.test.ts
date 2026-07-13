import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { WorkflowItemDetail } from '../../types/workflow';
import { recordWorkflowReceipt, resetWorkflowsPane } from '../../stores/workflowsPane.svelte';
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
      detail: detail('done', '', '{"action":"merged","base":"main","mode":"ff","sha":"abc","policy":"manual","at":1}'),
    });
    expect(view.getByTestId('wf-disposition-receipt')).toHaveTextContent('Merged to main');
    expect(view.getByTestId('wf-disposition-receipt')).toHaveTextContent('fast-forward · abc');
    expect(view.getByTestId('wf-disposition-receipt')).toHaveTextContent('policy · manual');
    expect(view.queryByTestId('wf-merge')).not.toBeInTheDocument();
  });

  it('offers done-manual actions for a disposition park', () => {
    const view = render(WorkflowActionRow, { detail: detail('needs-human', 'disposition') });
    expect(view.getByTestId('wf-merge')).toBeInTheDocument();
    expect(view.getByTestId('wf-create-pr')).toBeInTheDocument();
    expect(view.getByTestId('wf-done-continue')).toBeInTheDocument();
    expect(view.getByTestId('wf-done-discard')).toBeInTheDocument();
    expect(view.queryByTestId('wf-parked-resume')).not.toBeInTheDocument();
  });

  it('shows a failed run disposition receipt without offering re-enqueue', () => {
    const view = render(WorkflowActionRow, {
      detail: detail('failed', 'agent-error', '{"action":"discarded","policy":"manual","at":1}'),
    });
    expect(view.getByTestId('wf-disposition-receipt')).toHaveTextContent('Discarded');
    expect(view.queryByTestId('wf-resume')).not.toBeInTheDocument();
  });

  it('co-renders the session receipt above the persisted disposition', () => {
    const value = detail('done', '', '{"action":"merged","base":"release","mode":"ff","sha":"abc","policy":"manual","at":1}');
    recordWorkflowReceipt({ itemId: value.item.id, kind: 'merged', message: 'Merged to release', costUsd: 1 }, false);
    const view = render(WorkflowActionRow, { detail: value });
    expect(view.getByTestId('wf-resolved-receipt')).toHaveTextContent('Merged to release');
    expect(view.getByTestId('wf-disposition-receipt')).toHaveTextContent('Merged to release');
    expect(view.queryByTestId('wf-receipt-continue')).not.toBeInTheDocument();
  });

  it('focuses the reject note when request changes opens', async () => {
    const view = render(WorkflowActionRow, { detail: detail('needs-human', 'gate') });
    await fireEvent.click(view.getByTestId('wf-request-changes'));
    const input = await view.findByTestId('wf-reject-note');
    await waitFor(() => expect(input).toHaveFocus());
  });

  it('names the next phase in gate approval when one exists', () => {
    const view = render(WorkflowActionRow, { detail: detail('needs-human', 'gate'), approveTarget: 'docs' });
    expect(view.getByTestId('wf-approve')).toHaveTextContent('Approve → docs');
  });

  it('renders automatic merge policy and undo receipt rows', () => {
    const value = detail('done', '', JSON.stringify({
      action: 'merged', base: 'main', mode: 'ff', sha: 'abc123', policy: 'auto', at: 1,
    }));
    value.item.branch = 'run/work';
    const view = render(WorkflowActionRow, { detail: value });
    const receipt = view.getByTestId('wf-disposition-receipt');
    expect(receipt).toHaveTextContent('Merged automatically');
    expect(receipt).toHaveTextContent('run/work → main · fast-forward · abc123');
    expect(receipt).toHaveTextContent('project opted in; a conflict or dirty base parks for you instead');
    expect(receipt).toHaveTextContent('git revert abc123');
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
