import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { getToasts, resetToastsForTest } from '../../stores/toast.svelte';
import { getPinnedBackend } from '../../transport/backends';
import { __resetEntityIndexForTest, noteThread } from '../../transport/entityIndex';
import { forgetGrantedScopes, setCarriedSessionScopes } from '../../transport/scopes';
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

  // The confirmation counts the terminals in every worktree the delete
  // empties, as the backend reads them, and the backend removes them.
  it('says how many terminals close and deletes with the worktrees', async () => {
    const worktree = '/tmp/wt/archived';
    const archived = makeThread({ id: 'archived-on-wt', archived: true, projectId: 'project-1', workspacePath: worktree, worktreePath: worktree });
    setBindingMock('ListArchivedThreads', async () => [archived]);
    setBindingMock('GetDeleteDependents', async () => ({
      pastThreads: 0, helpers: 0, workers: 1,
      worktrees: [
        { projectId: 'project-1', worktreePath: worktree, terminals: 1 },
        { projectId: 'project-1', worktreePath: '/tmp/wt/worker', terminals: 2 },
      ],
    }));
    const deleted = setBindingMock('DeleteThreadAndWorktrees', async () => ({}));

    const { findByLabelText, findByText, getByRole } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await findByText(/This action cannot be undone\. 3 terminals will close\. Its worker is deleted with it\./);
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(deleted).toHaveBeenCalledWith('archived-on-wt'));
  });
});

describe('<ArchivedThreads> delete of a thread with workers', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetToastsForTest();
  });

  it('names a worker that was not deleted', async () => {
    const archived = makeThread({ id: 'archived-owner', title: 'Owner', archived: true, projectId: 'project-1' });
    setBindingMock('ListArchivedThreads', async () => [archived]);
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 2, worktrees: [] }));
    const deleted = setBindingMock('DeleteThreadAndWorktrees', async () => ({
      workersNotDeleted: [{ threadId: 'worker-2', title: 'Parser review', error: 'worktree /tmp/wt/parser has unsaved work' }],
    }));

    const { findByLabelText, findByText, getByRole } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await findByText(/This action cannot be undone\. Its 2 workers are deleted with it\./);
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(deleted).toHaveBeenCalledWith('archived-owner'));
    await waitFor(() => expect(getToasts().find((toast) => toast.type === 'warning')?.message).toBe(
      'Deleted "Owner", but its worker "Parser review" was not deleted: Worktree /tmp/wt/parser has unsaved work.',
    ));
  });
});

describe('<ArchivedThreads> delete with two computers attached', () => {
  const worktree = '/tmp/wt/laptop';

  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetToastsForTest();
    stageBackend();
    setCarriedSessionScopes('laptop', ['threads:operate', 'git:operate']);
  });

  afterEach(() => {
    forgetGrantedScopes('laptop');
    resetStagedBackends();
    __resetEntityIndexForTest();
  });

  // The project is archived too, so the project index never learns its
  // computer; only the archived thread row says where it lives.
  it('counts and deletes on the thread\'s computer', async () => {
    const archived = makeThread({ id: 'archived-on-laptop', archived: true, projectId: 'project-archived', workspacePath: worktree, worktreePath: worktree });
    noteThread(archived.id, 'laptop');
    setBindingMock('ListArchivedThreads', async () => [archived]);
    const countTargets: Array<string | null> = [];
    setBindingMock('GetDeleteDependents', async () => {
      countTargets.push(getPinnedBackend());
      return { pastThreads: 0, helpers: 0, workers: 0, worktrees: [{ projectId: 'project-archived', worktreePath: worktree, terminals: 2 }] };
    });
    const deleteTargets: Array<string | null> = [];
    const deleted = setBindingMock('DeleteThreadAndWorktrees', async () => {
      deleteTargets.push(getPinnedBackend());
      return {};
    });

    const { findByLabelText, findByText, getByRole } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await findByText(/2 terminals will close\./);
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(deleted).toHaveBeenCalledWith('archived-on-laptop'));
    expect(deleteTargets).toEqual(['laptop']);
    expect(countTargets.every((target) => target === 'laptop')).toBe(true);
  });

  it('deletes nothing when the thread\'s computer is unknown', async () => {
    const archived = makeThread({ id: 'archived-unowned', archived: true, projectId: 'project-archived', workspacePath: worktree, worktreePath: worktree });
    setBindingMock('ListArchivedThreads', async () => [archived]);
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0, worktrees: [] }));
    const deleted = setBindingMock('DeleteThreadAndWorktrees', async () => ({}));

    const { findByLabelText, getByRole } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(getToasts().some((t) => /computer that owns this item is unknown/.test(t.message))).toBe(true));
    expect(deleted).not.toHaveBeenCalled();
  });

  it('keeps the thread when its computer refuses the delete', async () => {
    const archived = makeThread({ id: 'archived-offline', archived: true, projectId: 'project-archived', workspacePath: worktree, worktreePath: worktree });
    noteThread(archived.id, 'laptop');
    setBindingMock('ListArchivedThreads', async () => [archived]);
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0, worktrees: [] }));
    setBindingMock('DeleteThreadAndWorktrees', async () => { throw new Error('client closed'); });

    const { findByLabelText, getByRole, findByText } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(getToasts().some((t) => /client closed/.test(t.message))).toBe(true));
    expect(await findByText(archived.title)).toBeTruthy();
  });
});
