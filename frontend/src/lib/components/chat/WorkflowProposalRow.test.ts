import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { Item } from '../../types/models';
import { __resetRunModeForTest } from '../../transport/runMode';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import WorkflowProposalRow from './WorkflowProposalRow.svelte';

function proposal(state: 'pending' | 'queued' | 'dismissed' = 'pending'): Item {
  return {
    id: 'workflow-proposal:1', threadId: 'thread-1', turnIndex: 1, itemIndex: 2,
    kind: 'workflow_proposal', role: 'assistant', status: 'completed', summary: 'Ship it',
    meta: JSON.stringify({
      state, projectId: 'project-1', projectName: 'AO', workflowId: 'build',
      workflowName: 'Build', workflowScope: 'shared', goal: 'Ship it',
      seeds: { ticket: 'AO-1' }, baseBranch: 'main', stepMode: false,
    }),
    createdAt: 1, updatedAt: 1,
  };
}

describe('WorkflowProposalRow', () => {
  beforeEach(() => {
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
    resetBindingMocks();
    setBindingMock('WorkflowQueueChatProposal', async () => ({ id: 'work-1' }));
    setBindingMock('WorkflowDismissChatProposal', async () => undefined);
  });

  afterEach(() => {
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
    resetBindingMocks();
  });

  it('queues and dismisses pending cards through proposal-specific bindings', async () => {
    const queued = render(WorkflowProposalRow, { item: proposal() });
    await fireEvent.click(queued.getByTestId('wf-confirm-queue'));
    await waitFor(() => expect(getBindingMock('WorkflowQueueChatProposal')).toHaveBeenCalledWith(
      'thread-1', 'workflow-proposal:1', 'project-1', 'build', 'shared', 'Ship it',
      { ticket: 'AO-1' }, 'main', false,
    ));
    queued.unmount();

    const dismissed = render(WorkflowProposalRow, { item: proposal() });
    await fireEvent.click(dismissed.getByTestId('wf-confirm-dismiss'));
    await waitFor(() => expect(getBindingMock('WorkflowDismissChatProposal')).toHaveBeenCalledWith(
      'thread-1', 'workflow-proposal:1',
    ));
  });

  it('renders queued state after reload and disables pending actions remotely', () => {
    const resolved = render(WorkflowProposalRow, { item: proposal('queued') });
    expect(resolved.getByTestId('wf-confirm-receipt')).toHaveTextContent('Added to Up next');
    resolved.unmount();

    (globalThis as { __AO_BOOTSTRAP__?: { remote: boolean } }).__AO_BOOTSTRAP__ = { remote: true };
    __resetRunModeForTest();
    const remote = render(WorkflowProposalRow, { item: proposal() });
    for (const id of ['wf-confirm-queue', 'wf-confirm-edit', 'wf-confirm-dismiss']) {
      expect(remote.getByTestId(id)).toBeDisabled();
      expect(remote.getByTestId(id)).toHaveAttribute('title', 'Local only');
    }
  });
});
