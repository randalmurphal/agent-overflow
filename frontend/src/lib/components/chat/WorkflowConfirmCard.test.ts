import { render, fireEvent } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import WorkflowConfirmCard from './WorkflowConfirmCard.svelte';

describe('WorkflowConfirmCard', () => {
  it('renders the proposal and never commits without Queue it', async () => {
    const onQueue = vi.fn();
    const onEdit = vi.fn();
    const onDismiss = vi.fn();
    const prefill = { projectId: 'p', goal: 'Fix it' };
    const view = render(WorkflowConfirmCard, {
      projectName: 'AO', title: 'Fix it', workflowName: 'Build', baseBranch: 'main',
      prefill, onQueue, onEdit, onDismiss,
    });
    expect(view.getByTestId('wf-confirm-card')).toHaveTextContent('AO · Fix it · Build · main');
    expect(onQueue).not.toHaveBeenCalled();
    await fireEvent.click(view.getByTestId('wf-confirm-edit'));
    expect(onEdit).toHaveBeenCalledWith(prefill);
    expect(onQueue).not.toHaveBeenCalled();
    await fireEvent.click(view.getByTestId('wf-confirm-queue'));
    expect(onQueue).toHaveBeenCalledWith(prefill);
    await fireEvent.click(view.getByTestId('wf-confirm-dismiss'));
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it('disables every pending action remotely and renders resolved receipts without actions', () => {
    const props = {
      projectName: 'AO', title: 'Fix it', workflowName: 'Build', baseBranch: 'main',
      prefill: {}, onQueue: vi.fn(), onEdit: vi.fn(), onDismiss: vi.fn(),
    };
    const disabled = render(WorkflowConfirmCard, { ...props, disabled: true });
    for (const id of ['wf-confirm-queue', 'wf-confirm-edit', 'wf-confirm-dismiss']) {
      expect(disabled.getByTestId(id)).toBeDisabled();
      expect(disabled.getByTestId(id)).toHaveAttribute('title', 'Local only');
    }
    disabled.unmount();
    const queued = render(WorkflowConfirmCard, { ...props, state: 'queued' as const });
    expect(queued.getByTestId('wf-confirm-receipt')).toHaveTextContent('Added to Up next');
    expect(queued.queryByTestId('wf-confirm-queue')).toBeNull();
  });
});
