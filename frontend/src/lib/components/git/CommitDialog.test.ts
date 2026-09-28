import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import CommitDialog from './CommitDialog.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { __seedGitStatusForTest } from '../../stores/gitStatusStore.svelte';
import type { GitActionResult, GitStatus } from '../../types/git';
import { buildPane, makeThread } from '../../../test/helpers/chat';

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
    expect(generate).toHaveBeenCalledWith(pane.workspace);
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

// makeThread's default checkout, which is the git-status store key the
// dialog reads the pending operation through.
const WORKSPACE = '/tmp/workspace';

function status(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true,
    branch: 'feature',
    isDefaultBranch: false,
    hasChanges: true,
    insertions: 1,
    deletions: 0,
    fileCount: 1,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    ...overrides,
  };
}

async function mountWithStatus(s: GitStatus) {
  const pane = await buildPane(makeThread());
  __seedGitStatusForTest(WORKSPACE, s);
  const onClose = vi.fn();
  const rendered = render(CommitDialog, { props: { pane, open: true, onClose } });
  return { pane, onClose, ...rendered };
}

describe('<CommitDialog> — pending operation notice', () => {
  beforeEach(() => {
    resetPanesForTest();
    resetBindingMocks();
  });

  it.each([
    ['merge', 'Committing completes the merge.'],
    ['rebase', 'git rebase --continue'],
    ['bisect', 'git bisect reset'],
  ])('shows an informational notice for a pending %s and still allows committing', async (op, text) => {
    const commit = setBindingMock('GitCommit', async () => ({
      action: 'commit',
      commitSha: 'abcdef0123456789',
    } as GitActionResult));
    const { pane, onClose, getByTestId, getByText } = await mountWithStatus(status({ pendingOperation: op }));

    const notice = getByTestId('commit-dialog-pending-operation');
    expect(notice.textContent).toContain(text);
    expect(notice.getAttribute('role')).toBe('status');

    await fireEvent.input(document.getElementById('commit-subject')!, { target: { value: 'Finish it' } });
    const button = getByText('Commit').closest('button') as HTMLButtonElement;
    expect(button.disabled).toBe(false);
    await fireEvent.click(button);
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    expect(commit).toHaveBeenCalledWith(pane.workspace, 'Finish it', '');
  });

  it('renders no notice when no operation is pending', async () => {
    const { queryByTestId } = await mountWithStatus(status({ pendingOperation: '' }));
    expect(queryByTestId('commit-dialog-pending-operation')).toBeNull();
  });

  it('surfaces git refusing the commit as the inline error and stays open', async () => {
    setBindingMock('GitCommit', async () => ({
      action: 'commit',
      error: 'Committing is not possible because you have unmerged files.',
    } as GitActionResult));
    const { onClose, getByText, findByRole } = await mountWithStatus(status({ pendingOperation: 'merge' }));
    await fireEvent.input(document.getElementById('commit-subject')!, { target: { value: 'Finish it' } });
    await fireEvent.click(getByText('Commit').closest('button')!);

    const alert = await findByRole('alert');
    expect(alert.textContent).toContain('unmerged files');
    expect(onClose).not.toHaveBeenCalled();
  });

  it('surfaces a thrown commit error inline', async () => {
    setBindingMock('GitCommit', async () => {
      throw new Error('git commit exited with code 128');
    });
    const { onClose, getByText, findByRole } = await mountWithStatus(status());
    await fireEvent.input(document.getElementById('commit-subject')!, { target: { value: 'Finish it' } });
    await fireEvent.click(getByText('Commit').closest('button')!);

    const alert = await findByRole('alert');
    expect(alert.textContent).toContain('exited with code 128');
    expect(onClose).not.toHaveBeenCalled();
  });
});
