import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import WorktreeNameInput from './WorktreeNameInput.svelte';
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
import { buildPane, makeThread } from '../../../../test/helpers/chat';
import { makeWorkspaceLock } from '../../../../test/helpers/workspaceLock';

function threadOverrides() {
  return { branch: 'main', workspacePath: '/repo', projectPath: '/repo', projectId: 'project-1' };
}

async function buildWorktreePane() {
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  return buildPane(makeThread(threadOverrides()));
}

describe('<WorktreeNameInput>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
  });

  it('renders nothing without a staged intent', async () => {
    const pane = await buildWorktreePane();
    const { queryByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    expect(queryByTestId('apply-worktree-intent-button')).toBeNull();
    expect(queryByTestId('new-branch-toggle')).toBeNull();
  });

  it('applies a staged attach-worktree intent from the confirm button', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    const updated = makeThread({ ...threadOverrides(), branch: 'main', worktreePath: '/wt/main' });
    setBindingMock('AttachThreadWorktree', async () => updated);

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('apply-worktree-intent-button'));

    await waitFor(() => {
      expect(getBindingMock('AttachThreadWorktree')!.mock.calls[0]).toEqual(['thread-1', 'main']);
      expect(worktreeIntentForThread(pane.thread).mode).toBe('local');
    });
  });

  it('applies a staged new-branch worktree intent with the typed name', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    enterCreateBranchMode(pane.thread!, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchName(pane.thread!, 'feat/button');
    const updated = makeThread({ ...threadOverrides(), branch: 'feat/button' });
    setBindingMock('PrepareThreadWorktree', async () => updated);

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('apply-worktree-intent-button'));

    await waitFor(() => {
      expect(getBindingMock('PrepareThreadWorktree')!.mock.calls[0]).toEqual([
        'thread-1',
        'main',
        'feat/button',
        false,
      ]);
      expect(worktreeIntentForThread(pane.thread).creatingBranch).toBe(false);
    });
  });

  it('applies on Enter inside the branch-name input', async () => {
    const pane = await buildWorktreePane();
    enterCreateBranchMode(pane.thread!, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchName(pane.thread!, 'feat/enter');
    const updated = makeThread({ ...threadOverrides(), branch: 'feat/enter' });
    setBindingMock('GitCreateBranchFrom', async () => updated);

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.keyDown(getByTestId('worktree-branch-name-input'), { key: 'Enter' });

    await waitFor(() => {
      expect(getBindingMock('GitCreateBranchFrom')!.mock.calls[0]).toEqual([
        'thread-1',
        'feat/enter',
        'main',
        false,
      ]);
    });
  });

  it('disables the confirm button while the workspace is locked, with the reason as title', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    const { getByTestId } = render(WorktreeNameInput, {
      props: {
        pane,
        workspaceDirty: false,
        workspaceLock: makeWorkspaceLock({ locked: true, reason: 'turn 0 is active' }),
      },
    });
    const button = getByTestId('apply-worktree-intent-button') as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(button.title).toBe('turn 0 is active');
  });

  it('disables the confirm button when the branch name is empty', async () => {
    const pane = await buildWorktreePane();
    enterCreateBranchMode(pane.thread!, { workspaceDirty: false, currentBranch: 'main' });
    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    const button = getByTestId('apply-worktree-intent-button') as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(button.title).toBe('Enter a branch name first');
  });

  it('ignores repeat clicks while a slow apply is in flight', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    const updated = makeThread({ ...threadOverrides(), branch: 'main', worktreePath: '/wt/main' });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    setBindingMock('AttachThreadWorktree', async () => {
      await gate;
      return updated;
    });

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    const button = getByTestId('apply-worktree-intent-button');
    await fireEvent.click(button);
    await fireEvent.click(button);
    release();

    await waitFor(() => {
      expect(worktreeIntentForThread(pane.thread).mode).toBe('local');
    });
    expect(getBindingMock('AttachThreadWorktree')!.mock.calls.length).toBe(1);
  });

  it('keeps the staged intent when the backend refuses', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    setBindingMock('AttachThreadWorktree', async () => {
      throw new Error('worktree create failed: branch busy');
    });

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('apply-worktree-intent-button'));

    await waitFor(() => {
      expect(getBindingMock('AttachThreadWorktree')!.mock.calls.length).toBe(1);
      expect(
        (getByTestId('apply-worktree-intent-button') as HTMLButtonElement).disabled,
      ).toBe(false);
    });
    expect(worktreeIntentForThread(pane.thread).mode).toBe('new-worktree');
  });
});
