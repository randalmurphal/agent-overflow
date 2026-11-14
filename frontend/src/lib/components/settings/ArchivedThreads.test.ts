import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { buildPane, makeThread } from '../../../test/helpers/chat';
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

  it('says how many terminals close and applies the rows the removal moved', async () => {
    const worktree = '/tmp/wt/archived';
    const archived = makeThread({ id: 'archived-on-wt', archived: true, projectId: 'project-1', workspacePath: worktree, worktreePath: worktree });
    const sibling = makeThread({ id: 'sibling-on-wt', workspacePath: worktree, worktreePath: worktree, branch: 'feature' });
    const siblingPane = await buildPane(sibling, [], 'pane-sibling');
    setBindingMock('ListArchivedThreads', async () => [archived]);
    setBindingMock('GitWorktreeStatus', async () => ({ path: worktree, terminals: 1 }));
    setBindingMock('StopSession', async () => {});
    const removed = setBindingMock('RemoveOtherWorktree', async () => ({
      workspace: { workspacePath: '/tmp/workspace', worktreePath: '', branch: 'main' },
      reattached: [{ ...sibling, workspacePath: '/tmp/workspace', worktreePath: '', branch: 'main' }],
    }));
    const deleted = setBindingMock('DeleteThread', async () => {});

    const { findByLabelText, findByText, getByRole } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await findByText(/This action cannot be undone\. 1 terminal will close\./);
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(deleted).toHaveBeenCalledWith('archived-on-wt'));
    expect(removed).toHaveBeenCalledWith({ projectId: 'project-1', workspacePath: '' }, worktree, false);
    expect(siblingPane.thread?.worktreePath).toBe('');
    expect(siblingPane.thread?.branch).toBe('main');
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
  it('removes the worktree on the thread\'s computer, then deletes the thread', async () => {
    const archived = makeThread({ id: 'archived-on-laptop', archived: true, projectId: 'project-archived', workspacePath: worktree, worktreePath: worktree });
    noteThread(archived.id, 'laptop');
    setBindingMock('ListArchivedThreads', async () => [archived]);
    const countTargets: Array<string | null> = [];
    setBindingMock('GitWorktreeStatus', async () => {
      countTargets.push(getPinnedBackend());
      return { path: worktree, terminals: 2 };
    });
    setBindingMock('StopSession', async () => {});
    const removalTargets: Array<string | null> = [];
    const removed = setBindingMock('RemoveOtherWorktree', async () => {
      removalTargets.push(getPinnedBackend());
      return { workspace: { workspacePath: '/tmp/repo', worktreePath: '', branch: 'main' }, reattached: [] };
    });
    const deleted = setBindingMock('DeleteThread', async () => {});

    const { findByLabelText, findByText, getByRole } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await findByText(/2 terminals will close\./);
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(deleted).toHaveBeenCalledWith('archived-on-laptop'));
    expect(removed).toHaveBeenCalledWith({ projectId: 'project-archived', workspacePath: '' }, worktree, false);
    expect(removalTargets).toEqual(['laptop']);
    expect(countTargets).toEqual(['laptop']);
  });

  it('removes and deletes nothing when the thread\'s computer is unknown', async () => {
    const archived = makeThread({ id: 'archived-unowned', archived: true, projectId: 'project-archived', workspacePath: worktree, worktreePath: worktree });
    setBindingMock('ListArchivedThreads', async () => [archived]);
    setBindingMock('GitWorktreeStatus', async () => ({ path: worktree, terminals: 0 }));
    const stopped = setBindingMock('StopSession', async () => {});
    const removed = setBindingMock('RemoveOtherWorktree', async () => ({ workspace: null, reattached: [] }));
    const deleted = setBindingMock('DeleteThread', async () => {});

    const { findByLabelText, getByRole } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(getToasts().some((t) => /computer that owns this item is unknown/.test(t.message))).toBe(true));
    expect(stopped).not.toHaveBeenCalled();
    expect(removed).not.toHaveBeenCalled();
    expect(deleted).not.toHaveBeenCalled();
  });

  it('keeps the thread when its computer refuses the removal', async () => {
    const archived = makeThread({ id: 'archived-offline', archived: true, projectId: 'project-archived', workspacePath: worktree, worktreePath: worktree });
    noteThread(archived.id, 'laptop');
    setBindingMock('ListArchivedThreads', async () => [archived]);
    setBindingMock('GitWorktreeStatus', async () => ({ path: worktree, terminals: 0 }));
    setBindingMock('StopSession', async () => {});
    setBindingMock('RemoveOtherWorktree', async () => { throw new Error('client closed'); });
    const deleted = setBindingMock('DeleteThread', async () => {});

    const { findByLabelText, getByRole, findByText } = render(ArchivedThreads);
    await fireEvent.click(await findByLabelText('Delete permanently'));
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(getToasts().some((t) => /client closed/.test(t.message))).toBe(true));
    expect(deleted).not.toHaveBeenCalled();
    expect(await findByText(archived.title)).toBeTruthy();
  });
});
