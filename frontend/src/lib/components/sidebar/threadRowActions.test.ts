import { describe, expect, it, afterEach, beforeEach, vi } from 'vitest';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { answerAsV0019 } from '../../../test/helpers/olderComputer';
import {
  archiveThreadAction,
  deleteThreadAction,
  deleteThreadsAction,
  forkThreadAction,
  markAllReadAction,
  nextAssistantThreadAction,
  pinThreadAction,
  setThreadPinGroupAction,
  unpinThreadAction,
  type ThreadActionCtx,
} from './threadRowActions';
import { applyThreadUpdated } from '../../stores/eventsThreadRows';
import { buildPane, makeThread as makeBaseThread } from '../../../test/helpers/chat';
import { getPane, registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { createComposerDraftStore, resetComposerDraftSnapshotsForTest } from '../../stores/composerDraft.svelte';
import { registerComposerDraft } from '../../stores/composerDraftRegistry.svelte';
import { TransportError } from '../../transport/wsClient';
import type { Draft } from '../../types/draft';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import {
  getThreadById,
  getThreads,
  prependThread,
  removeThread,
} from '../../stores/threads.svelte';
import type { Thread } from '../../types/models';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { makeSettings } from '../../../test/helpers/settings';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { getPinnedBackend } from '../../transport/backends';
import { HOME_BACKEND } from '../../transport/backendKey';
import { __resetEntityIndexForTest, noteThread } from '../../transport/entityIndex';
import { entryRecordForTest, setEntryExpanded } from '../../stores/sidebarEntryContinuity.svelte';

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
  for (const t of getThreads()) removeThread(t.id);
}

describe('forkThreadAction', () => {
  beforeEach(async () => {
    resetBindingMocks();
    clearThreadsForTest();
    setEntryExpanded('project-1', false);
    for (const toast of getToasts()) removeToast(toast.id);
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
    expect(entryRecordForTest('project-1').expanded).toBe(true);
    expect(ctx.switchPane).toHaveBeenCalledTimes(1);
    expect((ctx.switchPane as ReturnType<typeof vi.fn>).mock.calls[0][0]).toEqual(pinned);

    const toast = getToasts().find((t) => t.message.includes('Forked'));
    expect(toast?.type).toBe('info');
  });

  // The backend announces the fork as preparing while the call runs and as
  // ready when it ends, and the answer can arrive before the ready frame. A
  // preparing row cannot be mounted, so the answer has to win.
  it('opens the fork when its answer arrives before the ready frame', async () => {
    const forked = { ...makeCtx({}).thread, id: 'fork-2', title: 'Source thread (fork)', createdAt: 1, updatedAt: 1 };
    setBindingMock('ForkThread', vi.fn(async () => {
      applyThreadUpdated({ action: 'listed', thread: { ...forked, forkPreparing: true, sessionRef: '' } });
      return forked;
    }));
    setBindingMock('PinThread', vi.fn(async () => ({ ...forked, pinnedAt: 2, pinGroup: 0 })));
    const ctx = makeCtx({});
    prependThread(ctx.thread);

    await forkThreadAction(ctx);

    expect(ctx.reportError).not.toHaveBeenCalled();
    const opened = (ctx.switchPane as ReturnType<typeof vi.fn>).mock.calls[0][0] as Thread;
    expect(opened.forkPreparing ?? false).toBe(false);
    expect(opened.sessionRef).toBe('session-1');
    expect(getThreadById('fork-2')?.forkPreparing ?? false).toBe(false);
    expect(getToasts().some((t) => t.message.includes('Forked'))).toBe(true);
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

describe('archiveThreadAction', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    clearThreadsForTest();
    for (const toast of getToasts()) removeToast(toast.id);
  });

  it('treats an archive that left a worker open as archived, names the worker and archives it again', async () => {
    const archive = vi.fn(async (id: string) => (id === 'thread-1'
      ? { workersNotArchived: [{ threadId: 'worker-1', title: 'Parser review', error: 'thread is preparing a fork' }] }
      : {}));
    setBindingMock('ArchiveThread', archive);
    prependThread(makeCtx({}).thread);
    const ctx = makeCtx({});
    await archiveThreadAction(ctx);
    expect(ctx.reportError).not.toHaveBeenCalled();
    expect(getThreadById('thread-1')).toBeUndefined();
    const [toast] = getToasts();
    expect(toast.type).toBe('warning');
    expect(toast.message).toBe('Archived "Source thread", but its worker "Parser review" is still open: Thread is preparing a fork.');
    expect(toast.action?.label).toBe('Archive again');
    toast.action?.run();
    await vi.waitFor(() => expect(archive).toHaveBeenCalledWith('worker-1'));
    expect(getToasts()).toHaveLength(1);
  });

  // docs/specs/standing-agents.md, "Owned workers": an archive a paired
  // computer has not taken yet stands here and is sent again until it lands;
  // one that computer is too old for is said once. Neither is archived again
  // from here: the worker is not this computer's to archive.
  it('names a worker on another computer as pending there, and one on an older computer once, archiving neither again', async () => {
    const archive = vi.fn(async (id: string) => (id === 'thread-1'
      ? { workersNotArchived: [
          { threadId: 'worker-1', title: 'Parser review', error: 'thread is preparing a fork' },
          { threadId: 'remote-1', title: 'Kernel build', computer: 'Laptop', computerId: 'c-laptop', pending: true, error: 'Laptop did not answer in time; it is archived there once that computer answers' },
          { threadId: 'remote-2', title: 'Soak run', computer: 'Laptop', computerId: 'c-laptop', pending: true, error: 'Laptop: its thread there is still being made; it is archived there once that computer takes it' },
          { threadId: 'remote-3', title: 'Fuzz run', computer: 'Laptop', computerId: 'c-laptop', pending: true, error: 'Laptop did not answer in time; it is archived there once that computer answers' },
          { threadId: '', title: '', computer: 'Desk', computerId: 'c-desk', error: 'Desk runs an older version, so the worker there was not archived' },
        ] }
      : {}));
    setBindingMock('ArchiveThread', archive);
    const ctx = makeCtx({});
    await archiveThreadAction(ctx);
    expect(ctx.reportError).not.toHaveBeenCalled();
    expect(getToasts().map((toast) => [toast.type, toast.message, toast.action?.label ?? null])).toEqual([
      ['warning', 'Archived "Source thread", but its worker "Parser review" is still open: Thread is preparing a fork.', 'Archive again'],
      ['info', 'Archived "Source thread". Its workers "Kernel build", "Soak run", "Fuzz run" are pending on Laptop. "Kernel build", "Fuzz run": Laptop did not answer in time; it is archived there once that computer answers. "Soak run": Laptop: its thread there is still being made; it is archived there once that computer takes it.', null],
      ['warning', 'Archived "Source thread", but its worker on Desk is still open: Desk runs an older version, so the worker there was not archived.', null],
    ]);
    getToasts()[0].action?.run();
    await vi.waitFor(() => expect(archive).toHaveBeenCalledWith('worker-1'));
    expect(archive.mock.calls.map(([id]) => id)).toEqual(['thread-1', 'worker-1']);
  });

  // The archive committed and only the cleanup after it failed: the client
  // agrees with the backend (row gone, pane closed) and shows the backend's
  // sentence once, as a toast that outlives the closed pane, never as a
  // failed archive.
  it('drops the thread and toasts the cleanup failure of an archive that happened', async () => {
    const thread = makeBaseThread({ id: 'thread-1' });
    prependThread(thread);
    await buildPane(thread, [], 'pane-archived');
    const message = 'The thread was archived, but its browser pages could not all be closed.';
    setBindingMock('ArchiveThread', async () => ({ cleanupError: message }));
    const ctx = makeCtx({ id: 'thread-1' });

    await archiveThreadAction(ctx);

    expect(getThreadById('thread-1')).toBeUndefined();
    expect(getPane('pane-archived')).toBeUndefined();
    expect(ctx.reportError).not.toHaveBeenCalled();
    expect(getToasts().map((t) => [t.type, t.message])).toEqual([['error', message]]);
  });

  it('keeps the thread and reports through the caller when the archive itself failed', async () => {
    const thread = makeBaseThread({ id: 'thread-1' });
    prependThread(thread);
    await buildPane(thread, [], 'pane-kept');
    setBindingMock('ArchiveThread', async () => {
      throw new TransportError('not_found', 'The requested item no longer exists.');
    });
    const ctx = makeCtx({ id: 'thread-1' });

    await archiveThreadAction(ctx);

    expect(getThreadById('thread-1')).toBeDefined();
    expect(getPane('pane-kept')).toBeDefined();
    expect(ctx.reportError).toHaveBeenCalledTimes(1);
    expect(getToasts().filter((t) => t.type === 'error')).toEqual([]);
  });
});

describe('deleteThreadAction', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    clearThreadsForTest();
    for (const toast of getToasts()) removeToast(toast.id);
  });

  it('drops the thread and toasts work a delete that happened could not stop', async () => {
    const thread = makeBaseThread({ id: 'thread-1' });
    prependThread(thread);
    await buildPane(thread, [], 'pane-deleted');
    for (const toast of getToasts()) removeToast(toast.id);
    const message = 'The thread was deleted, but a command it started on another computer could not be cancelled and may keep running there for up to an hour.';
    setBindingMock('DeleteThreads', async () => ({ cleanupError: message }));
    const ctx = makeCtx({ id: 'thread-1' });

    await deleteThreadAction(ctx);

    expect(getThreadById('thread-1')).toBeUndefined();
    expect(getPane('pane-deleted')).toBeUndefined();
    expect(ctx.reportError).not.toHaveBeenCalled();
    expect(getToasts().map((t) => [t.type, t.message])).toEqual([['error', message]]);
  });

  // The backend deletes it with what lives under it
  // (docs/specs/standing-agents.md, "Owned workers"), and the worktree it is
  // on stays: the window stops no session, removes no worktree and reads no
  // confirmation preview to decide what goes.
  it('deletes it in one backend call and removes no worktree', async () => {
    const own = '/tmp/work-wt/own';
    const stopped = setBindingMock('StopSession', vi.fn(async () => {}));
    const removed = setBindingMock('RemoveOtherWorktree', vi.fn(async () => ({ workspace: null, reattached: [] })));
    const preview = setBindingMock('GetDeleteDependents', vi.fn(async () => ({ pastThreads: 0, helpers: 0, workers: 0 })));
    const plain = setBindingMock('DeleteThread', vi.fn(async () => ({})));
    const deleted = setBindingMock('DeleteThreads', vi.fn(async () => ({})));
    prependThread(makeCtx({ projectId: 'project-1', workspacePath: own, worktreePath: own }).thread);
    const ctx = makeCtx({ projectId: 'project-1', workspacePath: own, worktreePath: own });

    await deleteThreadAction(ctx);

    expect(ctx.reportError).not.toHaveBeenCalled();
    expect(deleted.mock.calls).toEqual([[['thread-1']]]);
    expect(getThreadById('thread-1')).toBeUndefined();
    for (const call of [stopped, removed, preview, plain]) expect(call).not.toHaveBeenCalled();
  });

  it('keeps the row and says why when the delete is refused', async () => {
    setBindingMock('DeleteThreads', async () => {
      throw new Error('thread worker-1, which works under the thread being deleted, is being transferred to another computer');
    });
    prependThread(makeCtx({}).thread);
    const ctx = makeCtx({});

    await deleteThreadAction(ctx);

    expect(getThreadById('thread-1')).toBeDefined();
    expect((ctx.reportError as ReturnType<typeof vi.fn>).mock.calls[0][0]).toMatch(/being transferred/i);
  });

  // A thread under it whose destroy failed keeps itself and the thread
  // hidden and unfinished; deleting the thread again finishes both.
  it('names a thread under it that was not deleted and deletes the thread again', async () => {
    let calls = 0;
    const remove = vi.fn(async () => (++calls === 1
      ? { workersNotDeleted: [{ threadId: 'worker-1', title: 'Parser review', error: 'the remote computer did not answer' }] }
      : {}));
    setBindingMock('DeleteThreads', remove);
    prependThread(makeCtx({}).thread);
    const ctx = makeCtx({});

    await deleteThreadAction(ctx);

    expect(ctx.reportError).not.toHaveBeenCalled();
    expect(getThreadById('thread-1')).toBeUndefined();
    const [toast] = getToasts();
    expect(toast.type).toBe('warning');
    expect(toast.message).toBe('"Source thread" is not fully deleted: the thread "Parser review" under it was not deleted: The remote computer did not answer.');
    expect(toast.action?.label).toBe('Delete again');
    toast.action?.run();
    await vi.waitFor(() => expect(remove).toHaveBeenCalledTimes(2));
    expect(remove.mock.calls[1]).toEqual([['thread-1']]);
    expect(getToasts()).toHaveLength(1);
  });

  // A delete a paired computer has not taken yet stands here and is sent
  // again until it lands; deleting again here has nothing left to delete.
  it('names a worker on another computer as pending there, and one on an older computer once, offering no delete again', async () => {
    const remove = vi.fn(async () => ({ workersNotDeleted: [
      { threadId: 'remote-1', title: 'Kernel build', computer: 'Laptop', computerId: 'c-laptop', pending: true, error: 'Laptop: refused for now: it is being transferred to another computer; it is deleted there once that computer takes it' },
      { threadId: 'remote-2', title: 'Soak run', computer: 'Desk', computerId: 'c-desk', error: 'Desk runs an older version, so the worker there was not deleted' },
      // A second computer that shares the first one's name is its own toast.
      { threadId: 'remote-3', title: 'Fuzz run', computer: 'Laptop', computerId: 'c-laptop-2', pending: true, error: 'Laptop did not answer in time; it is deleted there once that computer answers' },
    ] }));
    setBindingMock('DeleteThreads', remove);
    const ctx = makeCtx({});
    await deleteThreadAction(ctx);
    expect(ctx.reportError).not.toHaveBeenCalled();
    expect(getToasts().map((toast) => [toast.type, toast.message, toast.action?.label ?? null])).toEqual([
      ['info', 'Deleted "Source thread". Its worker "Kernel build" is pending on Laptop. Laptop: refused for now: it is being transferred to another computer; it is deleted there once that computer takes it.', null],
      ['info', 'Deleted "Source thread". Its worker "Fuzz run" is pending on Laptop. Laptop did not answer in time; it is deleted there once that computer answers.', null],
      ['warning', 'Deleted "Source thread", but its worker "Soak run" on Desk stays: Desk runs an older version, so the worker there was not deleted.', null],
    ]);
    expect(remove).toHaveBeenCalledTimes(1);
  });

  // Threads selected together are one delete per computer they are on.
  it('deletes a selection as one call per computer', async () => {
    stageBackend();
    try {
      noteThread('thread-l1', 'laptop');
      noteThread('thread-l2', 'laptop');
      noteThread('thread-h', HOME_BACKEND);
      const calls: Array<{ backend: string | null; ids: string[] }> = [];
      setBindingMock('DeleteThreads', async (ids: string[]) => {
        calls.push({ backend: getPinnedBackend(), ids });
        return {};
      });
      const reportError = vi.fn();
      const complete = await deleteThreadsAction([
        { id: 'thread-l1', title: 'One' }, { id: 'thread-h', title: 'Here' }, { id: 'thread-l2', title: 'Two' },
      ], reportError);

      expect(complete).toBe(true);
      expect(reportError).not.toHaveBeenCalled();
      expect(calls).toHaveLength(2);
      expect(calls.find((call) => call.backend === 'laptop')?.ids).toEqual(['thread-l1', 'thread-l2']);
      expect(calls.find((call) => call.backend !== 'laptop')?.ids).toEqual(['thread-h']);
    } finally {
      resetStagedBackends();
      __resetEntityIndexForTest();
    }
  });

  it('deletes on the computer the thread lives on', async () => {
    stageBackend();
    try {
      noteThread('thread-1', 'laptop');
      const targets: Array<string | null> = [];
      setBindingMock('DeleteThreads', async () => {
        targets.push(getPinnedBackend());
        return {};
      });
      const worktree = '/tmp/work-wt/laptop';
      const ctx = makeCtx({ projectId: 'project-laptop', workspacePath: worktree, worktreePath: worktree });

      await deleteThreadAction(ctx);

      expect(ctx.reportError).not.toHaveBeenCalled();
      expect(targets).toEqual(['laptop']);
    } finally {
      resetStagedBackends();
      __resetEntityIndexForTest();
    }
  });
});

// A thread on a computer on v0.0.19 (test/helpers/olderComputer.ts): the
// archive and delete answer null, and DeleteThreads and MarkThreadsRead are
// not there. Each action goes through the method that release has and
// reports nothing when it went.
describe('thread actions on a computer on v0.0.19', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    clearThreadsForTest();
    for (const toast of getToasts()) removeToast(toast.id);
    stageBackend();
    noteThread('thread-1', 'laptop');
    answerAsV0019();
  });
  afterEach(() => {
    resetStagedBackends();
    __resetEntityIndexForTest();
  });

  it('archives it there and says nothing more', async () => {
    prependThread(makeCtx({}).thread);
    const ctx = makeCtx({});
    await archiveThreadAction(ctx);
    expect(ctx.reportError).not.toHaveBeenCalled();
    expect(getBindingMock('ArchiveThread')).toHaveBeenCalledWith('thread-1');
    expect(getThreadById('thread-1')).toBeUndefined();
    expect(getToasts()).toEqual([]);
  });

  it('deletes it there with DeleteThread and says nothing more', async () => {
    const pins: (string | null)[] = [];
    setBindingMock('DeleteThread', async () => { pins.push(getPinnedBackend()); return null; });
    prependThread(makeCtx({}).thread);
    const ctx = makeCtx({});
    await deleteThreadAction(ctx);
    expect(ctx.reportError).not.toHaveBeenCalled();
    expect(pins).toEqual(['laptop']);
    expect(getThreadById('thread-1')).toBeUndefined();
    expect(getToasts()).toEqual([]);
  });

  it('keeps the row and says why when that release refuses the delete', async () => {
    setBindingMock('DeleteThread', async () => { throw new Error('thread is busy'); });
    prependThread(makeCtx({}).thread);
    const ctx = makeCtx({});
    await deleteThreadAction(ctx);
    expect(getThreadById('thread-1')).toBeDefined();
    expect((ctx.reportError as ReturnType<typeof vi.fn>).mock.calls.map(([message]) => message)).toEqual(['Thread is busy.']);
  });

  it('deletes a selection one thread at a time, keeping the deleted ones gone and naming each it could not delete', async () => {
    noteThread('thread-2', 'laptop');
    noteThread('thread-3', 'laptop');
    setBindingMock('DeleteThread', async (id: string) => {
      if (id === 'thread-2') throw new Error('thread is busy');
      return null;
    });
    for (const id of ['thread-1', 'thread-2', 'thread-3']) prependThread(makeCtx({ id }).thread);
    const reportError = vi.fn();
    const complete = await deleteThreadsAction([
      { id: 'thread-1', title: 'One' }, { id: 'thread-2', title: 'Two' }, { id: 'thread-3', title: 'Three' },
    ], reportError);
    expect(complete).toBe(false);
    expect(getBindingMock('DeleteThread')!.mock.calls).toEqual([['thread-1'], ['thread-2'], ['thread-3']]);
    expect(getThreadById('thread-1')).toBeUndefined();
    expect(getThreadById('thread-2')).toBeDefined();
    expect(getThreadById('thread-3')).toBeUndefined();
    expect(reportError.mock.calls.map(([message]) => message)).toEqual(['Could not delete "Two": Thread is busy.']);
  });

  it('marks a group read there one thread at a time', async () => {
    noteThread('thread-2', 'laptop');
    const unread = (id: string) => ({ ...makeCtx({ id }).thread, latestTurnCompletedAt: 50, lastReadAt: 10 });
    for (const id of ['thread-1', 'thread-2']) prependThread(unread(id));
    await markAllReadAction([unread('thread-1'), unread('thread-2')]);
    expect(getBindingMock('MarkThreadRead')!.mock.calls).toEqual([['thread-1'], ['thread-2']]);
    expect(getToasts()).toEqual([]);
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

  it('keeps the successor row a frame brought during the handoff over the older reply', async () => {
    const reply = makeBaseThread({ id: 'thread-2', title: 'Ops', agentId: 'agent-1' });
    setBindingMock('StartNextAssistantThread', async () => {
      applyThreadUpdated({ action: 'listed', thread: { ...reply, title: 'Ops renamed meanwhile' } });
      return reply;
    });
    const ctx = { ...makeCtx({ agentId: 'agent-1' }), isActive: true };
    await nextAssistantThreadAction(ctx);
    expect(getThreadById('thread-2')?.title).toBe('Ops renamed meanwhile');
    expect((ctx.switchPane as ReturnType<typeof vi.fn>).mock.calls[0][0].title).toBe('Ops renamed meanwhile');
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
});

// The organize thread tool pins and unpins concurrently with the person: a
// whole-row frame that lands during the call is newer than its reply.
describe('pin replies', () => {
  beforeEach(() => {
    resetBindingMocks();
    clearThreadsForTest();
  });

  it('keeps an unpin frame that arrived during PinThread', async () => {
    const ctx = makeCtx({ id: 'pin-raced' });
    prependThread(ctx.thread);
    setBindingMock('PinThread', async () => {
      applyThreadUpdated({ action: 'full', thread: { ...ctx.thread, pinnedAt: undefined, title: 'Unpinned by the tool' } });
      return { ...ctx.thread, pinnedAt: 500, pinGroup: 0 };
    });
    await pinThreadAction(ctx);
    expect(getThreadById('pin-raced')?.pinnedAt).toBeUndefined();
    expect(getThreadById('pin-raced')?.title).toBe('Unpinned by the tool');
  });

  it('keeps a pin frame that arrived during UnpinThread or SetThreadPinGroup', async () => {
    const ctx = makeCtx({ id: 'unpin-raced', pinnedAt: 100, pinGroup: 0 });
    prependThread(ctx.thread);
    setBindingMock('UnpinThread', async () => {
      applyThreadUpdated({ action: 'full', thread: { ...ctx.thread, pinnedAt: 700, pinGroup: 1 } });
      return { ...ctx.thread, pinnedAt: undefined, pinGroup: undefined };
    });
    await unpinThreadAction(ctx);
    expect(getThreadById('unpin-raced')?.pinnedAt).toBe(700);
    setBindingMock('SetThreadPinGroup', async () => {
      applyThreadUpdated({ action: 'full', thread: { ...ctx.thread, pinnedAt: 800, pinGroup: 0 } });
      return { ...ctx.thread, pinnedAt: 700, pinGroup: 1 };
    });
    await setThreadPinGroupAction(ctx, 1);
    expect(getThreadById('unpin-raced')?.pinnedAt).toBe(800);
    expect(getThreadById('unpin-raced')?.pinGroup).toBe(0);
  });

  it('applies the reply when no frame arrived during the call', async () => {
    const ctx = makeCtx({ id: 'pin-quiet' });
    prependThread(ctx.thread);
    setBindingMock('PinThread', async () => ({ ...ctx.thread, pinnedAt: 500, pinGroup: 0 }));
    await pinThreadAction(ctx);
    expect(getThreadById('pin-quiet')?.pinnedAt).toBe(500);
  });
});
