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
  setNewBranchBase,
  setAttachBranch,
  setNewBranchName,
  setThreadEnvMode,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';
import { __resetGitStatusStoreForTest, __seedGitStatusForTest } from './gitStatusStore.svelte';
import { getToasts, resetToastsForTest } from './toast.svelte';
import type { GitStatus } from '../types/git';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../test/mocks/bindings-app';
import { makeThread } from '../../test/helpers/chat';
import { applyThreadUpdated } from './eventsThreadRows';
import type { Thread } from '../types/models';

function thread(overrides: Partial<Thread> = {}): Thread {
  return makeThread({
    id: 'thread-1',
    isDraft: true,
    branch: 'main',
    workspacePath: '/repo',
    projectPath: '/repo',
    projectId: 'project-1',
    ...overrides,
  });
}

function detachedStatus(overrides: Partial<GitStatus>): GitStatus {
  return {
    isRepo: true, branch: '', isDefaultBranch: false, hasChanges: false,
    insertions: 0, deletions: 0, fileCount: 0, hasUpstream: false,
    aheadCount: 0, behindCount: 0, hasOriginRemote: true,
    headCommit: 'abcdef0123456789', ...overrides,
  };
}

function moved(source: Thread, overrides: Partial<Thread> = {}): Thread {
  return {
    ...source,
    workspacePath: '/wt/feature',
    worktreePath: '/wt/feature',
    branch: 'feature',
    ...overrides,
  };
}

function paneFor(current: Thread): PaneForIntentApply & {
  thread: Thread | null;
  hasDraftPlaceholder: boolean;
  ensureMaterializedThread: ReturnType<typeof vi.fn>;
} {
  return {
    thread: current,
    hasDraftPlaceholder: false,
    ensureMaterializedThread: vi.fn(async () => current.id),
  };
}

function placeholderPane(placeholder: Thread, created: Thread | null) {
  const pane = paneFor(placeholder);
  pane.hasDraftPlaceholder = true;
  pane.ensureMaterializedThread = vi.fn(async () => {
    if (!created) return null;
    migrateWorktreeIntent(placeholder.id, created.id);
    pane.thread = created;
    pane.hasDraftPlaceholder = false;
    return created.id;
  });
  return pane;
}

describe('worktree intent materialization', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    resetWorktreeIntentMaterializeForTest();
    __resetGitStatusStoreForTest();
    resetToastsForTest();
  });

  it('materializes a placeholder before applying its worktree intent', async () => {
    const placeholder = thread({ id: 'draft:pane:project-1:chat:1' });
    setThreadEnvMode(placeholder, 'new-worktree');
    const created = thread();
    const pane = placeholderPane(placeholder, created);
    const attach = setBindingMock('AttachThreadWorktree', async () => moved(created));

    const applied = await applyWorktreeIntentNow(pane);

    expect(pane.ensureMaterializedThread).toHaveBeenCalledTimes(1);
    expect(attach).toHaveBeenCalledWith('thread-1', 'main');
    expect(applied).toEqual({ worktreePath: '/wt/feature', branch: 'feature' });
    expect(worktreeIntentForThread(created).mode).toBe('local');
  });

  it('does not mutate git when placeholder materialization fails', async () => {
    const placeholder = thread({ id: 'draft:pane:project-1:chat:2' });
    setThreadEnvMode(placeholder, 'new-worktree');
    const pane = placeholderPane(placeholder, null);

    await expect(applyWorktreeIntentNow(pane)).resolves.toBeNull();

    expect(getBindingMock('AttachThreadWorktree')).toBeUndefined();
    expect(worktreeIntentForThread(placeholder).mode).toBe('new-worktree');
  });

  it('does not retarget the intent if the pane switches during materialization', async () => {
    const placeholder = thread({ id: 'draft:pane:project-1:chat:3' });
    const created = thread({ id: 'created-row' });
    const replacement = thread({ id: 'other-row' });
    setThreadEnvMode(placeholder, 'new-worktree');
    const pane = placeholderPane(placeholder, created);
    pane.ensureMaterializedThread = vi.fn(async () => {
      migrateWorktreeIntent(placeholder.id, created.id);
      pane.thread = replacement;
      pane.hasDraftPlaceholder = false;
      return created.id;
    });
    const attach = setBindingMock('AttachThreadWorktree', async () => moved(replacement));

    await expect(applyWorktreeIntentNow(pane)).resolves.toBeNull();

    expect(attach).not.toHaveBeenCalled();
    expect(worktreeIntentForThread(created).mode).toBe('new-worktree');
  });

  it('attaches an existing branch and clears the staged intent', async () => {
    const source = thread();
    setThreadEnvMode(source, 'new-worktree');
    const attach = setBindingMock('AttachThreadWorktree', async () => moved(source));

    const applied = await applyWorktreeIntentNow(paneFor(source));

    expect(attach).toHaveBeenCalledWith('thread-1', 'main');
    expect(applied?.worktreePath).toBe('/wt/feature');
    expect(worktreeIntentForThread(source).mode).toBe('local');
  });

  // The backend replies to CreateThread before its `listed` echo leaves, so
  // on a slow client the echo can land during the attach call. It carries
  // the row as created, which is older than the attach reply.
  it('reports the bound workspace when the creation echo lands during the call', async () => {
    const source = thread();
    setThreadEnvMode(source, 'new-worktree');
    setBindingMock('AttachThreadWorktree', async () => {
      applyThreadUpdated({ action: 'listed', thread: source });
      return moved(source);
    });

    const applied = await applyWorktreeIntentNow(paneFor(source));

    expect(applied).toEqual({ worktreePath: '/wt/feature', branch: 'feature' });
  });

  it('returns the bound row to an explicit target caller when a stale echo lands', async () => {
    const target = thread({ id: 'child' });
    setThreadEnvMode(target, 'new-worktree');
    setBindingMock('AttachThreadWorktree', async () => {
      applyThreadUpdated({ action: 'listed', thread: target });
      return moved(target);
    });

    const materialized = await materializeWorktreeIntentOnThread({
      targetThread: target,
      intent: worktreeIntentForThread(target),
      clearIntentOnSuccess: false,
    });

    expect(materialized?.worktreePath).toBe('/wt/feature');
  });

  it('creates a new worktree branch with the selected base and carry flag', async () => {
    const source = thread({ branch: 'feature/base' });
    setThreadEnvMode(source, 'new-worktree');
    enterCreateBranchMode(source, { workspaceDirty: true, currentRef: 'feature/base' });
    setNewBranchName(source, 'feature/new');
    const prepare = setBindingMock('PrepareThreadWorktree', async () =>
      moved(source, { branch: 'feature/new' }),
    );

    await applyWorktreeIntentNow(paneFor(source));

    expect(prepare).toHaveBeenCalledWith(
      'thread-1',
      'feature/base',
      'feature/new',
      true,
    );
    expect(worktreeIntentForThread(source).mode).toBe('local');
  });

  it('creates a local branch through the thread-scoped checkout', async () => {
    const source = thread();
    enterCreateBranchMode(source, { workspaceDirty: false, currentRef: 'main' });
    setNewBranchName(source, 'feature/local');
    setNewBranchBase(source, 'develop');
    const create = setBindingMock('GitCreateBranchFrom', async () => ({
      workspacePath: '/repo',
      worktreePath: '',
      branch: 'feature/local',
    }));
    // The branch write lands on the row through the backend's thread:updated
    // broadcast; this path re-reads it so the pane paints the new branch
    // without waiting for the event.
    const getThread = setBindingMock('GetThread', async () =>
      moved(source, { workspacePath: '/repo', worktreePath: '', branch: 'feature/local' }),
    );

    await applyWorktreeIntentNow(paneFor(source));

    expect(create).toHaveBeenCalledWith(
      { projectId: 'project-1', workspacePath: '/repo' },
      'feature/local',
      'develop',
      false,
    );
    expect(getThread).toHaveBeenCalledWith('thread-1');
    expect(worktreeIntentForThread(source).creatingBranch).toBe(false);
  });

  it('sends a staged tag ref to every workspace binding', async () => {
    const attachSource = thread({ id: 'attach' });
    setThreadEnvMode(attachSource, 'new-worktree');
    setAttachBranch(attachSource, 'refs/tags/v2.4.0');
    const attach = setBindingMock('AttachThreadWorktree', async () => moved(attachSource, { branch: '' }));
    await applyWorktreeIntentNow(paneFor(attachSource));
    expect(attach).toHaveBeenCalledWith('attach', 'refs/tags/v2.4.0');

    const prepareSource = thread({ id: 'prepare' });
    setThreadEnvMode(prepareSource, 'new-worktree');
    enterCreateBranchMode(prepareSource, { workspaceDirty: false, currentRef: 'main' });
    setNewBranchName(prepareSource, 'feature/from-tag');
    setNewBranchBase(prepareSource, 'refs/tags/v2.4.0');
    const prepare = setBindingMock('PrepareThreadWorktree', async () => moved(prepareSource));
    await applyWorktreeIntentNow(paneFor(prepareSource));
    expect(prepare).toHaveBeenCalledWith('prepare', 'refs/tags/v2.4.0', 'feature/from-tag', false);

    const localSource = thread({ id: 'local' });
    enterCreateBranchMode(localSource, { workspaceDirty: false, currentRef: 'main' });
    setNewBranchName(localSource, 'feature/local-tag');
    setNewBranchBase(localSource, 'refs/tags/v2.4.0');
    const create = setBindingMock('GitCreateBranchFrom', async () => ({
      workspacePath: '/repo', worktreePath: '', branch: 'feature/local-tag',
    }));
    setBindingMock('GetThread', async () => ({ ...localSource, branch: 'feature/local-tag' }));
    await applyWorktreeIntentNow(paneFor(localSource));
    expect(create).toHaveBeenCalledWith(
      { projectId: 'project-1', workspacePath: '/repo' },
      'feature/local-tag',
      'refs/tags/v2.4.0',
      false,
    );
  });

  it('resolves the current checkout to the tag at a detached HEAD', async () => {
    __seedGitStatusForTest('/repo', detachedStatus({ tag: 'v2.4.0' }));
    setBindingMock('UpdateThreadBranch', async () => []);

    // Attaching with nothing staged attaches the current checkout.
    const attachSource = thread({ id: 'attach', branch: '' });
    setThreadEnvMode(attachSource, 'new-worktree');
    const attach = setBindingMock('AttachThreadWorktree', async () => moved(attachSource, { branch: '' }));
    await applyWorktreeIntentNow(paneFor(attachSource));
    expect(attach).toHaveBeenCalledWith('attach', 'refs/tags/v2.4.0');

    // Carrying changes branches from the current ref, the only base carry allows.
    const carrySource = thread({ id: 'carry', branch: '' });
    setThreadEnvMode(carrySource, 'new-worktree');
    enterCreateBranchMode(carrySource, { workspaceDirty: true, currentRef: 'refs/tags/v2.4.0' });
    setNewBranchName(carrySource, 'feature/carry');
    const prepare = setBindingMock('PrepareThreadWorktree', async () => moved(carrySource));
    await applyWorktreeIntentNow(paneFor(carrySource));
    expect(prepare).toHaveBeenCalledWith('carry', 'refs/tags/v2.4.0', 'feature/carry', true);
  });

  it('prefers the live checkout over a stale cached branch', async () => {
    __seedGitStatusForTest('/repo', detachedStatus({ tag: 'v3' }));
    setBindingMock('UpdateThreadBranch', async () => []);
    const source = thread({ branch: 'main' });
    setThreadEnvMode(source, 'new-worktree');
    const attach = setBindingMock('AttachThreadWorktree', async () => moved(source, { branch: '' }));
    await applyWorktreeIntentNow(paneFor(source));
    expect(attach).toHaveBeenCalledWith('thread-1', 'refs/tags/v3');
  });

  it('attaches a branch whose name starts with refs/ by its branch ref', async () => {
    const source = thread({ branch: 'refs/tags/v1.0' });
    setThreadEnvMode(source, 'new-worktree');
    const attach = setBindingMock('AttachThreadWorktree', async () => moved(source));
    await applyWorktreeIntentNow(paneFor(source));
    expect(attach).toHaveBeenCalledWith('thread-1', 'refs/heads/refs/tags/v1.0');
  });

  it('keeps an empty base for a detached HEAD at no tag and warns about commits left behind', async () => {
    __seedGitStatusForTest('/repo', detachedStatus({ headCommit: 'abcdef0123456789' }));
    setBindingMock('UpdateThreadBranch', async () => []);
    const source = thread({ branch: '' });
    enterCreateBranchMode(source, { workspaceDirty: true, currentRef: '' });
    setNewBranchName(source, 'rescue');
    const create = setBindingMock('GitCreateBranchFrom', async () => ({
      workspacePath: '/repo', worktreePath: '', branch: 'rescue',
      leftBehind: { count: 2, commit: 'abcdef0123456789' },
    }));
    setBindingMock('GetThread', async () => ({ ...source, branch: 'rescue' }));

    await applyWorktreeIntentNow(paneFor(source));

    expect(create).toHaveBeenCalledWith(
      { projectId: 'project-1', workspacePath: '/repo' }, 'rescue', '', true,
    );
    expect(getToasts()).toEqual([
      expect.objectContaining({ type: 'warning', message: expect.stringContaining('abcdef01') }),
    ]);
  });

  // A branch intent staged against a thread that names no project is a bug
  // upstream, not a user situation: the picker cannot stage one. It must
  // surface as an error the caller reports, never as a click that silently
  // does nothing.
  it('throws instead of no-oping when the target thread names no workspace', async () => {
    const source = thread({ projectId: undefined });
    enterCreateBranchMode(source, { workspaceDirty: false, currentRef: 'main' });
    setNewBranchName(source, 'feature/local');
    const create = setBindingMock('GitCreateBranchFrom', async () => ({
      workspacePath: '/repo',
      worktreePath: '',
      branch: 'feature/local',
    }));

    await expect(applyWorktreeIntentNow(paneFor(source))).rejects.toThrow(
      /thread-1: it names no project workspace/,
    );
    expect(create).not.toHaveBeenCalled();
    // The applying flag must not stay stuck on after the throw.
    expect(isWorktreeIntentApplying(source.id)).toBe(false);
  });

  it('coalesces confirm and send onto one backend mutation', async () => {
    const source = thread();
    const pane = paneFor(source);
    setThreadEnvMode(source, 'new-worktree');
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const attach = setBindingMock('AttachThreadWorktree', async () => {
      await gate;
      return moved(source);
    });

    const confirm = applyWorktreeIntentNow(pane);
    const send = prepareThreadWorktreeIntent({ pane });
    release();
    await Promise.all([confirm, send]);

    expect(attach).toHaveBeenCalledTimes(1);
  });

  it('marks the row applying synchronously and clears it after failure', async () => {
    const source = thread();
    setThreadEnvMode(source, 'new-worktree');
    let reject!: (error: Error) => void;
    setBindingMock('AttachThreadWorktree', () => new Promise((_resolve, rejectPromise) => {
      reject = rejectPromise;
    }));

    const run = prepareThreadWorktreeIntent({ pane: paneFor(source) });
    expect(isWorktreeIntentApplying(source.id)).toBe(true);
    reject(new Error('branch busy'));
    await expect(run).rejects.toThrow('branch busy');
    expect(isWorktreeIntentApplying(source.id)).toBe(false);
    expect(worktreeIntentForThread(source).mode).toBe('new-worktree');
  });

  it('retries after a failed mutation instead of caching the rejection', async () => {
    const source = thread();
    setThreadEnvMode(source, 'new-worktree');
    let attempts = 0;
    const attach = setBindingMock('AttachThreadWorktree', async () => {
      attempts += 1;
      if (attempts === 1) throw new Error('branch busy');
      return moved(source);
    });

    await expect(applyWorktreeIntentNow(paneFor(source))).rejects.toThrow('branch busy');
    await expect(applyWorktreeIntentNow(paneFor(source))).resolves.toMatchObject({
      worktreePath: '/wt/feature',
    });
    expect(attach).toHaveBeenCalledTimes(2);
  });

  it('balances caller callbacks when joining an in-flight mutation', async () => {
    const source = thread();
    const pane = paneFor(source);
    setThreadEnvMode(source, 'new-worktree');
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    setBindingMock('AttachThreadWorktree', async () => {
      await gate;
      return moved(source);
    });
    const firstStarted = vi.fn();
    const firstFinished = vi.fn();
    const secondStarted = vi.fn();
    const secondFinished = vi.fn();

    const first = prepareThreadWorktreeIntent({
      pane,
      onWorktreePrepareStarted: firstStarted,
      onWorktreePrepareFinished: firstFinished,
    });
    const second = prepareThreadWorktreeIntent({
      pane,
      onWorktreePrepareStarted: secondStarted,
      onWorktreePrepareFinished: secondFinished,
    });
    expect(firstStarted).toHaveBeenCalledTimes(1);
    expect(secondStarted).toHaveBeenCalledTimes(1);
    release();
    await Promise.all([first, second]);
    expect(firstFinished).toHaveBeenCalledTimes(1);
    expect(secondFinished).toHaveBeenCalledTimes(1);
  });

  it('can leave an explicit target intent for its owning caller to clear', async () => {
    const target = thread({ id: 'child' });
    setThreadEnvMode(target, 'new-worktree');
    const intent = worktreeIntentForThread(target);
    setBindingMock('AttachThreadWorktree', async () => moved(target));

    await materializeWorktreeIntentOnThread({
      targetThread: target,
      intent,
      clearIntentOnSuccess: false,
    });

    expect(worktreeIntentForThread(target).mode).toBe('new-worktree');
  });

  it('does nothing when no workspace intent is staged', async () => {
    const source = thread();
    await expect(applyWorktreeIntentNow(paneFor(source))).resolves.toBeNull();
    expect(getBindingMock('AttachThreadWorktree')).toBeUndefined();
    expect(getBindingMock('PrepareThreadWorktree')).toBeUndefined();
    expect(getBindingMock('GitCreateBranchFrom')).toBeUndefined();
  });
});
