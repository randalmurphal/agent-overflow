import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { WorkflowItemDetail } from '../../types/workflow';
import { recordWorkflowReceipt, resetWorkflowsPane } from '../../stores/workflowsPane.svelte';
import * as panes from '../../stores/panes.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
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
    vi.restoreAllMocks();
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

  it('loads the PR review count once and navigates both follow-up actions to the linked thread', async () => {
    const value = detail('done', '', JSON.stringify({
      action: 'pr', prRef: 'https://github.com/owner/repo/pull/9', policy: 'manual', at: 1,
    }));
    const thread = {
      id: 'linked-thread', title: 'Workflow triage', provider: 'codex', model: 'gpt-5.4',
      workspacePath: '/tmp/workspace', projectPath: '/tmp/workspace', mode: 'workflow-triage',
      createdAt: 1, updatedAt: 1, archived: false,
    };
    const fetchComments = setBindingMock('WorkflowFetchPRReviewComments', async () => ({ count: 3, threads: [] }));
    const sendComments = setBindingMock('WorkflowSendPRReviewCommentsToThread', async () => thread);
    const discuss = setBindingMock('WorkflowDiscussPR', async () => thread);
    const openThread = vi.spyOn(panes, 'openThreadInNewPane').mockResolvedValue({} as never);

    const view = render(WorkflowActionRow, { detail: value });
    const review = view.getByTestId('wf-pr-review-comments') as HTMLButtonElement;
    await waitFor(() => expect(review).toHaveTextContent('Review comments (3)'));
    expect(review.disabled).toBe(false);
    await view.rerender({ detail: value });
    expect(fetchComments).toHaveBeenCalledTimes(1);

    await fireEvent.click(review);
    await waitFor(() => expect(sendComments).toHaveBeenCalledWith(value.item.id));
    await fireEvent.click(view.getByTestId('wf-pr-discuss'));
    await waitFor(() => expect(discuss).toHaveBeenCalledWith(value.item.id));
    expect(openThread).toHaveBeenCalledTimes(2);
    expect(openThread).toHaveBeenNthCalledWith(1, thread);
    expect(openThread).toHaveBeenNthCalledWith(2, thread);
  });

  it('disables the PR review action with the lazy fetch error as its title', async () => {
    const value = detail('done', '', JSON.stringify({
      action: 'pr', prRef: 'https://github.com/owner/repo/pull/9', policy: 'manual', at: 1,
    }));
    const fetchComments = setBindingMock('WorkflowFetchPRReviewComments', async () => {
      throw new Error('forge unavailable');
    });
    const view = render(WorkflowActionRow, { detail: value });
    const review = view.getByTestId('wf-pr-review-comments') as HTMLButtonElement;
    await waitFor(() => expect(review.title).toBe('Forge unavailable.'));
    expect(review.disabled).toBe(true);
    await view.rerender({ detail: value });
    expect(fetchComments).toHaveBeenCalledTimes(1);
  });

  it('keeps PR follow-up actions local-only without attempting the lazy fetch remotely', () => {
    (globalThis as { __AO_BOOTSTRAP__?: { remote: boolean } }).__AO_BOOTSTRAP__ = { remote: true };
    __resetRunModeForTest();
    const fetchComments = setBindingMock('WorkflowFetchPRReviewComments', async () => ({ count: 1, threads: [] }));
    const value = detail('done', '', JSON.stringify({
      action: 'pr', prRef: 'https://github.com/owner/repo/pull/9', policy: 'manual', at: 1,
    }));

    const view = render(WorkflowActionRow, { detail: value });
    for (const testId of ['wf-pr-review-comments', 'wf-pr-discuss']) {
      const control = view.getByTestId(testId) as HTMLButtonElement;
      expect(control.disabled).toBe(true);
      expect(control.title).toBe('Local only');
    }
    expect(fetchComments).not.toHaveBeenCalled();
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
