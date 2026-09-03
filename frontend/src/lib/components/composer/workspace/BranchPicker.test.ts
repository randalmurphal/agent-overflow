import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import BranchPicker from './BranchPicker.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Project, Thread } from '../../../types/models';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';
import {
  enterCreateBranchMode,
  resetForTest as resetWorktreeIntent,
  setNewBranchName,
  setThreadEnvMode,
  worktreeIntentForThread,
} from '../../../stores/worktreeIntent.svelte';
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../../test/helpers/chat';
import { recentBranchSelections, recordBranchSelection } from '../../../stores/branchMru';
import { resetAppStorageForTest } from '../../../stores/appStorage';
import { __seedGitStatusForTest } from '../../../stores/gitStatusStore.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../../stores/panes.svelte';
import { idleWorkspaceActivity } from '../../../../test/helpers/workspaceLock';

function makeThread(branch: string, overrides: Partial<Thread> = {}): Thread {
  return makeBaseThread({
    workspacePath: '/repo',
    projectPath: '/repo',
    model: 'm',
    branch,
    ...overrides,
  });
}

// The one checkout every pane in this suite addresses: project `project-1`
// at `/repo`. Every converted git RPC takes exactly this value.
const WS = { projectId: 'project-1', workspacePath: '/repo' };

/** What GitCheckout answers: the caller's checkout after the branch moved. */
function checkoutState(branch: string, workspacePath = '/repo') {
  return { workspacePath, worktreePath: '', branch };
}

function makeProject(overrides: Partial<Project> = {}): Project {
  return {
    id: 'project-1',
    path: '/repo',
    name: 'Repo',
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(branch: string, overrides: Partial<Thread> = {}) {
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
  return buildRegisteredPane(makeThread(branch, overrides));
}

function buildPlaceholderPane(branch = 'main', paneId?: string) {
  const pane = createThreadPane(paneId ? { paneId } : undefined);
  pane.startDraftPlaceholder(makeProject(), 'chat', {
    provider: 'claude',
    model: 'm',
    workspacePath: '/repo',
    branch,
  });
  if (paneId) registerPaneForTest(paneId, pane);
  return pane;
}

describe('<BranchPicker>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    resetAppStorageForTest();
    resetPanesForTest();
  });

  it('renders the current branch on the trigger', async () => {
    const pane = await buildPane('main');
    const { getByTestId } = render(BranchPicker, { props: { pane } });
    expect(getByTestId('branch-picker-trigger').textContent ?? '').toMatch(/main/);
  });

  it('updates the trigger when the pane thread branch changes after mount', async () => {
    const pane = await buildPane('main');
    const { getByTestId } = render(BranchPicker, { props: { pane } });

    pane.replaceThread({ ...pane.thread!, branch: 'feature/external' });

    await waitFor(() => {
      expect(getByTestId('branch-picker-trigger').textContent ?? '').toMatch(/feature\/external/);
    });
  });

  it('opens the dropdown and lists fetched branches', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
    ]);
    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });
    expect(row).toBeTruthy();
  });

  it('pins the default branch above other fetched branches', async () => {
    const pane = await buildPane('gpui-spike');
    setBindingMock('GitListBranches', async () => [
      { name: 'gpui-spike', isCurrent: true, isDefault: false },
      { name: 'keybinding-overhaul', isCurrent: false, isDefault: false },
      { name: 'main', isCurrent: false, isDefault: true },
      { name: 'multi-pane', isCurrent: false, isDefault: false },
    ]);

    const { getAllByRole, getByTestId, findByRole } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /main/ });

    const rowLabels = getAllByRole('menuitem').map((row) => row.textContent ?? '');
    const mainIndex = rowLabels.findIndex((label) => label.includes('main'));
    const gpuiIndex = rowLabels.findIndex((label) => label.includes('gpui-spike'));
    const keybindingIndex = rowLabels.findIndex((label) => label.includes('keybinding-overhaul'));

    expect(mainIndex).toBeGreaterThanOrEqual(0);
    expect(mainIndex).toBeLessThan(gpuiIndex);
    expect(mainIndex).toBeLessThan(keybindingIndex);
  });

  it('lifts recently-selected branches above the rest, below the default pin', async () => {
    const pane = await buildPane('main', { projectId: 'project-1' });
    recordBranchSelection('project-1', 'feat/older-pick');
    recordBranchSelection('project-1', 'feat/newest-pick');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/latest-commit', isCurrent: false, isDefault: false },
      { name: 'feat/older-pick', isCurrent: false, isDefault: false },
      { name: 'feat/newest-pick', isCurrent: false, isDefault: false },
    ]);

    const { getAllByRole, getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feat\/latest-commit/ });

    const rowLabels = getAllByRole('menuitem').map((row) => row.textContent ?? '');
    const indexOf = (needle: string) => rowLabels.findIndex((label) => label.includes(needle));
    expect(indexOf('main')).toBeLessThan(indexOf('feat/newest-pick'));
    expect(indexOf('feat/newest-pick')).toBeLessThan(indexOf('feat/older-pick'));
    expect(indexOf('feat/older-pick')).toBeLessThan(indexOf('feat/latest-commit'));
  });

  it('records a checkout selection into the project MRU', async () => {
    const pane = await buildPane('main', { projectId: 'project-1' });
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GitCheckout', async () => checkoutState('feat/abc'));
    setBindingMock('GetThread', async () => makeThread('feat/abc', { projectId: 'project-1' }));

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });
    await fireEvent.click(row);

    await waitFor(() => {
      expect(recentBranchSelections('project-1')).toEqual(['feat/abc']);
    });
  });

  it('refreshes the open branch list when the current branch changes externally', async () => {
    const pane = await buildPane('main');
    let listCallCount = 0;
    setBindingMock('GitListBranches', async () => {
      listCallCount += 1;
      if (listCallCount === 1) {
        return [
          { name: 'main', isCurrent: true, isDefault: true },
          { name: 'feature/external', isCurrent: false, isDefault: false },
        ];
      }
      return [
        { name: 'main', isCurrent: false, isDefault: true },
        { name: 'feature/external', isCurrent: true, isDefault: false },
      ];
    });

    const { getByTestId, findByRole } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feature\/external/ });

    pane.replaceThread({ ...pane.thread!, branch: 'feature/external' });

    await waitFor(() => {
      expect(listCallCount).toBe(2);
      expect(getByTestId('branch-picker-trigger').textContent ?? '').toMatch(/feature\/external/);
    });
  });

  it('does not let a stale branch-list response overwrite a newer branch refresh', async () => {
    const pane = await buildPane('main');
    let resolveInitial: ((branches: unknown[]) => void) | undefined;
    let listCallCount = 0;
    setBindingMock('GitListBranches', async () => {
      listCallCount += 1;
      if (listCallCount === 1) {
        return new Promise((resolve) => {
          resolveInitial = resolve;
        });
      }
      return [
        { name: 'feature/external', isCurrent: true, isDefault: false },
      ];
    });

    const { getByTestId, findByRole, queryByRole } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));

    pane.replaceThread({ ...pane.thread!, branch: 'feature/external' });

    resolveInitial!([
      { name: 'main', isCurrent: true, isDefault: true },
    ]);

    await waitFor(() => {
      expect(listCallCount).toBe(2);
      expect(queryByRole('menuitem', { name: /^main/ })).toBeNull();
    });
    expect(await findByRole('menuitem', { name: /feature\/external/ })).toBeTruthy();
  });

  it('calls GitCheckout with the pane checkout on selection', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GitCheckout', async () => checkoutState('feat/abc'));
    setBindingMock('GetThread', async () => makeThread('feat/abc'));

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });
    await fireEvent.click(row);
    await Promise.resolve();
    await Promise.resolve();

    await waitFor(() => {
      // One path for every pane: the checkout's subject is the DIRECTORY.
      // Persisted rows in it are re-branched by the backend and arrive as a
      // thread:updated broadcast, so nothing re-reads the row here.
      expect(getBindingMock('GitCheckout')!.mock.calls[0]).toEqual([WS, 'feat/abc']);
      expect(getBindingMock('GetThread')).not.toHaveBeenCalled();
    });
  });

  // The refresh legs used to re-read `pane.thread` AFTER the awaits, so a
  // pane switch mid-checkout asked the backend about thread B and then wrote
  // B's workspace and branch into the draft placeholders parked on thread
  // A's project.
  it('applies the checkout it launched, not whatever the pane switched to', async () => {
    const pane = await buildPane('main', { projectId: 'project-1' });
    const sibling = buildPlaceholderPane('main', 'pane-1');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GitMaybeFetchRemotes', async () => false);
    let finishCheckout: (() => void) | undefined;
    setBindingMock('GitCheckout', () => new Promise((resolve) => {
      finishCheckout = () => resolve(checkoutState('feat/abc'));
    }));
    setBindingMock('GetThread', async (threadId: unknown) =>
      threadId === 'thread-1'
        ? makeThread('feat/abc')
        : makeThread('other-branch', {
          id: 'thread-2',
          workspacePath: '/other',
          projectPath: '/other',
        }));

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await fireEvent.click(await findByRole('menuitem', { name: /feat\/abc/ }));
    await waitFor(() => expect(finishCheckout).toBeDefined());

    // The user moves the pane onto another thread in another worktree while
    // the checkout is still running.
    await pane.switchThread(makeThread('other-branch', {
      id: 'thread-2',
      workspacePath: '/other',
      projectPath: '/other',
    }));
    finishCheckout!();

    await waitFor(() => {
      // The captured ref is A's checkout, whatever the pane shows now…
      expect(getBindingMock('GitCheckout')!.mock.calls[0]).toEqual([WS, 'feat/abc']);
      // …and thread B's workspace never reaches the drafts parked on A's.
      expect(sibling.thread?.branch).toBe('feat/abc');
      expect(sibling.thread?.workspacePath).toBe('/repo');
    });
  });

  it('checks out branches for placeholders without materializing a thread', async () => {
    const pane = buildPlaceholderPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
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
    setBindingMock('GitMaybeFetchRemotes', async () => false);
    const checkout = setBindingMock('GitCheckout', async () => ({
      workspacePath: '/repo',
      branch: 'feat/abc',
    }));
    setBindingMock('CreateThread', async () => {
      throw new Error('CreateThread must not run for placeholder checkout');
    });

    const { getByTestId, findByRole } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });
    await fireEvent.click(row);

    await waitFor(() => {
      expect(checkout.mock.calls[0]).toEqual([WS, 'feat/abc']);
      expect(pane.threadId).toBeNull();
      expect(pane.thread?.branch).toBe('feat/abc');
    });
    expect(getBindingMock('CreateThread')).not.toHaveBeenCalled();
  });

  it('moves every draft composer in the checked-out workspace, not just the acting one', async () => {
    const acting = buildPlaceholderPane('main', 'main');
    const sibling = buildPlaceholderPane('main', 'pane-1');
    const elsewhere = buildPlaceholderPane('main', 'pane-2');
    // A worktree the checkout did not touch.
    elsewhere.applyDraftPlaceholderWorkspace({
      workspacePath: '/repo/.wt/a',
      worktreePath: '/repo/.wt/a',
      branch: 'wt-branch',
    });
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
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
    setBindingMock('GitMaybeFetchRemotes', async () => false);
    setBindingMock('GitCheckout', async () => ({
      workspacePath: '/repo',
      branch: 'feat/abc',
    }));

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane: acting } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await fireEvent.click(await findByRole('menuitem', { name: /feat\/abc/ }));

    await waitFor(() => {
      expect(acting.thread?.branch).toBe('feat/abc');
      expect(sibling.thread?.branch).toBe('feat/abc');
    });
    expect(elsewhere.thread?.branch).toBe('wt-branch');
  });

  it('ignores a stale placeholder checkout response after the placeholder is replaced', async () => {
    const pane = buildPlaceholderPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
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
    setBindingMock('GitMaybeFetchRemotes', async () => false);
    let resolveCheckout: ((value: { workspacePath: string; branch: string }) => void) | undefined;
    setBindingMock('GitCheckout', async () => new Promise((resolve) => {
      resolveCheckout = resolve;
    }));

    const { getByTestId, findByRole } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });
    await fireEvent.click(row);
    await waitFor(() => expect(resolveCheckout).toBeDefined());

    pane.startDraftPlaceholder(makeProject({ id: 'project-2', path: '/other', name: 'Other' }), 'chat', {
      provider: 'claude',
      model: 'm',
      workspacePath: '/other',
      branch: 'main',
    });
    resolveCheckout!({ workspacePath: '/repo', branch: 'feat/abc' });

    await waitFor(() => {
      expect(pane.thread?.projectId).toBe('project-2');
      expect(pane.thread?.workspacePath).toBe('/other');
      expect(pane.thread?.branch).toBe('main');
    });
  });

  it('does not move placeholders to an existing worktree from the branch picker', async () => {
    const pane = buildPlaceholderPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/worktree', isCurrent: false, isDefault: false, worktreePath: '/tmp/wt' },
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
    setBindingMock('GitMaybeFetchRemotes', async () => false);
    const checkout = setBindingMock('GitCheckout', async () => ({
      workspacePath: '/tmp/wt',
      worktreePath: '/tmp/wt',
      branch: 'feat/worktree',
    }));

    const { getByTestId, findByRole } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/worktree/ });
    expect(row).toHaveAttribute('aria-disabled', 'true');
    expect(row).toHaveAttribute('title', expect.stringContaining('/tmp/wt'));
    await fireEvent.click(row);

    expect(checkout).not.toHaveBeenCalled();
    expect(pane.thread?.workspacePath).toBe('/repo');
    expect(pane.thread?.worktreePath).toBeUndefined();
    expect(pane.thread?.branch).toBe('main');
  });

  it('disables a branch checked out in another worktree in local mode', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'feat/worktree', isCurrent: false, isDefault: false, worktreePath: '/tmp/wt' },
    ]);
    const checkout = setBindingMock('GitCheckout', async () => checkoutState('feat/abc'));

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/worktree/ });
    expect(row).toHaveAttribute('aria-disabled', 'true');
    expect(row).toHaveAttribute('title', expect.stringContaining('/tmp/wt'));
    await fireEvent.click(row);

    expect(checkout).not.toHaveBeenCalled();
    expect(getBindingMock('UpdateThreadWorkspace')).toBeUndefined();
  });

  it('checks out the default branch in the current worktree', async () => {
    const pane = await buildPane('feature', {
      workspacePath: '/repo-worktrees/feature',
      worktreePath: '/repo-worktrees/feature',
      projectPath: '/repo',
    });
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: false, isDefault: true },
      { name: 'feature', isCurrent: true, isDefault: false },
    ]);
    setBindingMock('GitCheckout', async () => checkoutState('main', '/repo-worktrees/feature'));
    setBindingMock('GetThread', async () => makeThread('main', {
      workspacePath: '/repo-worktrees/feature',
      worktreePath: '/repo-worktrees/feature',
      projectPath: '/repo',
    }));

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /main/ });
    await fireEvent.click(row);

    await waitFor(() => {
      expect(getBindingMock('GitCheckout')!.mock.calls[0]).toEqual([
        { projectId: 'project-1', workspacePath: '/repo-worktrees/feature' },
        'main',
      ]);
      expect(getBindingMock('UpdateThreadWorkspace')).toBeUndefined();
    });
  });

  it('filters branches from the search input', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feature/searchable', isCurrent: false, isDefault: false },
    ]);

    const { getByTestId, getByPlaceholderText, queryByRole, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feature\/searchable/ });

    await fireEvent.input(getByPlaceholderText('Search Branches'), { target: { value: 'searchable' } });

    expect(queryByRole('menuitem', { name: /^main/ })).toBeNull();
    expect(await findByRole('menuitem', { name: /feature\/searchable/ })).toBeTruthy();
  });

  it('stages the attach target when new worktree mode is pending and not creating a branch', async () => {
    const pane = await buildPane('main');
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'release', isCurrent: false, isDefault: false },
    ]);

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    // Trigger label reads as the current/attach branch (not "From X")
    // because we're picking which existing branch the new worktree
    // attaches to, not branching off it.
    expect(getByTestId('branch-picker-trigger').textContent ?? '').toMatch(/main/);
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /release/ });
    await fireEvent.click(row);

    expect(worktreeIntentForThread(pane.thread).attachBranch).toBe('release');
    expect(worktreeIntentForThread(pane.thread).creatingBranch).toBe(false);
    expect(getBindingMock('GitCheckout')).toBeUndefined();
  });

  it('stages the new-branch base when new-worktree + creating-branch is active', async () => {
    const pane = await buildPane('main');
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');
    enterCreateBranchMode(pane.thread, { workspaceDirty: false, currentBranch: 'main' });
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'release', isCurrent: false, isDefault: false },
    ]);

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    expect(getByTestId('branch-picker-trigger').textContent ?? '').toMatch(/From/);
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /release/ });
    await fireEvent.click(row);

    expect(worktreeIntentForThread(pane.thread).newBranchBase).toBe('release');
    expect(getBindingMock('GitCheckout')).toBeUndefined();
  });

  it('disables an attach target that is already checked out in another worktree', async () => {
    const pane = await buildPane('main');
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/has-wt', isCurrent: false, isDefault: false, worktreePath: '/tmp/wt-feat' },
    ]);

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/has-wt/ });
    expect(row).toHaveAttribute('aria-disabled', 'true');
    expect(row).toHaveAttribute('title', expect.stringContaining('/tmp/wt-feat'));
    await fireEvent.click(row);

    expect(getBindingMock('UpdateThreadWorkspace')).toBeUndefined();
    expect(worktreeIntentForThread(pane.thread).mode).toBe('new-worktree');
    expect(worktreeIntentForThread(pane.thread).attachBranch).toBe('');
  });

  it('allows branch checkout while the agent is responding', async () => {
    const pane = await buildPane('main');
    const checkout = setBindingMock('GitCheckout', async () => checkoutState('feat/abc'));
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GetThread', async () => makeThread('feat/abc'));

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });
    expect(row).not.toHaveAttribute('aria-disabled', 'true');

    await fireEvent.click(row);

    await waitFor(() => {
      expect(checkout).toHaveBeenCalled();
    });
  });

  it('+ New branch dropdown entry enters creating-branch mode and closes the menu', async () => {
    const pane = await buildPane('main');
    if (!pane.thread) throw new Error('missing test thread');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
    ]);

    const { getByTestId, findByRole, queryByRole } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));

    const newRow = await findByRole('menuitem', { name: /New branch/ });
    await fireEvent.click(newRow);

    // Dropdown closes; intent flips into creating-branch mode. The
    // text input itself lives outside BranchPicker (in
    // WorktreeNameInput.svelte), so we assert via the store.
    await waitFor(() => {
      expect(queryByRole('menu')).toBeNull();
    });
    expect(worktreeIntentForThread(pane.thread).creatingBranch).toBe(true);
  });

  it('replaces the new-branch popup entry with a cancel action while creating', async () => {
    const pane = await buildPane('main');
    if (!pane.thread) throw new Error('missing test thread');
    enterCreateBranchMode(pane.thread, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchName(pane.thread, 'feat/new');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
    ]);

    const { getByTestId, findByRole, queryByRole } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));

    expect(queryByRole('menuitem', { name: /New branch/ })).toBeNull();
    const cancelRow = await findByRole('menuitem', { name: /Cancel new branch/ });
    await fireEvent.click(cancelRow);

    await waitFor(() => {
      expect(queryByRole('menu')).toBeNull();
    });
    const intent = worktreeIntentForThread(pane.thread);
    expect(intent.creatingBranch).toBe(false);
    expect(intent.newBranchName).toBe('');
    expect(intent.newBranchBase).toBe('');
    expect(getBindingMock('GitCheckout')).toBeUndefined();
  });

  it('flips the new-branch base to the Local sentinel when picking the Base row while creating', async () => {
    const pane = await buildPane('main');
    if (!pane.thread) throw new Error('missing test thread');
    // Local row is a base picker for the new branch — only meaningful
    // when creatingBranch=true. Enter that mode explicitly so the row
    // becomes available.
    enterCreateBranchMode(pane.thread, { workspaceDirty: true, currentBranch: 'main' });
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
    ]);
    // A real thread reads the dirty bit off the workspace's shared git status
    // rather than re-fetching it — only a draft placeholder, which has no
    // workspace entity yet, still calls GetGitStatus.
    __seedGitStatusForTest('/repo', {
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
    });

    const { getByTestId, findByRole } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const localRow = await findByRole('menuitem', { name: /Base \(with changes\)/ });
    await fireEvent.click(localRow);

    expect(worktreeIntentForThread(pane.thread).newBranchBase).toBe('__LOCAL__');
  });

  it('renders ahead/behind arrows next to branches with upstream diffs', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true, aheadCount: 3, behindCount: 2 },
      { name: 'feat/ahead-only', isCurrent: false, isDefault: false, aheadCount: 1 },
      { name: 'feat/behind-only', isCurrent: false, isDefault: false, behindCount: 4 },
      { name: 'feat/clean', isCurrent: false, isDefault: false },
    ]);

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
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
      props: { pane },
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

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));

    await waitFor(async () => {
      const row = await findByRole('menuitem', { name: /main/ });
      expect(row.textContent ?? '').toMatch(/↑5/);
    });
    expect(getBindingMock('GitMaybeFetchRemotes')!.mock.calls[0]).toEqual([WS]);
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

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
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
      props: { pane },
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

  it('opens the prune preview dialog from the menu and closes the popover', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
    ]);
    setBindingMock('GitListBranchPruneCandidates', async () => ({
      candidates: [
        {
          branch: 'merged-gone',
          tip: 'a'.repeat(40),
          subject: 'work',
          safe: true,
          reason: 'merged into the default branch',
        },
      ],
    }));

    const { getByTestId, findByRole, findByTestId, queryByTestId } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const pruneRow = await findByRole('menuitem', { name: /Prune branches/ });
    await fireEvent.click(pruneRow);

    await findByTestId('prune-dialog-list');
    expect(getBindingMock('GitListBranchPruneCandidates')!.mock.calls[0]).toEqual([WS]);
    // Popover closed behind the dialog (its trigger reads collapsed).
    expect(getByTestId('branch-picker-trigger').getAttribute('aria-expanded')).toBe('false');
    expect(queryByTestId('branch-picker-loading')).toBeNull();
  });

  it('exposes an enabled sync action on a branch that is purely behind upstream', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/behind', isCurrent: false, isDefault: false, behindCount: 3 },
    ]);

    const { getByTestId, findByRole, findByLabelText } = render(BranchPicker, {
      props: { pane },
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
      props: { pane },
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
      props: { pane },
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
    setBindingMock('GitCheckout', async () => checkoutState('feat/abc'));

    const { getByTestId, findByRole, findByLabelText, queryByLabelText } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feat\/behind/ });

    const syncBtn = await findByLabelText(/Sync feat\/behind from upstream/);
    await fireEvent.click(syncBtn);

    await waitFor(() => {
      expect(getBindingMock('GitSyncBranch')!.mock.calls[0]).toEqual([WS, 'feat/behind']);
    });
    expect(getBindingMock('GitCheckout')).not.toHaveBeenCalled();
    // After sync the row no longer carries behind > 0, so the action
    // disappears.
    await waitFor(() => {
      expect(queryByLabelText(/Sync feat\/behind from upstream/)).toBeNull();
    });
  });

  it('confirms before syncing a branch checked out in another worktree', async () => {
    const pane = await buildPane('feature', {
      projectId: 'project-1',
      workspacePath: '/repo-worktrees/feature',
      worktreePath: '/repo-worktrees/feature',
    });
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: false, isDefault: true, behindCount: 1, worktreePath: '/repo' },
      { name: 'feature', isCurrent: true, isDefault: false },
    ]);
    const sync = setBindingMock('GitSyncBranch', async () => [
      { name: 'main', isCurrent: false, isDefault: true, worktreePath: '/repo' },
      { name: 'feature', isCurrent: true, isDefault: false },
    ]);
    const { getByTestId, findByRole, findByLabelText, findByText, getByText } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    // Selection of a checked-out-elsewhere branch is disabled at the row,
    // but MenuItem actions are independent of row disabled — the sync
    // icon must stay live inside the disabled row.
    const row = await findByRole('menuitem', { name: /main/ });
    expect(row).toHaveAttribute('aria-disabled', 'true');

    const syncBtn = await findByLabelText(/Sync main from upstream/);
    expect(syncBtn).not.toBeDisabled();
    expect(syncBtn).toHaveAttribute('title', expect.stringContaining('/repo'));
    await fireEvent.click(syncBtn);

    expect(sync).not.toHaveBeenCalled();
    await findByText(/main is checked out in \/repo/);
    await fireEvent.click(getByText('Sync'));

    await waitFor(() => {
      expect(sync.mock.calls[0]).toEqual([WS, 'main']);
    });
  });

  it('syncs branches for placeholders without materializing a thread', async () => {
    const pane = buildPlaceholderPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/behind', isCurrent: false, isDefault: false, behindCount: 3 },
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
    setBindingMock('GitMaybeFetchRemotes', async () => false);
    const sync = setBindingMock('GitSyncBranch', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/behind', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('CreateThread', async () => {
      throw new Error('CreateThread must not run for placeholder sync');
    });

    const { getByTestId, findByRole, findByLabelText, queryByLabelText } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feat\/behind/ });

    const syncBtn = await findByLabelText(/Sync feat\/behind from upstream/);
    await fireEvent.click(syncBtn);

    await waitFor(() => {
      expect(sync.mock.calls[0]).toEqual([WS, 'feat/behind']);
      expect(queryByLabelText(/Sync feat\/behind from upstream/)).toBeNull();
    });
    expect(getBindingMock('CreateThread')).not.toHaveBeenCalled();
    expect(pane.threadId).toBeNull();
  });

  it('confirms cross-worktree sync while keeping create-branch base selection enabled', async () => {
    const pane = await buildPane('main');
    if (!pane.thread) throw new Error('missing test thread');
    pane.replaceThread({ ...pane.thread, projectId: 'project-1' });
    enterCreateBranchMode(pane.thread, { workspaceDirty: false, currentBranch: 'main' });
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/has-wt', isCurrent: false, isDefault: false, behindCount: 2, worktreePath: '/tmp/wt-feat' },
    ]);
    const sync = setBindingMock('GitSyncBranch', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/has-wt', isCurrent: false, isDefault: false, worktreePath: '/tmp/wt-feat' },
    ]);

    const { getByTestId, findByRole, findByLabelText, findByText, getByText } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/has-wt/ });
    expect(row).not.toHaveAttribute('aria-disabled', 'true');

    const syncBtn = await findByLabelText(/Sync feat\/has-wt from upstream/);
    await fireEvent.click(syncBtn);
    await findByText(/feat\/has-wt is checked out in \/tmp\/wt-feat/);
    await fireEvent.click(getByText('Sync'));

    await waitFor(() => {
      expect(sync.mock.calls[0]).toEqual([
        { projectId: 'project-1', workspacePath: '/tmp/wt-feat' },
        'feat/has-wt',
      ]);
    });
    expect(worktreeIntentForThread(pane.thread).creatingBranch).toBe(true);
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
      props: { pane },
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
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /feat\/diverged/ });

    const syncBtn = await findByLabelText(/Sync feat\/diverged from upstream/);
    await fireEvent.click(syncBtn);

    expect(getBindingMock('GitSyncBranch')).not.toHaveBeenCalled();
  });

  it('allows branch checkout while background tasks are running', async () => {
    const pane = await buildPane('main');
    const checkout = setBindingMock('GitCheckout', async () => checkoutState('feat/abc'));
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true },
      { name: 'feat/abc', isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GetThread', async () => makeThread('feat/abc'));

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });

    await waitFor(() => {
      expect(row).not.toHaveAttribute('aria-disabled', 'true');
    });

    await fireEvent.click(row);

    await waitFor(() => {
      expect(checkout).toHaveBeenCalled();
    });
  });
});
