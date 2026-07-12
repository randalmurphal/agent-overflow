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
  });
});
