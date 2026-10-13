import { describe, expect, it, beforeEach, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  archiveDoneChildrenAction,
  deleteThreadAction,
  demoteThreadAction,
  forkThreadAction,
  promoteThreadAction,
  type ThreadActionCtx,
} from './threadRowActions';
import { buildPane, makeThread as makeBaseThread } from '../../../test/helpers/chat';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import {
  getThreads,
  prependThread,
  removeThread,
} from '../../stores/threads.svelte';
import {
  collapseProject,
  isProjectExpanded,
} from '../../stores/sidebar.svelte';
import type { Thread } from '../../types/models';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { makeSettings } from '../../../test/helpers/settings';

function makeCtx(thread: Partial<Thread>): ThreadActionCtx {
  const t: Thread = {
    id: 'thread-1',
    title: 'Source thread',
    provider: 'claude',
    projectId: 'project-1',
    workspacePath: '/tmp/work',
    projectPath: '/tmp/work',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    sessionRef: 'session-1',
    ...thread,
  };
  return {
    thread: t,
    isActive: false,
    clearPane: vi.fn(),
    switchPane: vi.fn(async () => {}),
    reportError: vi.fn(),
  };
}

function clearThreadsForTest(): void {
  for (const t of Array.from(getThreads())) removeThread(t.id);
}

describe('forkThreadAction', () => {
  beforeEach(async () => {
    resetBindingMocks();
    clearThreadsForTest();
    collapseProject('project-1');
    for (const toast of Array.from(getToasts())) removeToast(toast.id);
    setBindingMock('GetSettings', async () => makeSettings());
    await loadSettings();
  });

  it('forks the source, prepends the new thread, expands the parent project, and switches the pane', async () => {
    const forked: Thread = {
      id: 'fork-1',
      title: 'Source thread (fork)',
      provider: 'claude',
      projectId: 'project-1',
      workspacePath: '/tmp/work',
      projectPath: '/tmp/work',
      model: 'claude-sonnet-4-6',
      createdAt: 1,
      updatedAt: 1,
      archived: false,
      sessionRef: 'session-1',
    };
    const fork = setBindingMock('ForkThread', vi.fn(async () => forked));
    const pinned = { ...forked, pinnedAt: 2, pinGroup: 0 };
    const pin = setBindingMock('PinThread', vi.fn(async () => pinned));
    const ctx = makeCtx({});
    // Source thread already in the sidebar so we can confirm prepend
    // ordering: fork lands before source.
    prependThread(ctx.thread);

    await forkThreadAction(ctx);

    expect(fork).toHaveBeenCalledWith('thread-1', null);
    expect(pin).toHaveBeenCalledWith('fork-1');
    const ids = getThreads().map((t) => t.id);
    expect(ids[0]).toBe('fork-1');
    expect(isProjectExpanded('project-1')).toBe(true);
    expect(ctx.switchPane).toHaveBeenCalledTimes(1);
    expect((ctx.switchPane as ReturnType<typeof vi.fn>).mock.calls[0][0]).toEqual(pinned);

    const toast = getToasts().find((t) => t.message.includes('Forked'));
    expect(toast?.type).toBe('info');
  });

  it('reports the user-facing error on fork failure', async () => {
    const failure = new Error('source thread is missing a session reference');
    setBindingMock('ForkThread', vi.fn(async () => {
      throw failure;
    }));
    const ctx = makeCtx({});
    await forkThreadAction(ctx);
    expect(ctx.reportError).toHaveBeenCalledTimes(1);
    expect((ctx.reportError as ReturnType<typeof vi.fn>).mock.calls[0][0]).toMatch(/missing a session/);
    expect((ctx.reportError as ReturnType<typeof vi.fn>).mock.calls[0][1]).toBe(failure);
    expect(ctx.switchPane).not.toHaveBeenCalled();
  });
});

describe('deleteThreadAction', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    clearThreadsForTest();
  });

  it('applies the rows its worktree removal moved before deleting the thread', async () => {
    const worktree = '/tmp/work-wt/feature';
    const sibling = makeBaseThread({ id: 'thread-sibling', workspacePath: worktree, worktreePath: worktree, projectPath: '/tmp/work', branch: 'feature' });
    const siblingPane = await buildPane(sibling, [], 'pane-sibling');
    const order: string[] = [];
    setBindingMock('StopSession', async () => {});
    setBindingMock('GitRemoveWorktree', async () => {
      order.push('remove');
      return {
        workspace: { workspacePath: worktree, worktreePath: worktree, branch: 'feature' },
        reattached: [{ ...sibling, workspacePath: '/tmp/work', worktreePath: '', branch: 'main' }],
      };
    });
    setBindingMock('DeleteThread', async () => {
      order.push(`delete, sibling at ${siblingPane.thread?.workspacePath}`);
    });

    await deleteThreadAction(makeCtx({ workspacePath: worktree, worktreePath: worktree }));

    expect(order).toEqual(['remove', 'delete, sibling at /tmp/work']);
    expect(siblingPane.thread?.worktreePath).toBe('');
    expect(siblingPane.thread?.branch).toBe('main');
  });
});

describe('standing agent actions', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    clearThreadsForTest();
  });

  it('promotes and demotes through the bindings, reporting a refusal', async () => {
    const promote = setBindingMock('PromoteStandingAgent', async () => ({ threadId: 'thread-1' }));
    const demote = setBindingMock('DemoteStandingAgent', async () => {});
    const ctx = makeCtx({});
    await promoteThreadAction(ctx);
    await demoteThreadAction(ctx);
    expect(promote).toHaveBeenCalledWith('thread-1');
    expect(demote).toHaveBeenCalledWith('thread-1');
    expect(ctx.reportError).not.toHaveBeenCalled();

    const demoteAndDelete = setBindingMock('DemoteStandingAgentAndDeleteHome', async () => {
      throw new Error('the thread was demoted, but its home folder was not deleted: permission denied');
    });
    await demoteThreadAction(ctx, { deleteHome: true });
    expect(demoteAndDelete).toHaveBeenCalledWith('thread-1');
    expect(demote).toHaveBeenCalledTimes(1);
    expect(ctx.reportError).toHaveBeenCalledTimes(1);
    expect(((ctx.reportError as ReturnType<typeof vi.fn>).mock.calls[0][1] as Error).message).toContain('home folder was not deleted');
    (ctx.reportError as ReturnType<typeof vi.fn>).mockClear();

    promote.mockImplementation(async () => { throw new Error('only a chat or plan thread can be one'); });
    await promoteThreadAction(ctx);
    expect(ctx.reportError).toHaveBeenCalledTimes(1);
    expect((ctx.reportError as ReturnType<typeof vi.fn>).mock.calls[0][0]).toContain('chat or plan');
  });

  it('archives the done children in order, drops their rows, and reports how many failed', async () => {
    const children: Thread[] = ['c1', 'c2', 'c3'].map((id) => makeBaseThread({ id, title: id, spawnedByThreadId: 'thread-1' }));
    for (const child of children) prependThread(child);
    const archive = setBindingMock('ArchiveThread', async (id: unknown) => {
      if (id === 'c2') throw new Error('locked');
    });
    const ctx = makeCtx({});
    await archiveDoneChildrenAction(ctx, children);
    expect(archive.mock.calls.map((call) => call[0])).toEqual(['c1', 'c2', 'c3']);
    expect(getThreads().map((t) => t.id).sort()).toEqual(['c2']);
    expect(ctx.reportError).toHaveBeenCalledTimes(1);
    expect((ctx.reportError as ReturnType<typeof vi.fn>).mock.calls[0][0]).toMatch(/^2\/3 archived: /);
  });
});
