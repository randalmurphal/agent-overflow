import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { __resetRunModeForTest } from '../../transport/runMode';
import { clearPairedSession, redeemPairing, type PairingPayload } from '../../transport/deviceSession';
import { refreshGrantedScopes, setPageGrantsFromBootstrap } from '../../transport/scopes';
import WorkflowJobNotes from './WorkflowJobNotes.svelte';

describe('WorkflowJobNotes', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    __resetRunModeForTest();
    resetBindingMocks();
    setBindingMock('WorkflowGetJobNotes', async () => 'existing');
    setBindingMock('WorkflowSetJobNotes', async () => undefined);
  });

  afterEach(() => {
    vi.useRealTimers();
    __resetRunModeForTest();
    resetBindingMocks();
    clearPairedSession();
    setPageGrantsFromBootstrap(false);
  });

  // Enrol a paired session holding exactly `scopes`, the way the pairing
  // flow does: redeem, then re-resolve (wsClient.redialAfterPairing()'s job).
  async function pairedWith(scopes: string[]): Promise<void> {
    const payload: PairingPayload = {
      v: 1, backendId: 'b', endpoint: 'http://192.168.1.20:8123', token: 't',
    };
    const fetcher = vi.fn(async () => new Response(
      JSON.stringify({ sessionId: 's', credential: 'c', expiresAtMs: Date.now() + 6e5, scopes }),
      { status: 200, headers: { 'Content-Type': 'application/json' } },
    )) as unknown as typeof fetch;
    await redeemPairing(payload, 'a phone', fetcher);
    refreshGrantedScopes();
  }

  it('cancels a pending save when the page turns out to hold no grant', async () => {
    const view = render(WorkflowJobNotes, { automationId: 'job' });
    await fireEvent.click(view.getByTestId('wf-job-notes-toggle'));
    await waitFor(() => expect(view.getByTestId('wf-job-notes-input')).toHaveValue('existing'));
    await fireEvent.input(view.getByTestId('wf-job-notes-input'), { target: { value: 'changed' } });
    setPageGrantsFromBootstrap(true);
    await vi.advanceTimersByTimeAsync(350);
    expect(view.getByTestId('wf-job-notes-input')).toBeDisabled();
    expect(view.getByTestId('wf-job-notes-input')).toHaveAttribute('title', 'Local only');
    expect(getBindingMock('WorkflowSetJobNotes')).not.toHaveBeenCalled();
  });

  // The axis the capability model exists for: two networked sessions,
  // differing only in what they were granted, must get different screens.
  // The old boolean could not tell them apart at all.
  it('follows the paired session\u2019s own grant, not the page it loaded from', async () => {
    setPageGrantsFromBootstrap(true);
    await pairedWith(['threads:read', 'files:read']);
    const observer = render(WorkflowJobNotes, { automationId: 'job' });
    await fireEvent.click(observer.getByTestId('wf-job-notes-toggle'));
    await waitFor(() => expect(observer.getByTestId('wf-job-notes-input')).toBeDisabled());
    observer.unmount();

    await pairedWith(['threads:read', 'threads:autonomy']);
    const operator = render(WorkflowJobNotes, { automationId: 'job' });
    await fireEvent.click(operator.getByTestId('wf-job-notes-toggle'));
    await waitFor(() => expect(operator.getByTestId('wf-job-notes-input')).toHaveValue('existing'));
    expect(operator.getByTestId('wf-job-notes-input')).toBeEnabled();

    await fireEvent.input(operator.getByTestId('wf-job-notes-input'), { target: { value: 'changed' } });
    await vi.advanceTimersByTimeAsync(350);
    expect(getBindingMock('WorkflowSetJobNotes')).toHaveBeenCalledWith('job', 'changed');
  });
});
