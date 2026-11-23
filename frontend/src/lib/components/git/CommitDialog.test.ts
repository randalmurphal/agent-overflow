import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor, within } from '@testing-library/svelte';
import CommitDialog from './CommitDialog.svelte';
import { setBindingMock, setReviewDiffMock } from '../../../test/mocks/bindings-app';
import { __seedGitStatusForTest } from '../../stores/gitStatusStore.svelte';
import type { CommitFile, CommitFiles, GitActionOutcome, GitActionResult, GitStatus } from '../../types/git';
import { DisconnectedError, TransportError } from '../../transport/wsClient';
import { COMMIT_ANSWER_LOST, COMMIT_OUTCOME_UNKNOWN } from './commitDialogState.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';

// Partial mock: only the snapshot is pinned. A whole-module factory would
// have to re-declare every export, so any new one silently becomes undefined
// here (isMethodUnavailableError did exactly that).
vi.mock('../../stores/transportStatus.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/transportStatus.svelte')>()),
  getTransportStatus: () => ({ status: 'connected', nextAttemptAt: null }),
}));

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

function file(path: string, overrides: Partial<CommitFile> = {}): CommitFile {
  return { path, status: 'M', additions: 1, deletions: 0, version: `100644:${path}`, ...overrides };
}

function listing(files: CommitFile[], overrides: Partial<CommitFiles> = {}): CommitFiles {
  return { files, total: files.length, wholeOnly: false, ...overrides };
}

const THREE = [
  file('src/a.ts', { additions: 3, deletions: 1 }),
  file('src/new.ts', { status: 'R', oldPath: 'src/old.ts', additions: 0 }),
  file('z.txt', { status: 'A', partlyStaged: true }),
];

const PATCH = [
  'diff --git a/src/a.ts b/src/a.ts',
  '--- a/src/a.ts',
  '+++ b/src/a.ts',
  '@@ -1 +1 @@',
  '-before',
  '+after',
  '',
].join('\n');

async function mount(
  answer: CommitFiles | (() => CommitFiles | Promise<CommitFiles>) = listing(THREE),
  s: GitStatus = status(),
) {
  const list = setBindingMock('ListCommitFiles', async () => (typeof answer === 'function' ? answer() : answer));
  const diff = setReviewDiffMock('OpenCommitFileDiff', () => PATCH);
  const pane = await buildPane(makeThread());
  __seedGitStatusForTest(WORKSPACE, s);
  const onClose = vi.fn();
  const rendered = render(CommitDialog, { props: { pane, open: true, onClose } });
  await waitFor(() => expect(list).toHaveBeenCalled());
  const rows = () => rendered.queryAllByTestId('commit-file-row');
  const row = (path: string) => {
    const found = rows().find((el) => el.getAttribute('aria-label') === path);
    if (!found) throw new Error(`no row for ${path}`);
    return found;
  };
  return {
    ...rendered,
    pane,
    onClose,
    list,
    diff,
    rows,
    row,
    check: (path: string) => within(row(path)).getByTestId('commit-file-check'),
    commitButton: () => rendered.getByText('Commit').closest('button') as HTMLButtonElement,
    setSubject: async (value: string) => {
      await fireEvent.input(document.getElementById('commit-subject')!, { target: { value } });
    },
  };
}

describe('<CommitDialog> — file selection', () => {
  it('lists the changed files with nothing selected and refuses to commit until one is', async () => {
    const commit = setBindingMock('GitCommit', async () => ({ action: 'commit', commitSha: 'abcdef0123456789' } as GitActionResult));
    const m = await mount();
    await waitFor(() => expect(m.rows()).toHaveLength(3));

    expect(m.rows().map((el) => el.getAttribute('aria-selected'))).toEqual(['false', 'false', 'false']);
    expect(m.getByTestId('commit-dialog-selection').textContent?.trim()).toBe('0 of 3 files selected');
    expect(m.row('z.txt').textContent).toContain('partly staged');
    expect(m.row('src/new.ts').getAttribute('title')).toBe('src/old.ts → src/new.ts');

    await m.setSubject('Pick');
    expect(m.commitButton().disabled).toBe(true);
    expect((m.getByTestId('commit-dialog-generate') as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(m.commitButton());
    expect(commit).not.toHaveBeenCalled();
  });

  it('commits exactly the selected rows, a rename as both of its paths, then stays open on the rest', async () => {
    let files = THREE;
    const commit = setBindingMock('GitCommit', async () => {
      files = [THREE[2]];
      return { action: 'commit', commitSha: 'abcdef0123456789' } as GitActionResult;
    });
    const m = await mount(() => listing(files));
    await waitFor(() => expect(m.rows()).toHaveLength(3));

    await fireEvent.click(m.check('src/a.ts'));
    await fireEvent.click(m.check('src/new.ts'));
    expect(m.getByTestId('commit-dialog-selection').textContent?.trim()).toBe('2 of 3 files selected');
    await m.setSubject('First');
    await fireEvent.click(m.commitButton());

    await waitFor(() => expect(m.rows()).toHaveLength(1));
    expect(commit).toHaveBeenCalledWith(m.pane.workspace, 'First', '', { all: false, files: [THREE[0], THREE[1]] }, expect.any(String));
    expect(m.onClose).not.toHaveBeenCalled();
    expect((document.getElementById('commit-subject') as HTMLInputElement).value).toBe('');
    expect(m.row('z.txt').getAttribute('aria-selected')).toBe('false');
    expect(m.commitButton().disabled).toBe(true);
  });

  it('closes once a commit leaves nothing to commit', async () => {
    let files = [THREE[0]];
    setBindingMock('GitCommit', async () => {
      files = [];
      return { action: 'commit', commitSha: 'abcdef0123456789' } as GitActionResult;
    });
    const m = await mount(() => listing(files));
    await waitFor(() => expect(m.rows()).toHaveLength(1));
    await fireEvent.click(m.check('src/a.ts'));
    await m.setSubject('Last');
    await fireEvent.click(m.commitButton());
    await waitFor(() => expect(m.onClose).toHaveBeenCalledTimes(1));
  });

  it('stays open with a message when it opens on a clean checkout', async () => {
    const m = await mount(listing([]));
    await waitFor(() => expect(m.getByTestId('commit-dialog-empty').textContent).toContain('No changes to commit.'));
    expect(m.onClose).not.toHaveBeenCalled();
  });

  it('selects and clears every row from the button and from the keyboard', async () => {
    const m = await mount();
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    const toggle = m.getByTestId('commit-dialog-toggle-all');
    const selection = () => m.rows().map((el) => el.getAttribute('aria-selected'));

    expect(toggle.textContent?.trim()).toBe('Select all');
    await fireEvent.click(toggle);
    expect(selection()).toEqual(['true', 'true', 'true']);
    expect(toggle.textContent?.trim()).toBe('Select none');

    const listbox = m.getByTestId('commit-file-list');
    await fireEvent.keyDown(listbox, { key: 'a' });
    expect(selection()).toEqual(['false', 'false', 'false']);
    await fireEvent.keyDown(listbox, { key: 'a' });
    expect(selection()).toEqual(['true', 'true', 'true']);
  });

  it('moves the active row with the arrows and toggles it with space', async () => {
    const m = await mount();
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    const listbox = m.getByTestId('commit-file-list');
    const activePath = () => m.rows().find((el) => el.hasAttribute('data-active'))?.getAttribute('aria-label');

    expect(activePath()).toBe('src/a.ts');
    await fireEvent.keyDown(listbox, { key: 'ArrowDown' });
    expect(activePath()).toBe('src/new.ts');
    expect(listbox.getAttribute('aria-activedescendant')).toBe(m.row('src/new.ts').id);
    await fireEvent.keyDown(listbox, { key: ' ' });
    expect(m.row('src/new.ts').getAttribute('aria-selected')).toBe('true');
    await fireEvent.keyDown(listbox, { key: 'End' });
    expect(activePath()).toBe('z.txt');
    await fireEvent.keyDown(listbox, { key: 'ArrowDown' });
    expect(activePath()).toBe('z.txt');
    await fireEvent.keyDown(listbox, { key: 'ArrowUp' });
    await fireEvent.keyDown(listbox, { key: 'Home' });
    expect(activePath()).toBe('src/a.ts');
    // A modified key belongs to the dialog or the browser, not the list.
    await fireEvent.keyDown(listbox, { key: 'a', ctrlKey: true });
    expect(m.row('src/a.ts').getAttribute('aria-selected')).toBe('false');
  });

  it('commits on ctrl+enter from the subject field', async () => {
    const commit = setBindingMock('GitCommit', async () => ({ action: 'commit', commitSha: 'abcdef0123456789' } as GitActionResult));
    const m = await mount();
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    await fireEvent.click(m.check('z.txt'));
    await m.setSubject('Keys');
    await fireEvent.keyDown(document.getElementById('commit-subject')!, { key: 'Enter', ctrlKey: true });
    await waitFor(() => expect(commit).toHaveBeenCalledWith(m.pane.workspace, 'Keys', '', { all: false, files: [THREE[2]] }, expect.any(String)));
  });

  it('keeps the selection across a refresh and leaves files that appear unselected', async () => {
    let files = [THREE[0], THREE[2]];
    const m = await mount(() => listing(files));
    await waitFor(() => expect(m.rows()).toHaveLength(2));
    await fireEvent.click(m.check('src/a.ts'));
    await fireEvent.click(m.check('z.txt'));

    files = [THREE[0], file('appeared.ts', { status: 'A' })];
    // The refresh waits out the scheduler's interval after the first
    // listing (a second), on a fake clock.
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    try {
      __seedGitStatusForTest(WORKSPACE, status({ fileCount: 2, insertions: 9 }));
      await vi.advanceTimersByTimeAsync(1_000);
    } finally {
      vi.useRealTimers();
    }
    await waitFor(() => expect(m.list).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(m.row('appeared.ts').getAttribute('aria-selected')).toBe('false'));
    expect(m.rows()).toHaveLength(2);
    expect(m.row('src/a.ts').getAttribute('aria-selected')).toBe('true');
    expect(m.getByTestId('commit-dialog-selection').textContent?.trim()).toBe('1 of 2 files selected');
  });

  it('previews the active file through OpenCommitFileDiff and follows the active row', async () => {
    const m = await mount();
    await waitFor(() => expect(m.diff).toHaveBeenCalledWith(m.pane.workspace, ['src/a.ts']));
    await waitFor(() => expect(m.getByTestId('commit-file-preview').textContent).toContain('+after'));

    await fireEvent.click(m.row('src/new.ts'));
    await waitFor(() => expect(m.diff).toHaveBeenLastCalledWith(m.pane.workspace, ['src/old.ts', 'src/new.ts']));
    // Activating a row previews it; it does not select it.
    expect(m.row('src/new.ts').getAttribute('aria-selected')).toBe('false');
  });

  it('shows a list failure in the dialog', async () => {
    const m = await mount(() => {
      throw new Error('list commit files: git exploded');
    });
    const alert = await m.findByTestId('commit-dialog-list-error');
    expect(alert.textContent).toBe('Git exploded.');
    expect(m.rows()).toHaveLength(0);
  });

  it('says to update a connected computer whose build has no file list', async () => {
    const m = await mount(() => {
      throw Object.assign(new Error('method not found'), { code: 'method_not_found' });
    });
    const alert = await m.findByTestId('commit-dialog-list-error');
    expect(alert.textContent).toBe('Update Agent Overflow on this computer to commit from here.');
    await m.setSubject('Nothing listed');
    expect(m.commitButton().disabled).toBe(true);
  });

  it('reads the preview again when the active file changes without its line counts moving', async () => {
    let files = [THREE[0]];
    const m = await mount(() => listing(files));
    await waitFor(() => expect(m.diff).toHaveBeenCalledTimes(1));
    // A listing after one that just ran waits out the scheduler's interval
    // (a second), on a fake clock from here.
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    try {
      // Another status change with the same file content reads nothing.
      __seedGitStatusForTest(WORKSPACE, status({ aheadCount: 1 }));
      await vi.advanceTimersByTimeAsync(1_000);
      expect(m.list).toHaveBeenCalledTimes(2);
      await vi.advanceTimersByTimeAsync(0);
      expect(m.diff).toHaveBeenCalledTimes(1);

      files = [{ ...THREE[0], version: '100644:edited' }];
      __seedGitStatusForTest(WORKSPACE, status({ aheadCount: 2 }));
      await vi.advanceTimersByTimeAsync(1_000);
      expect(m.list).toHaveBeenCalledTimes(3);
      await vi.advanceTimersByTimeAsync(0);
      expect(m.diff).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('says how many files are not shown when the list is bounded', async () => {
    const m = await mount(listing(THREE, { total: 7000 }));
    await waitFor(() => expect(m.getByTestId('commit-dialog-truncated').textContent).toContain('first 3 of 7,000'));
  });
});

describe('<CommitDialog> — Generate commit message', () => {
  it('drafts subject and body for the selected files', async () => {
    const generate = setBindingMock('GenerateCommitMessage', async () => ({
      subject: 'Add login flow',
      body: 'Supports SSO.',
    }));
    const m = await mount();
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    await fireEvent.click(m.check('src/new.ts'));

    await fireEvent.click(m.getByTestId('commit-dialog-generate'));

    await waitFor(() => {
      const subject = document.getElementById('commit-subject') as HTMLInputElement;
      const body = document.getElementById('commit-body') as HTMLTextAreaElement;
      expect(subject.value).toBe('Add login flow');
      expect(body.value).toBe('Supports SSO.');
    });
    expect(generate).toHaveBeenCalledWith(m.pane.workspace, { all: false, files: [THREE[1]] });
  });

  it('keeps the drafted fields editable after a failed generation', async () => {
    setBindingMock('GenerateCommitMessage', async () => {
      throw new Error('no uncommitted changes to describe');
    });
    const m = await mount();
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    await fireEvent.click(m.check('src/a.ts'));

    await fireEvent.click(m.getByTestId('commit-dialog-generate'));

    await waitFor(() => {
      const button = m.getByTestId('commit-dialog-generate') as HTMLButtonElement;
      expect(button.disabled).toBe(false);
      expect(button.textContent?.trim()).toBe('Generate');
    });
    const subject = document.getElementById('commit-subject') as HTMLInputElement;
    expect(subject.value).toBe('');
  });
});

describe('<CommitDialog> — pending operation notice', () => {
  it.each([
    ['rebase', 'git rebase --continue'],
    ['bisect', 'git bisect reset'],
  ])('shows an informational notice for a pending %s and still commits a selection', async (op, text) => {
    const commit = setBindingMock('GitCommit', async () => ({
      action: 'commit',
      commitSha: 'abcdef0123456789',
    } as GitActionResult));
    const m = await mount(listing(THREE), status({ pendingOperation: op }));
    await waitFor(() => expect(m.rows()).toHaveLength(3));

    const notice = m.getByTestId('commit-dialog-pending-operation');
    expect(notice.textContent).toContain(text);
    expect(notice.getAttribute('role')).toBe('status');

    await fireEvent.click(m.check('src/a.ts'));
    await m.setSubject('Finish it');
    await fireEvent.click(m.commitButton());
    await waitFor(() => expect(commit).toHaveBeenCalledWith(m.pane.workspace, 'Finish it', '', { all: false, files: [THREE[0]] }, expect.any(String)));
  });

  it('commits everything, with the rows locked, while git records only whole commits', async () => {
    let files = THREE;
    const commit = setBindingMock('GitCommit', async () => {
      files = [];
      return { action: 'commit', commitSha: 'abcdef0123456789' } as GitActionResult;
    });
    const generate = setBindingMock('GenerateCommitMessage', async () => ({ subject: 'Merge', body: '' }));
    const m = await mount(() => listing(files, { wholeOnly: files.length > 0 }), status({ pendingOperation: 'merge' }));
    await waitFor(() => expect(m.rows()).toHaveLength(3));

    expect(m.getByTestId('commit-dialog-pending-operation').textContent).toContain('Committing completes the merge.');
    expect(m.getByTestId('commit-dialog-selection').textContent?.trim()).toBe('All 3 files · committed together');
    expect(m.queryByTestId('commit-dialog-toggle-all')).toBeNull();
    expect(m.rows().map((el) => el.getAttribute('aria-selected'))).toEqual(['true', 'true', 'true']);
    await fireEvent.click(m.check('src/a.ts'));
    await fireEvent.keyDown(m.getByTestId('commit-file-list'), { key: 'a' });
    await fireEvent.keyDown(m.getByTestId('commit-file-list'), { key: ' ' });
    expect(m.rows().map((el) => el.getAttribute('aria-selected'))).toEqual(['true', 'true', 'true']);

    await fireEvent.click(m.getByTestId('commit-dialog-generate'));
    await waitFor(() => expect(generate).toHaveBeenCalledWith(m.pane.workspace, { all: true, files: [] }));
    await fireEvent.click(m.commitButton());
    await waitFor(() => expect(m.onClose).toHaveBeenCalledTimes(1));
    expect(commit).toHaveBeenCalledWith(m.pane.workspace, 'Merge', '', { all: true, files: [] }, expect.any(String));
  });

  it('names a pending revert', async () => {
    const m = await mount(listing(THREE, { wholeOnly: true }), status({ pendingOperation: 'revert' }));
    expect(m.getByTestId('commit-dialog-pending-operation').textContent).toContain('A revert is in progress.');
  });

  it('shows why a commit of files that changed was refused and lists them again', async () => {
    let files = THREE;
    setBindingMock('GitCommit', async () => {
      files = [{ ...THREE[0], version: '100644:edited' }, THREE[1], THREE[2]];
      throw new Error('commit: the selected files changed since they were listed (src/a.ts); review them and commit again');
    });
    const m = await mount(() => listing(files));
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    await fireEvent.click(m.check('src/a.ts'));
    await m.setSubject('Stale');
    await waitFor(() => expect(m.diff).toHaveBeenCalledTimes(1));
    await fireEvent.click(m.commitButton());

    await waitFor(() => expect(m.getByText(/changed since they were listed \(src\/a\.ts\)/).getAttribute('role')).toBe('alert'));
    // The list and the preview are read again, and the selection and the message stay for the retry.
    await waitFor(() => expect(m.diff).toHaveBeenCalledTimes(2));
    expect(m.row('src/a.ts').getAttribute('aria-selected')).toBe('true');
    expect((document.getElementById('commit-subject') as HTMLInputElement).value).toBe('Stale');
    expect(m.onClose).not.toHaveBeenCalled();
  });

  it('names a pending cherry-pick', async () => {
    const m = await mount(listing(THREE, { wholeOnly: true }), status({ pendingOperation: 'cherry-pick' }));
    expect(m.getByTestId('commit-dialog-pending-operation').textContent).toContain('A cherry-pick is in progress.');
  });

  it('lists again on each opening, with the selection of the last one gone', async () => {
    const m = await mount();
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    await fireEvent.click(m.check('src/a.ts'));
    await m.setSubject('Draft kept');
    const calls = m.list.mock.calls.length;

    await m.rerender({ pane: m.pane, open: false, onClose: m.onClose });
    await m.rerender({ pane: m.pane, open: true, onClose: m.onClose });
    await waitFor(() => expect(m.list.mock.calls.length).toBe(calls + 1));
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    expect(m.rows().map((el) => el.getAttribute('aria-selected'))).toEqual(['false', 'false', 'false']);
    // The typed message is a draft and survives the close.
    expect((document.getElementById('commit-subject') as HTMLInputElement).value).toBe('Draft kept');
  });

  it('lists once on open, not once per effect', async () => {
    // On the fake clock from before the mount, so a second listing that an
    // effect asks for during it waits on this clock.
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    try {
      const m = await mount();
      await vi.advanceTimersByTimeAsync(0);
      expect(m.rows()).toHaveLength(3);
      // Past the scheduler's delay (250 ms), its longest wait (1 s) and its
      // spacing between listings (1 s): a second listing would have run.
      await vi.advanceTimersByTimeAsync(2_500);
      expect(m.list).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('renders no notice when no operation is pending', async () => {
    const m = await mount(listing(THREE), status({ pendingOperation: '' }));
    expect(m.queryByTestId('commit-dialog-pending-operation')).toBeNull();
  });

  it('surfaces git refusing the commit as the inline error and stays open', async () => {
    setBindingMock('GitCommit', async () => ({
      action: 'commit',
      error: 'Committing is not possible because you have unmerged files.',
    } as GitActionResult));
    const m = await mount(listing(THREE, { wholeOnly: true }), status({ pendingOperation: 'merge' }));
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    await m.setSubject('Finish it');
    await fireEvent.click(m.commitButton());

    await waitFor(() => expect(m.getByText(/unmerged files/).getAttribute('role')).toBe('alert'));
    expect(m.onClose).not.toHaveBeenCalled();
    expect((document.getElementById('commit-subject') as HTMLInputElement).value).toBe('Finish it');
  });

  it('shows a commit whose answer was lost as committed once the computer says it landed', async () => {
    let files = THREE;
    const commit = setBindingMock('GitCommit', async () => {
      // The hook ran past the wait; the commit lands on the computer anyway.
      files = [THREE[1], THREE[2]];
      throw new TransportError('timeout', 'RPC 7 timed out after 735000ms');
    });
    const outcome = setBindingMock('GetGitActionOutcome', async () => ({
      state: 'succeeded',
      result: { action: 'commit', commitSha: 'abcdef0123456789' },
    } as GitActionOutcome));
    const m = await mount(() => listing(files));
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    await fireEvent.click(m.check('src/a.ts'));
    await m.setSubject('Slow hook');
    await fireEvent.click(m.commitButton());

    await waitFor(() => expect(m.rows()).toHaveLength(2));
    const attemptId = commit.mock.calls[0]![4];
    expect(attemptId).toEqual(expect.any(String));
    expect(outcome).toHaveBeenCalledWith(m.pane.workspace, 'commit', attemptId);
    expect(m.queryByRole('alert')).toBeNull();
    expect((document.getElementById('commit-subject') as HTMLInputElement).value).toBe('');
  });

  it('asks again on the next press while a lost commit cannot be asked about, and never commits twice', async () => {
    const commit = setBindingMock('GitCommit', async () => {
      throw new DisconnectedError('socket closed');
    });
    let asks = 0;
    const outcome = setBindingMock('GetGitActionOutcome', async () => {
      asks += 1;
      if (asks === 1) throw new DisconnectedError('socket closed');
      return { state: 'failed', message: 'git commit: the pre-commit hook refused it' } as GitActionOutcome;
    });
    const m = await mount();
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    await fireEvent.click(m.check('src/a.ts'));
    await m.setSubject('Lost');
    await fireEvent.click(m.commitButton());

    await waitFor(() => expect(m.getByRole('alert').textContent).toContain(COMMIT_ANSWER_LOST));
    await waitFor(() => expect(m.commitButton().disabled).toBe(false));
    await fireEvent.click(m.commitButton());
    await waitFor(() => expect(m.getByRole('alert').textContent).toContain('pre-commit hook refused it'));
    expect(commit).toHaveBeenCalledTimes(1);
    expect(outcome).toHaveBeenCalledTimes(2);
    expect(outcome.mock.calls[1]![2]).toBe(commit.mock.calls[0]![4]);
  });

  it('says so when the computer has no record of a lost commit', async () => {
    setBindingMock('GitCommit', async () => {
      throw new DisconnectedError('socket closed');
    });
    setBindingMock('GetGitActionOutcome', async () => ({ state: 'unknown' } as GitActionOutcome));
    const m = await mount();
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    await fireEvent.click(m.check('src/a.ts'));
    await m.setSubject('Unknown');
    await fireEvent.click(m.commitButton());
    await waitFor(() => expect(m.getByRole('alert').textContent).toBe(COMMIT_OUTCOME_UNKNOWN));
    expect(m.row('src/a.ts').getAttribute('aria-selected')).toBe('true');
  });

  it('surfaces a thrown commit error inline and keeps the selection', async () => {
    setBindingMock('GitCommit', async () => {
      throw new Error('git commit exited with code 128');
    });
    const m = await mount();
    await waitFor(() => expect(m.rows()).toHaveLength(3));
    await fireEvent.click(m.check('src/a.ts'));
    await m.setSubject('Finish it');
    await fireEvent.click(m.commitButton());

    await waitFor(() => expect(m.getByText(/exited with code 128/).getAttribute('role')).toBe('alert'));
    expect(m.onClose).not.toHaveBeenCalled();
    expect(m.row('src/a.ts').getAttribute('aria-selected')).toBe('true');
  });
});
