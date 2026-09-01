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
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../../test/helpers/chat';
import { idleWorkspaceActivity, makeWorkspaceLock } from '../../../../test/helpers/workspaceLock';
import { emitWailsEvent } from '../../../../test/mocks/wailsio-runtime';
import { registerPaneForTest, resetPanesForTest } from '../../../stores/panes.svelte';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return makeBaseThread({
    workspacePath: '/repo',
    projectPath: '/repo',
    model: 'm',
    ...overrides,
  });
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
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
  return buildRegisteredPane(thread);
}

function buildPlaceholderPane(paneId?: string, workspacePath = '/tmp/wt-feature', branch = 'feat') {
  const pane = createThreadPane(paneId ? { paneId } : undefined);
  pane.startDraftPlaceholder(makeProject(), 'chat', {
    provider: 'claude',
    model: 'm',
    workspacePath,
    branch,
  });
  if (paneId) registerPaneForTest(paneId, pane);
  return pane;
}

describe('<EnvPicker>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    resetPanesForTest();
  });

  it('labels the trigger Base at the project root', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo' }));
    const { getByTestId } = render(EnvPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    const trigger = getByTestId('env-picker-trigger');
    expect(trigger.textContent ?? '').toMatch(/Base/);
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

  it('materializes a placeholder when New Worktree is selected', async () => {
    const pane = buildPlaceholderPane(undefined, '/repo', 'main');
    setBindingMock('GitListWorktreesForProject', async () => [
      { path: '/repo', branch: 'main', head: 'abc' },
    ]);
    const create = setBindingMock('CreateThread', async () =>
      makeThread({ id: 'draft-row', isDraft: true, workspacePath: '/repo', branch: 'main' }),
    );

    const { getByTestId, findByRole } = render(EnvPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    await fireEvent.click(await findByRole('menuitem', { name: /New Worktree/ }));

    await waitFor(() => {
      expect(create).toHaveBeenCalledTimes(1);
      expect(pane.threadId).toBe('draft-row');
      expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/New Worktree/);
    });
  });

  it('materializes a placeholder directly in the selected existing worktree', async () => {
    const pane = buildPlaceholderPane(undefined, '/repo', 'main');
    setBindingMock('GitListWorktreesForProject', async () => [
      { path: '/repo', branch: 'main', head: 'abc' },
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def' },
    ]);
    const create = setBindingMock('CreateThread', async (options: Record<string, unknown>) =>
      makeThread({
        id: 'draft-row',
        isDraft: true,
        workspacePath: String(options.workspaceOverride),
        worktreePath: String(options.worktreePath),
        branch: String(options.branch),
      }),
    );

    const { getByTestId, findByRole } = render(EnvPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    await fireEvent.click(await findByRole('menuitem', { name: /wt-feature/ }));

    await waitFor(() => {
      expect(create).toHaveBeenCalledTimes(1);
      expect(pane.threadId).toBe('draft-row');
      expect(pane.thread?.workspacePath).toBe('/tmp/wt-feature');
      expect(pane.thread?.worktreePath).toBe('/tmp/wt-feature');
      expect(pane.thread?.branch).toBe('feat');
    });
    expect(getBindingMock('UpdateThreadWorkspace')).toBeUndefined();
  });

  it('disables workspace changes while the agent is responding', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    const workspaceLock = makeWorkspaceLock({
      locked: true,
      reason: 'Workspace changes are unavailable while the agent is responding.',
      threadLocked: true,
      threadReason: 'Workspace changes are unavailable while the agent is responding.',
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
    expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/Base/);
  });

  // The rows move THIS thread; they must not be pinned by a sibling thread
  // responding in the same directory (the directory view is what Remove
  // Worktree reads).
  it('keeps the rows live when only the DIRECTORY is busy with a sibling thread', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    const workspaceLock = makeWorkspaceLock({
      locked: true,
      reason: 'Workspace changes are unavailable while the agent is responding.',
    });
    setBindingMock('GitListWorktrees', async () => [
      { path: '/repo', branch: 'main', head: 'abc' },
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def' },
    ]);
    const update = setBindingMock('UpdateThreadWorkspace', async () =>
      makeThread({ workspacePath: '/tmp/wt-feature', worktreePath: '/tmp/wt-feature', branch: 'feat' }),
    );

    const { getByTestId, findByRole } = render(EnvPicker, { props: { pane, workspaceLock } });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    const newWorktreeRow = await findByRole('menuitem', { name: /New Worktree/ });
    expect(newWorktreeRow).not.toHaveAttribute('aria-disabled', 'true');
    const wtRow = await findByRole('menuitem', { name: /wt-feature/ });
    expect(wtRow).not.toHaveAttribute('aria-disabled', 'true');
    expect(wtRow).not.toHaveAttribute('title');

    await fireEvent.click(wtRow);
    await waitFor(() => {
      expect(update).toHaveBeenCalledWith('thread-1', '/tmp/wt-feature');
    });
  });

  it('disables workspace changes while background tasks are running', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    const workspaceLock = makeWorkspaceLock({
      locked: true,
      reason: 'Workspace changes are unavailable while background tasks are running.',
      threadLocked: true,
      threadReason: 'Workspace changes are unavailable while background tasks are running.',
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
    expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/Base/);
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

  it('disables removal when an attached thread is running', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    setBindingMock('GitListWorktrees', async () => [
      {
        path: '/tmp/wt-feature',
        branch: 'feat',
        head: 'def',
        deleteBlocked: true,
      },
    ]);

    const { getByTestId, findByLabelText, findByTestId } = render(EnvPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('env-picker-trigger'));

    const trash = await findByLabelText(/Remove worktree wt-feature/);
    expect(trash).toBeDisabled();
    expect(trash).toHaveAttribute('title', expect.stringMatching(/attached thread is running/));
    // The affordance flips from a trash icon to the running-dot so a
    // blocked row can't be misread as deletable at a glance.
    await findByTestId('env-picker-busy-wt-feature');
    await fireEvent.click(trash);

    expect(getBindingMock('GitWorktreeStatus')).toBeUndefined();
    expect(getBindingMock('RemoveOtherWorktree')).toBeUndefined();
  });

  it('re-fetches deleteBlocked when a turn starts while the popover is open', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    let blocked = false;
    setBindingMock('GitListWorktrees', async () => [
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def', deleteBlocked: blocked },
    ]);

    const { getByTestId, findByLabelText, findByTestId } = render(EnvPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    const trash = await findByLabelText(/Remove worktree wt-feature/);
    expect(trash).not.toBeDisabled();

    // A sibling thread starts a turn in that worktree while the popover
    // is still open — the list must refresh in place, not go stale.
    blocked = true;
    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-sibling',
      turnId: 'turn-1',
      turnIndex: 0,
    });

    await waitFor(
      () => {
        expect(getBindingMock('GitListWorktrees')!.mock.calls.length).toBeGreaterThan(1);
      },
      { timeout: 2000 },
    );
    await findByTestId('env-picker-busy-wt-feature');
    expect(await findByLabelText(/Remove worktree wt-feature/)).toBeDisabled();
  });

  it('still allows removing an idle worktree while this pane is busy', async () => {
    // The workspace lock only blocks moving THIS thread (switch / new
    // worktree); removal is gated per-worktree via deleteBlocked. A pane
    // mid-turn can clean up worktrees no running thread occupies.
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    const workspaceLock = makeWorkspaceLock({
      locked: true,
      reason: 'Workspace changes are unavailable while the agent is responding.',
    });
    setBindingMock('GitListWorktrees', async () => [
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def', deleteBlocked: false },
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
      props: { pane, workspaceLock },
    });
    await fireEvent.click(getByTestId('env-picker-trigger'));

    const trash = await findByLabelText(/Remove worktree wt-feature/);
    expect(trash).not.toBeDisabled();
    await fireEvent.click(trash);

    const removeButton = await findByTestId('env-picker-confirm-remove');
    await waitFor(() => expect(removeButton).not.toBeDisabled());
    await fireEvent.click(removeButton);

    await waitFor(() => {
      expect(getBindingMock('RemoveOtherWorktree')).toHaveBeenCalledWith(
        pane.threadId,
        '/tmp/wt-feature',
        false,
      );
    });
  });

  it('keeps the confirm strip open with the trimmed error when the backend refuses', async () => {
    // deleteBlocked is fetched at picker-open; an occupant's turn can start
    // after that, so the authoritative backend refusal must surface in the
    // strip (final `: `-segment only, per userFacingError) instead of
    // silently closing it.
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    setBindingMock('GitListWorktrees', async () => [
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def', deleteBlocked: false },
    ]);
    setBindingMock('GitWorktreeStatus', async () => ({
      path: '/tmp/wt-feature',
      branch: 'feat',
      dirty: false,
      uncommittedCount: 0,
      unpushedCommits: 0,
      hasUpstream: true,
      attachedThreads: 1,
    }));
    setBindingMock('RemoveOtherWorktree', async () => {
      throw new Error(
        'worktree /tmp/wt-feature in use by thread thread-2: cannot remove worktree while turn 0 is active',
      );
    });

    const { getByTestId, findByLabelText, findByTestId, findByText } = render(EnvPicker, {
      props: { pane, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    await fireEvent.click(await findByLabelText(/Remove worktree wt-feature/));
    const removeButton = await findByTestId('env-picker-confirm-remove');
    await waitFor(() => expect(removeButton).not.toBeDisabled());
    await fireEvent.click(removeButton);

    await findByText(/Cannot remove worktree while turn 0 is active/);
    // Strip stays open and re-armable after the refusal.
    const retryButton = await findByTestId('env-picker-confirm-remove');
    expect(retryButton).not.toBeDisabled();
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

  it('moves every draft composer parked in the removed worktree, not just the acting one', async () => {
    const acting = buildPlaceholderPane('main');
    const sibling = buildPlaceholderPane('pane-1');
    const elsewhere = buildPlaceholderPane('pane-2');
    // A composer that was never in the removed directory.
    elsewhere.applyDraftPlaceholderWorkspace({
      workspacePath: '/repo',
      worktreePath: '',
      branch: 'main',
    });
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
    setBindingMock('RemoveOtherWorktreeForProject', async () => ({
      workspacePath: '/repo',
      branch: 'main',
    }));

    const { getByTestId, findByLabelText, findByTestId } = render(EnvPicker, {
      props: { pane: acting, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    await fireEvent.click(await findByLabelText(/Remove worktree wt-feature/));
    await fireEvent.click(await findByTestId('env-picker-confirm-remove'));

    await waitFor(() => {
      expect(acting.thread?.workspacePath).toBe('/repo');
      expect(sibling.thread?.workspacePath).toBe('/repo');
      expect(sibling.thread?.worktreePath).toBe('');
      expect(sibling.thread?.branch).toBe('main');
    });
    expect(elsewhere.thread?.branch).toBe('main');
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
