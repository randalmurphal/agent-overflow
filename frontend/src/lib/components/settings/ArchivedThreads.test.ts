import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { getToasts, resetToastsForTest } from '../../stores/toast.svelte';
import { getPinnedBackend } from '../../transport/backends';
import { __resetEntityIndexForTest, noteThread } from '../../transport/entityIndex';
import { forgetGrantedScopes, setCarriedSessionScopes } from '../../transport/scopes';
import ArchivedThreads from './ArchivedThreads.svelte';
import { applySettingsSnapshot } from '../../stores/settings.svelte';
import { makeSettings } from '../../../test/helpers/settings';

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

  // While assistants are off, an archived assistant and the threads under
  // it are not listed; they are when assistants are on again.
  it('lists no thread under an assistant while assistants are off', async () => {
    setBindingMock('ListArchivedThreads', async () => [
      makeThread({ id: 'plain', title: 'Plain', archived: true }),
      makeThread({ id: 'agent', title: 'Ops', archived: true, agentId: 'ops' }),
      makeThread({ id: 'past', title: 'Earlier Ops', archived: true, pastAgentId: 'ops' }),
      makeThread({ id: 'helper', title: 'Research', archived: true, helperOfAgentId: 'ops' }),
    ]);
    applySettingsSnapshot(makeSettings({ assistantsEnabled: false }));
    const view = render(ArchivedThreads);
    expect(await view.findByText('Plain')).toBeTruthy();
    expect(view.getByText('1 archived thread')).toBeTruthy();
    for (const title of ['Ops', 'Earlier Ops', 'Research']) expect(view.queryByText(title)).toBeNull();
    applySettingsSnapshot(makeSettings({ assistantsEnabled: true }));
    expect(await view.findByText('4 archived threads')).toBeTruthy();
  });
});

describe('<ArchivedThreads> delete of a thread on a worktree', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
  });

  // The confirmation names what goes with the thread and says nothing of
  // its worktree, which the delete keeps; no worktree is read or removed.
  it('says nothing of the worktree and removes none', async () => {
    const worktree = '/tmp/wt/archived';
    const archived = makeThread({ id: 'archived-on-wt', archived: true, projectId: 'project-1', workspacePath: worktree, worktreePath: worktree });
    setBindingMock('ListArchivedThreads', async () => [archived]);
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 1 }));
    const status = setBindingMock('GitWorktreeStatus', vi.fn(async () => ({ terminals: 3 })));
    const removed = setBindingMock('RemoveOtherWorktree', vi.fn(async () => ({ workspace: null, reattached: [] })));
    const deleted = setBindingMock('DeleteThreads', async () => ({}));

    const { findByLabelText, findByText, getByRole } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    expect((await findByText(/This action cannot be undone\./)).textContent).toBe(
      'This will permanently delete this thread and all its messages. This action cannot be undone. Its worker is deleted with it.',
    );
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(deleted).toHaveBeenCalledWith(['archived-on-wt']));
    expect(status).not.toHaveBeenCalled();
    expect(removed).not.toHaveBeenCalled();
  });
});

describe('<ArchivedThreads> delete of a thread with workers', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetToastsForTest();
  });

  it('names a thread under it that was not deleted', async () => {
    const archived = makeThread({ id: 'archived-owner', title: 'Owner', archived: true, projectId: 'project-1' });
    setBindingMock('ListArchivedThreads', async () => [archived]);
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 2 }));
    const deleted = setBindingMock('DeleteThreads', async () => ({
      workersNotDeleted: [{ threadId: 'worker-2', title: 'Parser review', error: 'the remote computer did not answer' }],
    }));

    const { findByLabelText, findByText, getByRole } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await findByText(/This action cannot be undone\. Its 2 workers are deleted with it\./);
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(deleted).toHaveBeenCalledWith(['archived-owner']));
    await waitFor(() => expect(getToasts().find((toast) => toast.type === 'warning')?.message).toBe(
      '"Owner" is not fully deleted: the thread "Parser review" under it was not deleted: The remote computer did not answer.',
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
  it('reads what goes with it and deletes on the thread\'s computer', async () => {
    const archived = makeThread({ id: 'archived-on-laptop', archived: true, projectId: 'project-archived', workspacePath: worktree, worktreePath: worktree });
    noteThread(archived.id, 'laptop');
    setBindingMock('ListArchivedThreads', async () => [archived]);
    const countTargets: Array<string | null> = [];
    setBindingMock('GetDeleteDependents', async () => {
      countTargets.push(getPinnedBackend());
      return { pastThreads: 0, helpers: 0, workers: 1 };
    });
    const deleteTargets: Array<string | null> = [];
    const deleted = setBindingMock('DeleteThreads', async () => {
      deleteTargets.push(getPinnedBackend());
      return {};
    });

    const { findByLabelText, findByText, getByRole } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await findByText(/Its worker is deleted with it\./);
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(deleted).toHaveBeenCalledWith(['archived-on-laptop']));
    expect(deleteTargets).toEqual(['laptop']);
    expect(countTargets.every((target) => target === 'laptop')).toBe(true);
  });

  it('deletes nothing when the thread\'s computer is unknown', async () => {
    const archived = makeThread({ id: 'archived-unowned', archived: true, projectId: 'project-archived', workspacePath: worktree, worktreePath: worktree });
    setBindingMock('ListArchivedThreads', async () => [archived]);
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0 }));
    const deleted = setBindingMock('DeleteThreads', async () => ({}));

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
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0 }));
    setBindingMock('DeleteThreads', async () => { throw new Error('client closed'); });

    const { findByLabelText, getByRole, findByText } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(getToasts().some((t) => /client closed/.test(t.message))).toBe(true));
    expect(await findByText(archived.title)).toBeTruthy();
  });
});
