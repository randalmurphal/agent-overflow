import { beforeEach, describe, expect, it, vi } from 'vitest';

import {
  applyWorktreeIntentNow,
  materializeWorktreeIntentOnThread,
  type PaneForIntentApply,
  prepareThreadWorktreeIntent,
  resetWorktreeIntentMaterializeForTest,
} from './worktreeIntentMaterialize';
import {
  enterCreateBranchMode,
  isWorktreeIntentApplying,
  migrateWorktreeIntent,
  resetForTest as resetWorktreeIntent,
  setNewBranchName,
  setThreadEnvMode,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';
import { recentBranchSelections } from './branchMru';
import { resetAppStorageForTest } from './appStorage';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../test/mocks/bindings-app';
import { makeThread } from '../../test/helpers/chat';
import type { Thread } from '../types/models';

/**
 * The narrow pane surface the apply/bind paths take. A real ThreadPane would
 * drag the whole switch pipeline into a test about which RPC gets called.
 */
function fakePane(thread: Thread, hasDraftPlaceholder = false) {
  const stamp = vi.fn(
    (_workspace: { workspacePath: string; worktreePath?: string; branch?: string }) => true,
  );
  // Mutable, because the re-keying tests move the pane onto the materialized
  // row mid-RPC — which is exactly what ensureMaterializedThread does.
  const pane: {
    thread: Thread | null;
    hasDraftPlaceholder: boolean;
    applyDraftPlaceholderWorkspace: typeof stamp;
  } & PaneForIntentApply = {
    thread,
    hasDraftPlaceholder,
    applyDraftPlaceholderWorkspace: stamp,
  };
  return pane;
}

function threadFields(overrides: Partial<Thread> = {}): Thread {
  return makeThread({
    id: 'thread-mat',
    branch: 'main',
    workspacePath: '/repo',
    projectPath: '/repo',
    projectId: 'project-1',
    ...overrides,
  });
}

/**
 * A materialized DRAFT row — a real row with no items yet. It owns the
 * project-scoped route (the empty-draft cleanup can still delete it, so the
 * thread-scoped RPCs would race that delete) and binds at send time.
 */
function draftFields(overrides: Partial<Thread> = {}): Thread {
  return threadFields({ isDraft: true, ...overrides });
}

/** A DRAFT row with `new-worktree` + attach-existing staged. */
function stagedAttach(overrides: Partial<Thread> = {}): Thread {
  const thread = draftFields(overrides);
  setThreadEnvMode(thread, 'new-worktree');
  return thread;
}

describe('applyWorktreeIntentNow (project-scoped, no thread row)', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    resetWorktreeIntentMaterializeForTest();
    resetAppStorageForTest();
  });

  it('applies a draft placeholder without materializing a thread', async () => {
    const placeholder = stagedAttach({ id: 'draft:main:project-1:chat:1', isDraft: true });
    const pane = fakePane(placeholder, true);
    setBindingMock('AttachProjectWorktree', async () => ({
      worktreePath: '/wt/main',
      branch: 'main',
    }));

    const applied = await applyWorktreeIntentNow(pane);

    expect(applied).toEqual({ worktreePath: '/wt/main', branch: 'main' });
    expect(getBindingMock('AttachProjectWorktree')!.mock.calls[0]).toEqual([
      'project-1',
      'main',
    ]);
    // Nothing thread-shaped may run: the placeholder has no row, and creating
    // one is exactly the race this rework removed.
    expect(getBindingMock('CreateThread')).toBeUndefined();
    expect(getBindingMock('AttachThreadWorktree')).toBeUndefined();
    expect(getBindingMock('UpdateThreadWorkspace')).toBeUndefined();
    // The placeholder is synthetic, so stamping it is what moves the whole
    // pane (workspace strip, git status, terminal cwd) onto the worktree.
    expect(pane.applyDraftPlaceholderWorkspace.mock.calls[0][0]).toEqual({
      workspacePath: '/wt/main',
      worktreePath: '/wt/main',
      branch: 'main',
    });
    // A placeholder must never enter the empty-draft cleanup's active-work
    // set: there is no row for that cleanup to delete.
    expect(isWorktreeIntentApplying(placeholder.id)).toBe(false);
  });

  it('creates a new branch + worktree with the resolved base and carry flag', async () => {
    const thread = draftFields({ id: 'thread-new-wt' });
    setThreadEnvMode(thread, 'new-worktree');
    enterCreateBranchMode(thread, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchName(thread, 'feat/x');
    setBindingMock('PrepareProjectWorktree', async () => ({
      worktreePath: '/wt/feat-x',
      branch: 'feat/x',
    }));

    const applied = await applyWorktreeIntentNow(fakePane(thread));

    expect(getBindingMock('PrepareProjectWorktree')!.mock.calls[0]).toEqual([
      'project-1',
      'main',
      'feat/x',
      false,
      // The carry stash and the base comparison belong in the pane's own
      // checkout, which is not always the project root.
      '/repo',
    ]);
    expect(applied?.branch).toBe('feat/x');
    // The materialized branch is the user's working branch now.
    expect(recentBranchSelections('project-1')).toEqual(['feat/x']);
  });

  it('creates a branch in place when the workspace mode stays local', async () => {
    const thread = draftFields({ id: 'thread-local-branch' });
    enterCreateBranchMode(thread, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchName(thread, 'feat/inplace');
    setBindingMock('CreateProjectBranch', async () => ({ worktreePath: '', branch: 'feat/inplace' }));

    const applied = await applyWorktreeIntentNow(fakePane(thread));

    expect(getBindingMock('CreateProjectBranch')!.mock.calls[0]).toEqual([
      'project-1',
      'feat/inplace',
      'main',
      false,
      '/repo',
    ]);
    // No worktree came back: the checkout happened where the pane already was.
    expect(applied).toEqual({ worktreePath: '', branch: 'feat/inplace' });
  });

  it('is idempotent — a second confirm returns the applied result without a second cut', async () => {
    const thread = stagedAttach({ id: 'thread-twice' });
    const pane = fakePane(thread);
    setBindingMock('AttachProjectWorktree', async () => ({
      worktreePath: '/wt/main',
      branch: 'main',
    }));

    const first = await applyWorktreeIntentNow(pane);
    const second = await applyWorktreeIntentNow(pane);

    expect(second).toEqual(first);
    expect(getBindingMock('AttachProjectWorktree')!.mock.calls.length).toBe(1);
    expect(worktreeIntentForThread(thread).applied).toEqual(first);
  });

  it('coalesces concurrent applies into one backend call', async () => {
    const thread = stagedAttach({ id: 'draft:main:project-1:chat:3', isDraft: true });
    const pane = fakePane(thread, true);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    setBindingMock('AttachProjectWorktree', async () => {
      await gate;
      return { worktreePath: '/wt/main', branch: 'main' };
    });

    // The confirm button and a send racing each other.
    const first = applyWorktreeIntentNow(pane);
    const second = prepareThreadWorktreeIntent({ pane });
    release();
    const [firstResult, secondResult] = await Promise.all([first, second]);

    expect(getBindingMock('AttachProjectWorktree')!.mock.calls.length).toBe(1);
    expect(firstResult).toEqual({ worktreePath: '/wt/main', branch: 'main' });
    expect(secondResult).toEqual(firstResult);
  });

  it('keeps the staged intent unapplied when the backend refuses', async () => {
    const thread = stagedAttach({ id: 'thread-refused' });
    const pane = fakePane(thread);
    setBindingMock('AttachProjectWorktree', async () => {
      throw new Error('worktree create failed: branch busy');
    });

    await expect(applyWorktreeIntentNow(pane)).rejects.toThrow('branch busy');
    // A failed attempt must not stay cached as the in-flight winner.
    await expect(applyWorktreeIntentNow(pane)).rejects.toThrow('branch busy');
    expect(getBindingMock('AttachProjectWorktree')!.mock.calls.length).toBe(2);
    expect(worktreeIntentForThread(thread).mode).toBe('new-worktree');
    expect(worktreeIntentForThread(thread).applied).toBeNull();
  });

  it('does nothing when there is nothing staged', async () => {
    const thread = draftFields({ id: 'thread-unstaged' });
    expect(await applyWorktreeIntentNow(fakePane(thread))).toBeNull();
  });
});

describe('prepareThreadWorktreeIntent (the send path)', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    resetWorktreeIntentMaterializeForTest();
    resetAppStorageForTest();
  });

  it('applies a staged-but-unapplied intent and binds it to the row', async () => {
    const thread = stagedAttach({ id: 'thread-send' });
    setBindingMock('AttachProjectWorktree', async () => ({
      worktreePath: '/wt/main',
      branch: 'main',
    }));
    setBindingMock('UpdateThreadWorkspace', async () => makeThread({
      ...thread,
      workspacePath: '/wt/main',
      worktreePath: '/wt/main',
    }));

    await prepareThreadWorktreeIntent({ pane: fakePane(thread) });

    expect(getBindingMock('AttachProjectWorktree')!.mock.calls.length).toBe(1);
    expect(getBindingMock('UpdateThreadWorkspace')!.mock.calls[0]).toEqual([
      'thread-send',
      '/wt/main',
    ]);
    // Bound: nothing is left staged for the next send to re-apply.
    expect(worktreeIntentForThread(thread).mode).toBe('local');
  });

  it('reuses an apply the confirm button already ran', async () => {
    const thread = stagedAttach({ id: 'thread-preapplied' });
    const pane = fakePane(thread);
    setBindingMock('AttachProjectWorktree', async () => ({
      worktreePath: '/wt/main',
      branch: 'main',
    }));
    setBindingMock('UpdateThreadWorkspace', async () => makeThread({
      ...thread,
      workspacePath: '/wt/main',
    }));

    await applyWorktreeIntentNow(pane);
    await prepareThreadWorktreeIntent({ pane });

    expect(getBindingMock('AttachProjectWorktree')!.mock.calls.length).toBe(1);
    expect(getBindingMock('UpdateThreadWorkspace')!.mock.calls.length).toBe(1);
  });

  it('flags the row as applying for the whole bind, and clears it either way', async () => {
    // The empty-draft cleanup reads this flag: deleting the row mid-RPC fails
    // the bind backend-side ("no rows in result set"). The mark has to be set
    // before the first await, not after it.
    const thread = stagedAttach({ id: 'thread-flag' });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    setBindingMock('AttachProjectWorktree', async () => {
      await gate;
      return { worktreePath: '/wt/main', branch: 'main' };
    });
    setBindingMock('UpdateThreadWorkspace', async () => makeThread({
      ...thread,
      workspacePath: '/wt/main',
    }));

    const run = prepareThreadWorktreeIntent({ pane: fakePane(thread) });
    expect(isWorktreeIntentApplying(thread.id)).toBe(true);
    release();
    await run;
    expect(isWorktreeIntentApplying(thread.id)).toBe(false);

    const failing = stagedAttach({ id: 'thread-flag-fail' });
    setBindingMock('AttachProjectWorktree', async () => {
      throw new Error('boom');
    });
    await expect(prepareThreadWorktreeIntent({ pane: fakePane(failing) })).rejects.toThrow('boom');
    expect(isWorktreeIntentApplying(failing.id)).toBe(false);
  });

  it('keeps the intent when the bind fails, so a retried send re-binds', async () => {
    const thread = stagedAttach({ id: 'thread-bind-fail' });
    setBindingMock('AttachProjectWorktree', async () => ({
      worktreePath: '/wt/main',
      branch: 'main',
    }));
    setBindingMock('UpdateThreadWorkspace', async () => {
      throw new Error('no rows in result set');
    });

    await expect(
      prepareThreadWorktreeIntent({ pane: fakePane(thread) }),
    ).rejects.toThrow('no rows in result set');
    // The applied workspace survives so the retry binds it rather than
    // cutting a second worktree.
    expect(worktreeIntentForThread(thread).applied).toEqual({
      worktreePath: '/wt/main',
      branch: 'main',
    });
  });

  it('binds a branch-only apply with no workspace RPC at all', async () => {
    const thread = draftFields({ id: 'thread-branch-only' });
    enterCreateBranchMode(thread, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchName(thread, 'feat/only');
    setBindingMock('CreateProjectBranch', async () => ({ worktreePath: '', branch: 'feat/only' }));

    await prepareThreadWorktreeIntent({ pane: fakePane(thread) });

    // The checkout already moved the shared workspace; the row's branch heals
    // through the backend's workspace-keyed branch persist.
    expect(getBindingMock('UpdateThreadWorkspace')).toBeUndefined();
    expect(worktreeIntentForThread(thread).creatingBranch).toBe(false);
  });

  it('stops after the apply for a draft placeholder — CreateThread carries it', async () => {
    const placeholder = stagedAttach({ id: 'draft:main:project-1:chat:2', isDraft: true });
    const pane = fakePane(placeholder, true);
    setBindingMock('AttachProjectWorktree', async () => ({
      worktreePath: '/wt/main',
      branch: 'main',
    }));

    await prepareThreadWorktreeIntent({ pane });

    expect(getBindingMock('UpdateThreadWorkspace')).toBeUndefined();
    // Still staged: ensureMaterializedThread clears it once CreateThread has
    // carried the stamped workspace through.
    expect(worktreeIntentForThread(placeholder).applied).toEqual({
      worktreePath: '/wt/main',
      branch: 'main',
    });
  });
});

describe('routing: which engine owns the row', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    resetWorktreeIntentMaterializeForTest();
    resetAppStorageForTest();
  });

  it('sends a NON-draft row through the thread-scoped RPC, carry semantics and all', async () => {
    // A thread with history, parked in a worktree. The thread-scoped engine is
    // the only one whose carry stashes from the row's own workspace AND moves
    // the row in the same call.
    const thread = threadFields({
      id: 'thread-with-history',
      isDraft: false,
      branch: 'feat/live',
      workspacePath: '/wt/live',
      worktreePath: '/wt/live',
    });
    setThreadEnvMode(thread, 'new-worktree');
    enterCreateBranchMode(thread, { workspaceDirty: true, currentBranch: 'feat/live' });
    setNewBranchName(thread, 'feat/next');
    const moved = makeThread({ ...thread, worktreePath: '/wt/next', workspacePath: '/wt/next' });
    setBindingMock('PrepareThreadWorktree', async () => moved);

    await prepareThreadWorktreeIntent({ pane: fakePane(thread) });

    // LOCAL sentinel base (dirty workspace default) → carry=true against the
    // thread's own branch.
    expect(getBindingMock('PrepareThreadWorktree')!.mock.calls[0]).toEqual([
      'thread-with-history',
      'feat/live',
      'feat/next',
      true,
    ]);
    expect(getBindingMock('PrepareProjectWorktree')).toBeUndefined();
    // The row moved as part of the call, so nothing is left pending on it.
    expect(getBindingMock('UpdateThreadWorkspace')).toBeUndefined();
    expect(worktreeIntentForThread(thread).applied).toBeNull();
    expect(worktreeIntentForThread(thread).mode).toBe('local');
  });

  it('passes a draft\'s own worktree as the project-scoped sourceWorkspace', async () => {
    // A draft that already cut one worktree and is staging a second choice.
    const draft = draftFields({
      id: 'draft-in-worktree',
      branch: 'feat/first',
      workspacePath: '/wt/first',
      worktreePath: '/wt/first',
    });
    setThreadEnvMode(draft, 'new-worktree');
    enterCreateBranchMode(draft, { workspaceDirty: true, currentBranch: 'feat/first' });
    setNewBranchName(draft, 'feat/second');
    setBindingMock('PrepareProjectWorktree', async () => ({
      worktreePath: '/wt/second',
      branch: 'feat/second',
    }));

    await applyWorktreeIntentNow(fakePane(draft));

    expect(getBindingMock('PrepareProjectWorktree')!.mock.calls[0]).toEqual([
      'project-1',
      'feat/first',
      'feat/second',
      true,
      // Not the project root: the stash has to come out of the checkout the
      // pane is actually in.
      '/wt/first',
    ]);
    expect(getBindingMock('PrepareThreadWorktree')).toBeUndefined();
  });

  it('brackets the confirm-button path on a real row before its first await', async () => {
    const draft = stagedAttach({ id: 'draft-confirm-bracket' });
    const pane = fakePane(draft);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    setBindingMock('AttachProjectWorktree', async () => {
      await gate;
      return { worktreePath: '/wt/main', branch: 'main' };
    });

    const run = applyWorktreeIntentNow(pane);
    // The empty-draft cleanup reads this. Deleting the row mid-RPC is what the
    // bracket exists to prevent, and the confirm button drives the same RPCs
    // the send path does.
    expect(isWorktreeIntentApplying(draft.id)).toBe(true);
    release();
    await run;
    expect(isWorktreeIntentApplying(draft.id)).toBe(false);
  });
});

describe('re-keying: an apply that outlives its thread id', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    resetWorktreeIntentMaterializeForTest();
    resetAppStorageForTest();
  });

  it('lands `applied` under the id the pane holds when the RPC completes', async () => {
    // Typing into a placeholder mid-apply materializes the row, which re-keys
    // the intent. Writing to the id we started with would strand the worktree:
    // invisible to the pane, and re-cut on the next send.
    const placeholder = stagedAttach({ id: 'draft:pane:project-1:chat:9', isDraft: true });
    const pane = fakePane(placeholder, true);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    setBindingMock('AttachProjectWorktree', async () => {
      await gate;
      return { worktreePath: '/wt/main', branch: 'main' };
    });

    const run = prepareThreadWorktreeIntent({ pane });
    // ensureMaterializedThread's half of the handover.
    const created = draftFields({ id: 'thread-created', workspacePath: '/repo' });
    migrateWorktreeIntent(placeholder.id, created.id);
    pane.thread = created;
    pane.hasDraftPlaceholder = false;
    release();
    await run;

    expect(worktreeIntentForThread(created).applied).toEqual({
      worktreePath: '/wt/main',
      branch: 'main',
    });
    expect(worktreeIntentForThread(placeholder).applied).toBeNull();
  });

  it('finds an in-flight apply under the new id, so a second send does not re-cut', async () => {
    const placeholder = stagedAttach({ id: 'draft:pane:project-1:chat:10', isDraft: true });
    const pane = fakePane(placeholder, true);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    setBindingMock('AttachProjectWorktree', async () => {
      await gate;
      return { worktreePath: '/wt/main', branch: 'main' };
    });
    setBindingMock('UpdateThreadWorkspace', async () => makeThread({
      id: 'thread-created',
      workspacePath: '/wt/main',
    }));

    const first = prepareThreadWorktreeIntent({ pane });
    const created = draftFields({ id: 'thread-created', workspacePath: '/repo' });
    migrateWorktreeIntent(placeholder.id, created.id);
    pane.thread = created;
    pane.hasDraftPlaceholder = false;

    // A send arriving under the NEW id while the first apply is still in the
    // air must join it, not start a second cut.
    const second = prepareThreadWorktreeIntent({ pane });
    release();
    await Promise.all([first, second]);

    expect(getBindingMock('AttachProjectWorktree')!.mock.calls.length).toBe(1);
  });
});

describe('materializeWorktreeIntentOnThread (thread-scoped, plan-implementation flow)', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    resetWorktreeIntentMaterializeForTest();
    resetAppStorageForTest();
  });

  it('records the materialized branch in the project MRU on success', async () => {
    const thread = threadFields({ id: 'thread-mru' });
    const updated = makeThread({ ...thread, branch: 'feat/new' });
    setBindingMock('GitCreateBranchFrom', async () => updated);

    const result = await materializeWorktreeIntentOnThread({
      targetThread: thread,
      intent: {
        mode: 'local',
        creatingBranch: true,
        newBranchName: 'feat/new',
        newBranchBase: 'main',
        attachBranch: '',
        applied: null,
      },
    });

    expect(result?.branch).toBe('feat/new');
    expect(recentBranchSelections('project-1')).toEqual(['feat/new']);
  });

  it('brackets the row with the applying flag from its first statement', async () => {
    const thread = threadFields({ id: 'thread-scoped-flag' });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    setBindingMock('AttachThreadWorktree', async () => {
      await gate;
      return makeThread({ ...thread, worktreePath: '/wt/main' });
    });

    const run = materializeWorktreeIntentOnThread({
      targetThread: thread,
      intent: {
        mode: 'new-worktree',
        creatingBranch: false,
        newBranchName: '',
        newBranchBase: '',
        attachBranch: 'main',
        applied: null,
      },
    });
    expect(isWorktreeIntentApplying(thread.id)).toBe(true);
    release();
    await run;
    expect(isWorktreeIntentApplying(thread.id)).toBe(false);
  });
});
