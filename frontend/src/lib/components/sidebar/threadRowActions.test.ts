import { describe, expect, it, beforeEach, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { forkThreadAction, type ThreadActionCtx } from './threadRowActions';
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
  for (const t of [...getThreads()]) removeThread(t.id);
}

describe('forkThreadAction', () => {
  beforeEach(async () => {
    resetBindingMocks();
    clearThreadsForTest();
    collapseProject('project-1');
    for (const toast of [...getToasts()]) removeToast(toast.id);
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
    setBindingMock('ForkThread', vi.fn(async () => {
      throw new Error('source thread is missing a session reference');
    }));
    const ctx = makeCtx({});
    await forkThreadAction(ctx);
    expect(ctx.reportError).toHaveBeenCalledTimes(1);
    expect((ctx.reportError as ReturnType<typeof vi.fn>).mock.calls[0][0]).toMatch(/missing a session/);
    expect(ctx.switchPane).not.toHaveBeenCalled();
  });
});
