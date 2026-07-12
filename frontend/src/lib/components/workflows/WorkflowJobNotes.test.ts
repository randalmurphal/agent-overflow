import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { __resetRunModeForTest, setViewOnlySessionFromBootstrap } from '../../transport/runMode';
import WorkflowJobNotes from './WorkflowJobNotes.svelte';

describe('WorkflowJobNotes', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
    resetBindingMocks();
    setBindingMock('WorkflowGetJobNotes', async () => 'existing');
    setBindingMock('WorkflowSetJobNotes', async () => undefined);
  });

  afterEach(() => {
    vi.useRealTimers();
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
    resetBindingMocks();
  });

  it('cancels a pending save when the bootstrap changes to view-only', async () => {
    const view = render(WorkflowJobNotes, { automationId: 'job' });
    await fireEvent.click(view.getByTestId('wf-job-notes-toggle'));
    await waitFor(() => expect(view.getByTestId('wf-job-notes-input')).toHaveValue('existing'));
    await fireEvent.input(view.getByTestId('wf-job-notes-input'), { target: { value: 'changed' } });
    setViewOnlySessionFromBootstrap(true);
    await vi.advanceTimersByTimeAsync(350);
    expect(view.getByTestId('wf-job-notes-input')).toBeDisabled();
    expect(view.getByTestId('wf-job-notes-input')).toHaveAttribute('title', 'Local only');
    expect(getBindingMock('WorkflowSetJobNotes')).not.toHaveBeenCalled();
  });
});
