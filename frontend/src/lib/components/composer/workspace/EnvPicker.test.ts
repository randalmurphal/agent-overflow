import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import EnvPicker from './EnvPicker.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Project, Thread } from '../../../types/models';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';
import { resetForTest as resetWorktreeIntent } from '../../../stores/worktreeIntent.svelte';
import type { WorkspaceChangeLockState } from '../../../stores/workspaceChangeLock.svelte';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/repo',
    projectPath: '/repo',
    mode: 'chat',
    model: 'm',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
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

async function buildPane(thread: Thread) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

function buildPlaceholderPane() {
  const pane = createThreadPane();
  pane.startDraftPlaceholder(makeProject(), 'chat', {
    provider: 'claude',
    model: 'm',
    workspacePath: '/tmp/wt-feature',
    branch: 'feat',
  });
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

describe('<EnvPicker>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
  });

  it('labels the trigger Local at the project root', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo' }));
    const { getByTestId } = render(EnvPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    const trigger = getByTestId('env-picker-trigger');
    expect(trigger.textContent ?? '').toMatch(/Local/);
    expect(trigger).toHaveAttribute('data-trigger-icon', 'folder');
  });

  it('labels the trigger with the worktree basename when off-root', async () => {
    const pane = await buildPane(
      makeThread({ workspacePath: '/tmp/wt-feature', projectPath: '/repo' }),
    );
    const { getByTestId } = render(EnvPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    const trigger = getByTestId('env-picker-trigger');
    expect(trigger.textContent ?? '').toMatch(/wt-feature/);
    expect(trigger).toHaveAttribute('data-trigger-icon', 'folder-git-2');
  });

  it('lists worktrees on open and switches via UpdateThreadWorkspace', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    setBindingMock('GitListWorktrees', async () => [
      { path: '/repo', branch: 'main', head: 'abc' },
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def' },
    ]);
    setBindingMock('UpdateThreadWorkspace', async () =>
      makeThread({ workspacePath: '/tmp/wt-feature' }),
    );
    const { getByTestId, findByRole } = render(EnvPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('env-picker-trigger'));

    const wtRow = await findByRole('menuitem', { name: /wt-feature/ });
    await fireEvent.click(wtRow);
    await Promise.resolve();
    await Promise.resolve();

    await waitFor(() => {
      const call = getBindingMock('UpdateThreadWorkspace')?.mock.calls[0];
      expect(call).toEqual(['thread-1', '/tmp/wt-feature']);
    });
  });

  it('stages a new worktree without switching immediately', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    setBindingMock('GitListWorktrees', async () => [{ path: '/repo', branch: 'main', head: 'abc' }]);

    const { getByTestId, findByRole } = render(EnvPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    const row = await findByRole('menuitem', { name: /New Worktree/ });
    await fireEvent.click(row);

    expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/New Worktree/);
    expect(getByTestId('env-picker-trigger')).toHaveAttribute('data-trigger-icon', 'folder-git-2');
    expect(getBindingMock('UpdateThreadWorkspace')).toBeUndefined();
  });

  it('disables workspace changes while the agent is responding', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    const workspaceLock = makeWorkspaceLock({
      locked: true,
      reason: 'Workspace changes are unavailable while the agent is responding.',
    });
    setBindingMock('GitListWorktrees', async () => [
      { path: '/repo', branch: 'main', head: 'abc' },
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def' },
    ]);
    setBindingMock('UpdateThreadWorkspace', async () =>
      makeThread({ workspacePath: '/tmp/wt-feature' }),
    );

    const { getByTestId, findByRole } = render(EnvPicker, { props: { pane, workspaceLock } });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    const newWorktreeRow = await findByRole('menuitem', { name: /New Worktree/ });
    expect(newWorktreeRow).toHaveAttribute('aria-disabled', 'true');
    const wtRow = await findByRole('menuitem', { name: /wt-feature/ });
    expect(wtRow).toHaveAttribute('aria-disabled', 'true');
    expect(wtRow).toHaveAttribute('title', expect.stringMatching(/agent is responding/));

    await fireEvent.click(wtRow);
    await fireEvent.click(newWorktreeRow);

    expect(getBindingMock('UpdateThreadWorkspace')).not.toHaveBeenCalled();
    expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/Local/);
  });

  it('disables workspace changes while background tasks are running', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    const workspaceLock = makeWorkspaceLock({
      locked: true,
      reason: 'Workspace changes are unavailable while background tasks are running.',
      runningBackgroundCount: 1,
    });
    setBindingMock('GitListWorktrees', async () => [
      { path: '/repo', branch: 'main', head: 'abc' },
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def' },
    ]);
    setBindingMock('UpdateThreadWorkspace', async () =>
      makeThread({ workspacePath: '/tmp/wt-feature' }),
    );

    const { getByTestId, findByRole } = render(EnvPicker, { props: { pane, workspaceLock } });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    const newWorktreeRow = await findByRole('menuitem', { name: /New Worktree/ });
    expect(newWorktreeRow).toHaveAttribute('aria-disabled', 'true');
    const wtRow = await findByRole('menuitem', { name: /wt-feature/ });

    await waitFor(() => {
      expect(wtRow).toHaveAttribute('aria-disabled', 'true');
      expect(wtRow).toHaveAttribute('title', expect.stringMatching(/background tasks/));
    });

    await fireEvent.click(wtRow);
    await fireEvent.click(newWorktreeRow);

    expect(getBindingMock('UpdateThreadWorkspace')).not.toHaveBeenCalled();
    expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/Local/);
  });

  it('opens an inline confirm strip and removes a clean worktree', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    setBindingMock('GitListWorktrees', async () => [
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def' },
    ]);
    setBindingMock('GitWorktreeStatus', async () => ({
      path: '/tmp/wt-feature',
      branch: 'feat',
      dirty: false,
      uncommittedCount: 0,
      unpushedCommits: 0,
      hasUpstream: true,
      attachedThreads: 0,
    }));
    setBindingMock('RemoveOtherWorktree', async () => undefined);

    const { getByTestId, findByLabelText, findByTestId } = render(EnvPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('env-picker-trigger'));

    const trash = await findByLabelText(/Remove worktree wt-feature/);
    await fireEvent.click(trash);

    const confirmRow = await findByTestId('env-picker-confirm-row');
    expect(confirmRow.textContent ?? '').toMatch(/Remove\s*wt-feature/);

    const removeBtn = await findByTestId('env-picker-confirm-remove');
    await fireEvent.click(removeBtn);

    await waitFor(() => {
      const call = getBindingMock('RemoveOtherWorktree')?.mock.calls[0];
      expect(call).toEqual(['thread-1', '/tmp/wt-feature', false]);
    });
  });

  it('removes worktrees for placeholders and updates placeholder workspace state', async () => {
    const pane = buildPlaceholderPane();
    setBindingMock('GitListWorktreesForProject', async () => [
      { path: '/repo', branch: 'main', head: 'abc' },
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def' },
    ]);
    setBindingMock('GitWorktreeStatusForProject', async () => ({
      path: '/tmp/wt-feature',
      branch: 'feat',
      dirty: false,
      uncommittedCount: 0,
      unpushedCommits: 0,
      hasUpstream: true,
      attachedThreads: 0,
    }));
    const remove = setBindingMock('RemoveOtherWorktreeForProject', async () => ({
      workspacePath: '/repo',
      branch: 'main',
    }));
    setBindingMock('CreateThread', async () => {
      throw new Error('CreateThread must not run for placeholder worktree removal');
    });

    const { getByTestId, findByLabelText, findByTestId } = render(EnvPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('env-picker-trigger'));

    const trash = await findByLabelText(/Remove worktree wt-feature/);
    await fireEvent.click(trash);
    const removeBtn = await findByTestId('env-picker-confirm-remove');
    await fireEvent.click(removeBtn);

    await waitFor(() => {
      expect(remove.mock.calls[0]).toEqual(['project-1', '/tmp/wt-feature', '/tmp/wt-feature', false]);
      expect(pane.threadId).toBeNull();
      expect(pane.thread?.workspacePath).toBe('/repo');
      expect(pane.thread?.worktreePath).toBe('');
      expect(pane.thread?.branch).toBe('main');
    });
    expect(getBindingMock('RemoveOtherWorktree')).toBeUndefined();
    expect(getBindingMock('CreateThread')).not.toHaveBeenCalled();
  });

  it('ignores a stale placeholder worktree removal response after the placeholder is replaced', async () => {
    const pane = buildPlaceholderPane();
    setBindingMock('GitListWorktreesForProject', async () => [
      { path: '/repo', branch: 'main', head: 'abc' },
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def' },
    ]);
    setBindingMock('GitWorktreeStatusForProject', async () => ({
      path: '/tmp/wt-feature',
      branch: 'feat',
      dirty: false,
      uncommittedCount: 0,
      unpushedCommits: 0,
      hasUpstream: true,
      attachedThreads: 0,
    }));
    let resolveRemove: ((value: { workspacePath: string; branch: string }) => void) | undefined;
    setBindingMock('RemoveOtherWorktreeForProject', async () => new Promise((resolve) => {
      resolveRemove = resolve;
    }));

    const { getByTestId, findByLabelText, findByTestId } = render(EnvPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    const trash = await findByLabelText(/Remove worktree wt-feature/);
    await fireEvent.click(trash);
    const removeBtn = await findByTestId('env-picker-confirm-remove');
    await fireEvent.click(removeBtn);
    await waitFor(() => expect(resolveRemove).toBeDefined());

    pane.startDraftPlaceholder(makeProject({ id: 'project-2', path: '/other', name: 'Other' }), 'chat', {
      provider: 'claude',
      model: 'm',
      workspacePath: '/other',
      branch: 'main',
    });
    resolveRemove!({ workspacePath: '/repo', branch: 'main' });

    await waitFor(() => {
      expect(pane.thread?.projectId).toBe('project-2');
      expect(pane.thread?.workspacePath).toBe('/other');
      expect(pane.thread?.branch).toBe('main');
    });
  });

  it('confirms with the Discard variant when the worktree is risky', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    setBindingMock('GitListWorktrees', async () => [
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def' },
    ]);
    setBindingMock('GitWorktreeStatus', async () => ({
      path: '/tmp/wt-feature',
      branch: 'feat',
      dirty: true,
      uncommittedCount: 3,
      unpushedCommits: 0,
      hasUpstream: true,
      attachedThreads: 0,
    }));
    setBindingMock('RemoveOtherWorktree', async () => undefined);

    const { getByTestId, findByLabelText, findByTestId, queryByTestId } = render(EnvPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('env-picker-trigger'));

    const trash = await findByLabelText(/Remove worktree wt-feature/);
    await fireEvent.click(trash);

    const force = await findByTestId('env-picker-confirm-force');
    expect(force.textContent ?? '').toMatch(/Discard and remove/);
    expect(queryByTestId('env-picker-confirm-remove')).toBeNull();

    await fireEvent.click(force);

    await waitFor(() => {
      const call = getBindingMock('RemoveOtherWorktree')?.mock.calls[0];
      expect(call).toEqual(['thread-1', '/tmp/wt-feature', true]);
    });
  });
});
