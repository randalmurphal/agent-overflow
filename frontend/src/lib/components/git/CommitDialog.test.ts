import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import CommitDialog from './CommitDialog.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane } from '../../../test/helpers/chat';

// Partial mock: only the snapshot is pinned. A whole-module factory would
// have to re-declare every export, so any new one silently becomes undefined
// here (isMethodUnavailableError did exactly that).
vi.mock('../../stores/transportStatus.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/transportStatus.svelte')>()),
  getTransportStatus: () => ({ status: 'connected', nextAttemptAt: null }),
}));

describe('<CommitDialog> — Generate commit message', () => {
  beforeEach(() => {
    resetPanesForTest();
    resetBindingMocks();
  });

  it('drafts subject and body through GenerateCommitMessage', async () => {
    const generate = setBindingMock('GenerateCommitMessage', async () => ({
      subject: 'Add login flow',
      body: 'Supports SSO.',
    }));
    const pane = await buildPane();
    const { getByTestId } = render(CommitDialog, {
      props: { pane, open: true, onClose: vi.fn() },
    });

    await fireEvent.click(getByTestId('commit-dialog-generate'));

    await waitFor(() => {
      const subject = document.getElementById('commit-subject') as HTMLInputElement;
      const body = document.getElementById('commit-body') as HTMLTextAreaElement;
      expect(subject.value).toBe('Add login flow');
      expect(body.value).toBe('Supports SSO.');
    });
    expect(generate).toHaveBeenCalledWith(pane.threadId);
  });

  it('keeps the drafted fields editable after a failed generation', async () => {
    setBindingMock('GenerateCommitMessage', async () => {
      throw new Error('no uncommitted changes to describe');
    });
    const pane = await buildPane();
    const { getByTestId } = render(CommitDialog, {
      props: { pane, open: true, onClose: vi.fn() },
    });

    await fireEvent.click(getByTestId('commit-dialog-generate'));

    await waitFor(() => {
      const button = getByTestId('commit-dialog-generate') as HTMLButtonElement;
      expect(button.disabled).toBe(false);
      expect(button.textContent?.trim()).toBe('Generate');
    });
    const subject = document.getElementById('commit-subject') as HTMLInputElement;
    expect(subject.value).toBe('');
  });
});
