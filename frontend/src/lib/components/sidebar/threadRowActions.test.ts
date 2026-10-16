import { describe, expect, it, beforeEach, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  archiveDoneChildrenAction,
  deleteThreadAction,
  forkThreadAction,
  nextAssistantThreadAction,
  type ThreadActionCtx,
} from './threadRowActions';
import { buildPane, makeThread as makeBaseThread } from '../../../test/helpers/chat';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { createComposerDraftStore, resetComposerDraftSnapshotsForTest } from '../../stores/composerDraft.svelte';
import { registerComposerDraft } from '../../stores/composerDraftRegistry.svelte';
import type { Draft } from '../../types/draft';
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

/** Lets an action that was not refused reach its RPC. */
function settle(): Promise<void> {
  return new Promise((resolve) => { setTimeout(resolve, 20); });
}

describe('standing agent actions', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    clearThreadsForTest();
  });

  it("starts the assistant's next thread, shows it in the pane that asked, and reports a refusal", async () => {
    const successor = makeBaseThread({ id: 'thread-2', title: 'Ops', agentId: 'agent-1' });
    const next = setBindingMock('StartNextAssistantThread', async () => successor);
    const ctx = { ...makeCtx({ agentId: 'agent-1' }), isActive: true };
    await nextAssistantThreadAction(ctx);
    expect(next).toHaveBeenCalledWith('thread-1');
    expect(ctx.switchPane).toHaveBeenCalledWith(successor);
    expect(getThreads().some((t) => t.id === 'thread-2')).toBe(true);
    expect(ctx.reportError).not.toHaveBeenCalled();

    next.mockImplementation(async () => { throw new Error('this assistant is archived; unarchive it first'); });
    await nextAssistantThreadAction(ctx);
    expect(ctx.reportError).toHaveBeenCalledTimes(1);
    expect((ctx.reportError as ReturnType<typeof vi.fn>).mock.calls[0][0]).toContain('archived');
  });

  it('saves the text typed just before New Thread, Same Assistant and holds the composer until the handoff ends', async () => {
    resetComposerDraftSnapshotsForTest();
    const saved = new Map<string, Draft>();
    const order: string[] = [];
    setBindingMock('GetDraft', async (id: string) => saved.get(id) ?? { threadId: id, content: '', attachmentIds: [], terminalChips: [], sourceProposedPlan: null, updatedAt: 0 });
    setBindingMock('ListAttachments', async () => []);
    const save = setBindingMock('SaveDraft', async (id: string, content: string) => {
      order.push(`save ${id} ${content}`);
      saved.set(id, { threadId: id, content, attachmentIds: [], terminalChips: [], sourceProposedPlan: null, updatedAt: 0 });
    });
    const ctx = { ...makeCtx({ agentId: 'agent-1' }), isActive: true };
    const pane = createThreadPane({ paneId: 'handoff-draft' });
    pane.replaceThread(ctx.thread);
    registerPaneForTest(pane.paneId, pane);
    const draft = createComposerDraftStore({ debounceMs: 60_000 });
    const dispose = registerComposerDraft(pane.paneId, draft);
    try {
      await draft.setThread('thread-1');
      draft.setContent('half-typed note');
      const waiting: (() => void)[] = [];
      const next = setBindingMock('StartNextAssistantThread', async () => {
        order.push(`hand off, composer held ${draft.moving}`);
        await new Promise<void>((resolve) => { waiting.push(resolve); });
        return makeBaseThread({ id: 'thread-2', title: 'Ops', agentId: 'agent-1' });
      });
      const first = nextAssistantThreadAction(ctx);
      await vi.waitFor(() => expect(next).toHaveBeenCalledTimes(1));
      const second = nextAssistantThreadAction(ctx);
      await settle();
      expect(next).toHaveBeenCalledTimes(1);
      for (const resolve of waiting.splice(0)) resolve();
      await Promise.all([first, second]);
      expect(order).toEqual(['save thread-1 half-typed note', 'hand off, composer held true']);
      expect(draft.moving).toBe(false);
      expect(draft.hasPendingSave).toBe(false);

      // A save that fails stops the handoff, so the text is not left behind.
      draft.setContent('newer note');
      save.mockImplementation(async () => { throw new Error('disk full'); });
      await nextAssistantThreadAction(ctx);
      expect(next).toHaveBeenCalledTimes(1);
      expect((ctx.reportError as ReturnType<typeof vi.fn>).mock.calls.at(-1)?.[0]).toBe('Disk full.');
      expect(draft.moving).toBe(false);
    } finally {
      dispose();
      await draft.setThread(null);
    }
  });

  it('starts one handoff when a row with no pane showing it is clicked twice', async () => {
    const waiting: (() => void)[] = [];
    const next = setBindingMock('StartNextAssistantThread', async () => {
      await new Promise<void>((resolve) => { waiting.push(resolve); });
      return makeBaseThread({ id: 'thread-2', title: 'Ops', agentId: 'agent-1' });
    });
    const ctx = makeCtx({ agentId: 'agent-1' });
    const first = nextAssistantThreadAction(ctx);
    await vi.waitFor(() => expect(next).toHaveBeenCalledTimes(1));
    const second = nextAssistantThreadAction(makeCtx({ agentId: 'agent-1' }));
    await settle();
    expect(next).toHaveBeenCalledTimes(1);
    for (const resolve of waiting.splice(0)) resolve();
    await Promise.all([first, second]);
    expect(ctx.reportError).not.toHaveBeenCalled();
  });

  it('starts one handoff when the successor\'s header is clicked after the frame moved the pane and before the call returned', async () => {
    resetComposerDraftSnapshotsForTest();
    setBindingMock('GetDraft', async (id: string) => ({ threadId: id, content: '', attachmentIds: [], terminalChips: [], sourceProposedPlan: null, updatedAt: 0 }));
    setBindingMock('ListAttachments', async () => []);
    setBindingMock('SaveDraft', async () => {});
    const ctx = { ...makeCtx({ agentId: 'agent-1' }), isActive: true };
    const successor = makeBaseThread({ id: 'thread-2', title: 'Ops', agentId: 'agent-1' });
    const pane = createThreadPane({ paneId: 'handoff-frame' });
    pane.replaceThread(ctx.thread);
    registerPaneForTest(pane.paneId, pane);
    const draft = createComposerDraftStore({ debounceMs: 60_000 });
    const dispose = registerComposerDraft(pane.paneId, draft);
    try {
      await draft.setThread('thread-1');
      draft.setContent('half-typed note');
      const waiting: (() => void)[] = [];
      const next = setBindingMock('StartNextAssistantThread', async () => {
        await new Promise<void>((resolve) => { waiting.push(resolve); });
        return successor;
      });
      const first = nextAssistantThreadAction(ctx);
      await vi.waitFor(() => expect(next).toHaveBeenCalledTimes(1));
      // The unlisted frame naming the successor moves the pane
      // (moveAssistantPanes) before the call returns, which ends the
      // composer's hold.
      pane.replaceThread(successor);
      await draft.setThread('thread-2');
      expect(draft.moving).toBe(false);
      const second = nextAssistantThreadAction({ ...makeCtx({ id: 'thread-2', agentId: 'agent-1' }), isActive: true });
      await settle();
      expect(next).toHaveBeenCalledTimes(1);
      for (const resolve of waiting.splice(0)) resolve();
      await Promise.all([first, second]);
    } finally {
      dispose();
      await draft.setThread(null);
    }
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
