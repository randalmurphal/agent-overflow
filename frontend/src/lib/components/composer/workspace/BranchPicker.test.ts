import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import BranchPicker from './BranchPicker.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Thread } from '../../../types/models';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';
import {
  resetForTest as resetWorktreeIntent,
  setThreadEnvMode,
  worktreeIntentForThread,
} from '../../../stores/worktreeIntent.svelte';
import type { WorkspaceChangeLockState } from '../../../stores/workspaceChangeLock.svelte';

function makeThread(branch: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/repo',
    projectPath: '/repo',
    mode: 'chat',
    model: 'm',
    branch,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(branch: string, overrides: Partial<Thread> = {}) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(makeThread(branch, overrides));
  return pane;
}

function makeWorkspaceLock(overrides: Partial<WorkspaceChangeLockState> = {}): WorkspaceChangeLockState {
  return {
    locked: false,
    reason: '',
    runningBackgroundCount: 0,
    refresh: async () => {},
    ...overrides,
  };
}

describe('<BranchPicker>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
  });

  it('renders the current branch on the trigger', async () => {
    const pane = await buildPane('main');
    const { getByTestId } = render(BranchPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    expect(getByTestId('branch-picker-trigger').textContent ?? '').toMatch(/main/);
  });

  it('opens the dropdown and lists fetched branches', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
    ]);
    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });
    expect(row).toBeTruthy();
  });

  it('calls GitCheckout and refreshes the thread on selection', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GitCheckout', async () => {});
    setBindingMock('GetThread', async () => makeThread('feat/abc'));

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });
    await fireEvent.click(row);
    await Promise.resolve();
    await Promise.resolve();

    await waitFor(() => {
      expect(getBindingMock('GitCheckout')!.mock.calls[0]).toEqual(['thread-1', 'feat/abc']);
      expect(getBindingMock('GetThread')!.mock.calls[0]).toEqual(['thread-1']);
    });
  });

  it('switches to an existing worktree instead of checking out its branch', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'feat/worktree', isCurrent: false, isDefault: false, worktreePath: '/tmp/wt' },
    ]);
    setBindingMock('UpdateThreadWorkspace', async () => makeThread('feat/worktree'));

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/worktree/ });
    await fireEvent.click(row);

    await waitFor(() => {
      expect(getBindingMock('UpdateThreadWorkspace')!.mock.calls[0]).toEqual(['thread-1', '/tmp/wt']);
      expect(getBindingMock('GitCheckout')).toBeUndefined();
    });
  });

  it('returns to the project checkout when selecting the default branch from a worktree', async () => {
    const pane = await buildPane('feature', {
      workspacePath: '/repo-worktrees/feature',
      worktreePath: '/repo-worktrees/feature',
      projectPath: '/repo',
    });
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: false, isDefault: true },
      { name: 'feature', isCurrent: true, isDefault: false },
    ]);
    setBindingMock('GitCheckout', async () => {});
    setBindingMock('GetThread', async () => makeThread('main', {
      workspacePath: '/repo',
      worktreePath: undefined,
      projectPath: '/repo',
    }));

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /main/ });
    await fireEvent.click(row);

    await waitFor(() => {
      expect(getBindingMock('GitCheckout')!.mock.calls[0]).toEqual(['thread-1', 'main']);
      expect(getBindingMock('UpdateThreadWorkspace')).toBeUndefined();
    });
  });

  it('filters branches from the search input', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feature/searchable', isCurrent: false, isDefault: false },
    ]);

    const { getByTestId, getByPlaceholderText, queryByRole, findByRole } = render(BranchPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feature\/searchable/ });

    await fireEvent.input(getByPlaceholderText('Search Branches'), { target: { value: 'searchable' } });

    expect(queryByRole('menuitem', { name: /^main/ })).toBeNull();
    expect(await findByRole('menuitem', { name: /feature\/searchable/ })).toBeTruthy();
  });

  it('sets the base branch when new worktree mode is pending', async () => {
    const pane = await buildPane('main');
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'release', isCurrent: false, isDefault: false },
    ]);

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    expect(getByTestId('branch-picker-trigger').textContent ?? '').toMatch(/From main/);
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /release/ });
    await fireEvent.click(row);

    expect(worktreeIntentForThread(pane.thread).baseBranch).toBe('release');
    expect(getBindingMock('GitCheckout')).toBeUndefined();
  });

  it('disables branch checkout while the agent is responding', async () => {
    const pane = await buildPane('main');
    const workspaceLock = makeWorkspaceLock({
      locked: true,
      reason: 'Workspace changes are unavailable while the agent is responding.',
    });
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GitCheckout', async () => {});

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane, workspaceLock } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });
    expect(row).toHaveAttribute('aria-disabled', 'true');
    expect(row).toHaveAttribute('title', expect.stringMatching(/agent is responding/));

    await fireEvent.click(row);

    expect(getBindingMock('GitCheckout')).not.toHaveBeenCalled();
  });

  it('exposes a New branch row that opens the inline create form', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
    ]);
    setBindingMock('GetGitStatus', async () => ({
      isRepo: true,
      branch: 'main',
      isDefaultBranch: true,
      hasChanges: false,
      insertions: 0,
      deletions: 0,
      fileCount: 0,
      hasUpstream: true,
      aheadCount: 0,
      behindCount: 0,
      hasOriginRemote: true,
    }));

    const { getByTestId, findByRole, queryByTestId } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));

    const newRow = await findByRole('menuitem', { name: /New branch/ });
    await fireEvent.click(newRow);

    await waitFor(() => {
      expect(queryByTestId('branch-picker-create-form')).not.toBeNull();
    });
  });

  it('creates a branch via GitCreateBranchFrom from a chosen base', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'release', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GetGitStatus', async () => ({
      isRepo: true,
      branch: 'main',
      isDefaultBranch: true,
      hasChanges: false,
      insertions: 0,
      deletions: 0,
      fileCount: 0,
      hasUpstream: true,
      aheadCount: 0,
      behindCount: 0,
      hasOriginRemote: true,
    }));
    setBindingMock('GitCreateBranchFrom', async () => makeThread('feat/new'));

    const { getByTestId, findByRole, findByPlaceholderText, findByTestId } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const newRow = await findByRole('menuitem', { name: /New branch/ });
    await fireEvent.click(newRow);

    const nameInput = await findByPlaceholderText('New branch name');
    await fireEvent.input(nameInput, { target: { value: 'feat/new' } });

    const releaseRow = await findByRole('menuitem', { name: /release/ });
    await fireEvent.click(releaseRow);

    const submit = await findByTestId('branch-picker-create-submit');
    await fireEvent.click(submit);

    await waitFor(() => {
      expect(getBindingMock('GitCreateBranchFrom')!.mock.calls[0]).toEqual([
        'thread-1',
        'feat/new',
        'release',
        false,
      ]);
    });
  });

  it('passes carryLocalChanges=true when Local (with changes) is the create base', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
    ]);
    setBindingMock('GetGitStatus', async () => ({
      isRepo: true,
      branch: 'main',
      isDefaultBranch: true,
      hasChanges: true,
      insertions: 1,
      deletions: 0,
      fileCount: 1,
      hasUpstream: true,
      aheadCount: 0,
      behindCount: 0,
      hasOriginRemote: true,
    }));
    setBindingMock('GitCreateBranchFrom', async () => makeThread('feat/dirty'));

    const { getByTestId, findByRole, findByPlaceholderText, findByTestId } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const newRow = await findByRole('menuitem', { name: /New branch/ });
    await fireEvent.click(newRow);

    const nameInput = await findByPlaceholderText('New branch name');
    await fireEvent.input(nameInput, { target: { value: 'feat/dirty' } });

    const submit = await findByTestId('branch-picker-create-submit');
    await fireEvent.click(submit);

    await waitFor(() => {
      expect(getBindingMock('GitCreateBranchFrom')!.mock.calls[0]).toEqual([
        'thread-1',
        'feat/dirty',
        'main',
        true,
      ]);
    });
  });

  it('warns and uses the danger button when create base differs from current on a dirty workspace', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'release', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GetGitStatus', async () => ({
      isRepo: true,
      branch: 'main',
      isDefaultBranch: true,
      hasChanges: true,
      insertions: 5,
      deletions: 0,
      fileCount: 2,
      hasUpstream: true,
      aheadCount: 0,
      behindCount: 0,
      hasOriginRemote: true,
    }));

    const { getByTestId, findByRole, findByPlaceholderText, findByTestId, queryByTestId } = render(
      BranchPicker,
      { props: { pane, workspaceLock: makeWorkspaceLock() } },
    );
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const newRow = await findByRole('menuitem', { name: /New branch/ });
    await fireEvent.click(newRow);

    const nameInput = await findByPlaceholderText('New branch name');
    await fireEvent.input(nameInput, { target: { value: 'feat/destructive' } });

    expect(queryByTestId('branch-picker-create-discards')).toBeNull();

    const releaseRow = await findByRole('menuitem', { name: /release/ });
    await fireEvent.click(releaseRow);

    await waitFor(() => {
      expect(queryByTestId('branch-picker-create-discards')).not.toBeNull();
    });
    const submit = await findByTestId('branch-picker-create-submit');
    expect(submit.textContent ?? '').toMatch(/Discard and create/);
  });

  it('flips intent to Local sentinel when picking the Local row in worktree mode', async () => {
    const pane = await buildPane('main');
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
    ]);
    setBindingMock('GetGitStatus', async () => ({
      isRepo: true,
      branch: 'main',
      isDefaultBranch: true,
      hasChanges: true,
      insertions: 1,
      deletions: 0,
      fileCount: 1,
      hasUpstream: true,
      aheadCount: 0,
      behindCount: 0,
      hasOriginRemote: true,
    }));

    const { getByTestId, findByRole } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const localRow = await findByRole('menuitem', { name: /Local \(with changes\)/ });
    await fireEvent.click(localRow);

    expect(worktreeIntentForThread(pane.thread).baseBranch).toBe('__LOCAL__');
  });

  it('renders ahead/behind arrows next to branches with upstream diffs', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true, aheadCount: 3, behindCount: 2 },
      { name: 'feat/ahead-only', isCurrent: false, isDefault: false, aheadCount: 1 },
      { name: 'feat/behind-only', isCurrent: false, isDefault: false, behindCount: 4 },
      { name: 'feat/clean', isCurrent: false, isDefault: false },
    ]);

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));

    const mainRow = await findByRole('menuitem', { name: /main/ });
    // Assert both arrows and tag exist somewhere on the row; don't
    // couple to suffix ordering (that's MenuItem's contract, not ours).
    expect(mainRow.textContent ?? '').toMatch(/↑3/);
    expect(mainRow.textContent ?? '').toMatch(/↓2/);
    expect(mainRow.textContent ?? '').toMatch(/default/);

    const aheadRow = await findByRole('menuitem', { name: /feat\/ahead-only/ });
    expect(aheadRow.textContent ?? '').toMatch(/↑1/);
    expect(aheadRow.textContent ?? '').not.toMatch(/↓/);

    const behindRow = await findByRole('menuitem', { name: /feat\/behind-only/ });
    expect(behindRow.textContent ?? '').toMatch(/↓4/);
    expect(behindRow.textContent ?? '').not.toMatch(/↑/);

    const cleanRow = await findByRole('menuitem', { name: /feat\/clean/ });
    expect(cleanRow.textContent ?? '').not.toMatch(/[↑↓]/);
  });

  it('truncates long branch labels and surfaces the full name as the row tooltip', async () => {
    const pane = await buildPane('main');
    const longName = 'feature/this-is-a-very-long-branch-name';
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: longName, isCurrent: false, isDefault: false },
      { name: 'short', isCurrent: false, isDefault: false },
    ]);

    const { getByTestId, findByTitle, findByRole } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));

    // Hover tooltip carries the full branch name.
    const longRow = await findByTitle(longName);
    expect(longRow.getAttribute('role')).toBe('menuitem');
    // Visible label is right-truncated with an ellipsis and never exceeds
    // the configured ceiling (20 chars including the …).
    expect(longRow.textContent ?? '').toContain(`${longName.slice(0, 19)}…`);
    expect(longRow.textContent ?? '').not.toContain(longName);

    // Short names render unchanged and skip the tooltip — nothing to expand.
    const shortRow = await findByRole('menuitem', { name: /short/ });
    expect(shortRow.getAttribute('title')).toBeNull();
    expect(shortRow.textContent ?? '').toContain('short');
  });

  it('fires background fetch on open and refreshes branches when it actually fetched', async () => {
    const pane = await buildPane('main');
    let listCallCount = 0;
    setBindingMock('GitListBranches', async () => {
      listCallCount += 1;
      if (listCallCount === 1) {
        return [{ name: 'main', isCurrent: true, isDefault: true, aheadCount: 1 }];
      }
      return [{ name: 'main', isCurrent: true, isDefault: true, aheadCount: 5 }];
    });
    setBindingMock('GitMaybeFetchRemotes', async () => true);

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));

    await waitFor(async () => {
      const row = await findByRole('menuitem', { name: /main/ });
      expect(row.textContent ?? '').toMatch(/↑5/);
    });
    expect(getBindingMock('GitMaybeFetchRemotes')!.mock.calls[0]).toEqual(['thread-1']);
    expect(listCallCount).toBe(2);
  });

  it('skips refresh when background fetch reports no work', async () => {
    const pane = await buildPane('main');
    let listCallCount = 0;
    setBindingMock('GitListBranches', async () => {
      listCallCount += 1;
      return [{ name: 'main', isCurrent: true, isDefault: true }];
    });
    setBindingMock('GitMaybeFetchRemotes', async () => false);

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /main/ });

    await waitFor(() => {
      expect(getBindingMock('GitMaybeFetchRemotes')!).toHaveBeenCalled();
    });
    expect(listCallCount).toBe(1);
  });

  it('drops background fetch refresh after the picker closes mid-flight', async () => {
    const pane = await buildPane('main');
    let resolveFetch: ((v: boolean) => void) | undefined;
    const fetchPromise = new Promise<boolean>((r) => (resolveFetch = r));
    let listCallCount = 0;
    setBindingMock('GitListBranches', async () => {
      listCallCount += 1;
      return [{ name: 'main', isCurrent: true, isDefault: true, aheadCount: 1 }];
    });
    setBindingMock('GitMaybeFetchRemotes', () => fetchPromise);

    const { getByTestId, findByRole, queryByRole } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /main/ });

    // Close before the background fetch resolves — simulates user
    // dismissing the picker while the fetch is in flight.
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    expect(queryByRole('menuitem', { name: /main/ })).toBeNull();

    // Resolve the fetch with a "fetched" result; the post-fetch
    // refresh path must NOT call GitListBranches because the picker
    // is closed.
    resolveFetch!(true);
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();

    expect(listCallCount).toBe(1);
  });

  it('shows an error toast when prune fails and leaves the row clickable again', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
    ]);
    setBindingMock('GitPruneRemotes', async () => {
      throw new Error('network unreachable');
    });

    const { getByTestId, findByRole } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /main/ });

    const pruneRow = await findByRole('menuitem', { name: /Prune stale branches/ });
    await fireEvent.click(pruneRow);

    await waitFor(() => {
      expect(getBindingMock('GitPruneRemotes')!).toHaveBeenCalled();
    });
    // After the failure the row label resets ("Pruning…" → "Prune
    // stale branches") and is no longer disabled, so a retry is
    // possible.
    const retryRow = await findByRole('menuitem', { name: /Prune stale branches/ });
    expect(retryRow).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('runs prune on demand and replaces the branch list', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'stale/branch', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GitPruneRemotes', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
    ]);

    const { getByTestId, findByRole, queryByRole } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /stale\/branch/ });

    const pruneRow = await findByRole('menuitem', { name: /Prune stale branches/ });
    await fireEvent.click(pruneRow);

    await waitFor(() => {
      expect(getBindingMock('GitPruneRemotes')!.mock.calls[0]).toEqual(['thread-1']);
      expect(queryByRole('menuitem', { name: /stale\/branch/ })).toBeNull();
    });
  });

  it('exposes an enabled sync action on a branch that is purely behind upstream', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/behind', isCurrent: false, isDefault: false, behindCount: 3 },
    ]);

    const { getByTestId, findByRole, findByLabelText } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feat\/behind/ });

    const syncBtn = await findByLabelText(/Sync feat\/behind from upstream/);
    expect(syncBtn).not.toHaveAttribute('aria-disabled', 'true');
    expect(syncBtn).not.toHaveAttribute('disabled');
  });

  it('renders a disabled sync action with tooltip on a diverged branch', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      {
        name: 'feat/diverged',
        isCurrent: false,
        isDefault: false,
        aheadCount: 2,
        behindCount: 3,
      },
    ]);

    const { getByTestId, findByRole, findByLabelText } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feat\/diverged/ });

    const syncBtn = await findByLabelText(/Sync feat\/diverged from upstream/);
    expect(syncBtn).toHaveAttribute('aria-disabled', 'true');
    expect(syncBtn.getAttribute('title') ?? '').toMatch(/diverged/i);
    expect(syncBtn.getAttribute('title') ?? '').toMatch(/2 ahead/);
    expect(syncBtn.getAttribute('title') ?? '').toMatch(/3 behind/);
  });

  it('omits the sync action when the branch is up to date or only ahead', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/clean', isCurrent: false, isDefault: false },
      { name: 'feat/ahead-only', isCurrent: false, isDefault: false, aheadCount: 2 },
    ]);

    const { getByTestId, findByRole, queryByLabelText } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feat\/clean/ });

    expect(queryByLabelText(/Sync feat\/clean from upstream/)).toBeNull();
    expect(queryByLabelText(/Sync feat\/ahead-only from upstream/)).toBeNull();
  });

  it('runs sync, refreshes the branch list, and does not trigger checkout', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/behind', isCurrent: false, isDefault: false, behindCount: 3 },
    ]);
    setBindingMock('GitSyncBranch', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/behind', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GitCheckout', async () => {});

    const { getByTestId, findByRole, findByLabelText, queryByLabelText } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feat\/behind/ });

    const syncBtn = await findByLabelText(/Sync feat\/behind from upstream/);
    await fireEvent.click(syncBtn);

    await waitFor(() => {
      expect(getBindingMock('GitSyncBranch')!.mock.calls[0]).toEqual(['thread-1', 'feat/behind']);
    });
    expect(getBindingMock('GitCheckout')).not.toHaveBeenCalled();
    // After sync the row no longer carries behind > 0, so the action
    // disappears.
    await waitFor(() => {
      expect(queryByLabelText(/Sync feat\/behind from upstream/)).toBeNull();
    });
  });

  it('surfaces a toast on sync failure and keeps the row clickable', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/behind', isCurrent: false, isDefault: false, behindCount: 1 },
    ]);
    setBindingMock('GitSyncBranch', async () => {
      throw new Error('non-fast-forward');
    });

    const { getByTestId, findByRole, findByLabelText } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feat\/behind/ });

    const syncBtn = await findByLabelText(/Sync feat\/behind from upstream/);
    await fireEvent.click(syncBtn);

    await waitFor(() => {
      expect(getBindingMock('GitSyncBranch')!).toHaveBeenCalled();
    });

    // Button is back to enabled after the failure so the user can retry.
    const retry = await findByLabelText(/Sync feat\/behind from upstream/);
    expect(retry).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('does not invoke GitSyncBranch when the diverged sync action is clicked', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      {
        name: 'feat/diverged',
        isCurrent: false,
        isDefault: false,
        aheadCount: 1,
        behindCount: 1,
      },
    ]);
    setBindingMock('GitSyncBranch', async () => []);

    const { getByTestId, findByRole, findByLabelText } = render(BranchPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feat\/diverged/ });

    const syncBtn = await findByLabelText(/Sync feat\/diverged from upstream/);
    await fireEvent.click(syncBtn);

    expect(getBindingMock('GitSyncBranch')).not.toHaveBeenCalled();
  });

  it('disables branch checkout while background tasks are running', async () => {
    const pane = await buildPane('main');
    const workspaceLock = makeWorkspaceLock({
      locked: true,
      reason: 'Workspace changes are unavailable while background tasks are running.',
      runningBackgroundCount: 1,
    });
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GitCheckout', async () => {});

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane, workspaceLock } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });

    await waitFor(() => {
      expect(row).toHaveAttribute('aria-disabled', 'true');
      expect(row).toHaveAttribute('title', expect.stringMatching(/background tasks/));
    });

    await fireEvent.click(row);

    expect(getBindingMock('GitCheckout')).not.toHaveBeenCalled();
  });
});
