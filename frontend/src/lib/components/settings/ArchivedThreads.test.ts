import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';

import { buildPane, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetPanesForTest } from '../../stores/panes.svelte';
import ArchivedThreads from './ArchivedThreads.svelte';

describe('<ArchivedThreads>', () => {
  it('renders friendly model aliases', async () => {
    setBindingMock('ListArchivedThreads', async () => [
      makeThread({
        id: 'codex-archived',
        archived: true,
        provider: 'codex',
        model: 'gpt-5.6-sol',
      }),
    ]);

    const { findByText } = render(ArchivedThreads);
    expect(await findByText(/codex · GPT 5\.6 Sol/)).toBeTruthy();
  });
});

describe('<ArchivedThreads> delete of a thread on a worktree', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
  });

  it('says how many terminals close and applies the rows the removal moved', async () => {
    const worktree = '/tmp/wt/archived';
    const archived = makeThread({ id: 'archived-on-wt', archived: true, workspacePath: worktree, worktreePath: worktree });
    const sibling = makeThread({ id: 'sibling-on-wt', workspacePath: worktree, worktreePath: worktree, branch: 'feature' });
    const siblingPane = await buildPane(sibling, [], 'pane-sibling');
    setBindingMock('ListArchivedThreads', async () => [archived]);
    setBindingMock('GitWorktreeStatus', async () => ({ path: worktree, terminals: 1 }));
    setBindingMock('StopSession', async () => {});
    setBindingMock('GitRemoveWorktree', async () => ({
      workspace: { workspacePath: '/tmp/workspace', worktreePath: '', branch: 'main' },
      reattached: [{ ...sibling, workspacePath: '/tmp/workspace', worktreePath: '', branch: 'main' }],
    }));
    const deleted = setBindingMock('DeleteThread', async () => {});

    const { findByLabelText, findByText, getByRole } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await findByText(/This action cannot be undone\. 1 terminal will close\./);
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(deleted).toHaveBeenCalledWith('archived-on-wt'));
    expect(siblingPane.thread?.worktreePath).toBe('');
    expect(siblingPane.thread?.branch).toBe('main');
  });
});
