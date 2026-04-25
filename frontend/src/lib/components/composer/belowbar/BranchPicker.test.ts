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
      { name: 'main', isRemote: false, isCurrent: true, isDefault: true },
      { name: 'feat/abc', isRemote: false, isCurrent: false, isDefault: false },
    ]);
    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });
    expect(row).toBeTruthy();
  });

  it('calls GitCheckout and refreshes the thread on selection', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isRemote: false, isCurrent: true, isDefault: true },
      { name: 'feat/abc', isRemote: false, isCurrent: false, isDefault: false },
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
      { name: 'feat/worktree', isRemote: false, isCurrent: false, isDefault: false, worktreePath: '/tmp/wt' },
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
      { name: 'main', isRemote: false, isCurrent: false, isDefault: true },
      { name: 'feature', isRemote: false, isCurrent: true, isDefault: false },
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
      { name: 'main', isRemote: false, isCurrent: true, isDefault: true },
      { name: 'feature/searchable', isRemote: false, isCurrent: false, isDefault: false },
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
      { name: 'main', isRemote: false, isCurrent: true, isDefault: true },
      { name: 'release', isRemote: false, isCurrent: false, isDefault: false },
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
      { name: 'main', isRemote: false, isCurrent: true, isDefault: true },
      { name: 'feat/abc', isRemote: false, isCurrent: false, isDefault: false },
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

  it('disables branch checkout while background tasks are running', async () => {
    const pane = await buildPane('main');
    const workspaceLock = makeWorkspaceLock({
      locked: true,
      reason: 'Workspace changes are unavailable while background tasks are running.',
      runningBackgroundCount: 1,
    });
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isRemote: false, isCurrent: true, isDefault: true },
      { name: 'feat/abc', isRemote: false, isCurrent: false, isDefault: false },
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
