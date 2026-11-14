import { beforeEach, describe, expect, it } from 'vitest';

import type { Thread } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { loadSettings, updateSetting } from './settings.svelte';
import {
  LOCAL_BASE_SENTINEL,
  enterCreateBranchMode,
  exitCreateBranchMode,
  hasStagedWorktreeIntent,
  isLocalBase,
  resetForTest,
  resolveBaseForWire,
  seedDefaultWorktreeIntentForDraft,
  setAttachBranch,
  setNewBranchBase,
  setNewBranchName,
  setThreadEnvMode,
  startNewBranch,
  startNewBranchFromHere,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';
import type { GitStatus } from '../types/git';
import type { Settings } from '../types/settings';
import { makeSettings } from '../../test/helpers/settings';

const SETTINGS: Settings = makeSettings();

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Draft',
    provider: 'claude',
    workspacePath: '/repo',
    projectPath: '/repo',
    projectId: 'project-1',
    branch: 'main',
    model: 'm',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

describe('worktreeIntent store', () => {
  beforeEach(async () => {
    resetForTest();
    setBindingMock('GetSettings', async () => SETTINGS);
    setBindingMock('UpdateSettings', async (patch: Partial<Settings>) => ({
      ...SETTINGS,
      ...patch,
    }));
    await loadSettings();
  });

  it('seeds new-worktree + creatingBranch when defaultThreadEnvMode=worktree on draft creation', async () => {
    await updateSetting('defaultThreadEnvMode', 'worktree');
    const thread = makeThread();

    seedDefaultWorktreeIntentForDraft(thread);
    await updateSetting('defaultThreadEnvMode', 'local');

    const intent = worktreeIntentForThread(thread);
    expect(intent.mode).toBe('new-worktree');
    expect(intent.creatingBranch).toBe(true);
    expect(intent.newBranchBase).toBe('main');
    expect(intent.newBranchName).toMatch(/^ao-[0-9a-f]{8}$/);
  });

  it('does not let later settings changes affect an unseeded draft', async () => {
    const thread = makeThread({ id: 'thread-2' });

    await updateSetting('defaultThreadEnvMode', 'worktree');

    expect(worktreeIntentForThread(thread).mode).toBe('local');
  });

  it('toggling into new-worktree leaves creatingBranch=false (user opts in via + new branch)', () => {
    const thread = makeThread({ id: 'thread-prefill' });

    setThreadEnvMode(thread, 'new-worktree');

    const intent = worktreeIntentForThread(thread);
    expect(intent.mode).toBe('new-worktree');
    expect(intent.creatingBranch).toBe(false);
    expect(intent.newBranchName).toBe('');
    expect(intent.attachBranch).toBe('');
  });

  it('enterCreateBranchMode seeds an auto branch name in new-worktree mode but leaves it blank in local mode', () => {
    const wtThread = makeThread({ id: 'thread-wt-create' });
    setThreadEnvMode(wtThread, 'new-worktree');
    enterCreateBranchMode(wtThread, 'main');
    const wt = worktreeIntentForThread(wtThread);
    expect(wt.creatingBranch).toBe(true);
    expect(wt.newBranchName).toMatch(/^ao-[0-9a-f]{8}$/);
    expect(wt.newBranchBase).toBe('main');

    const localThread = makeThread({ id: 'thread-local-create' });
    enterCreateBranchMode(localThread, 'main');
    const local = worktreeIntentForThread(localThread);
    expect(local.mode).toBe('local');
    expect(local.creatingBranch).toBe(true);
    expect(local.newBranchName).toBe('');
    expect(local.newBranchBase).toBe('main');
  });

  it('enterCreateBranchMode pre-selects the LOCAL sentinel when the workspace is dirty', () => {
    const thread = makeThread({ id: 'thread-dirty' });
    enterCreateBranchMode(thread, LOCAL_BASE_SENTINEL);
    expect(worktreeIntentForThread(thread).newBranchBase).toBe(LOCAL_BASE_SENTINEL);
  });

  it('exitCreateBranchMode drops creatingBranch + name/base but preserves the workspace mode', () => {
    const thread = makeThread({ id: 'thread-exit' });
    setThreadEnvMode(thread, 'new-worktree');
    enterCreateBranchMode(thread, 'main');
    setNewBranchName(thread, 'feat/x');
    expect(worktreeIntentForThread(thread).creatingBranch).toBe(true);

    exitCreateBranchMode(thread);
    const intent = worktreeIntentForThread(thread);
    expect(intent.creatingBranch).toBe(false);
    expect(intent.newBranchName).toBe('');
    expect(intent.newBranchBase).toBe('');
    expect(intent.mode).toBe('new-worktree');
  });

  it('setNewBranchName / setNewBranchBase only mutate while creatingBranch=true', () => {
    const thread = makeThread({ id: 'thread-guard' });
    setThreadEnvMode(thread, 'new-worktree');

    setNewBranchName(thread, 'should-not-stick');
    setNewBranchBase(thread, 'dev');

    const before = worktreeIntentForThread(thread);
    expect(before.newBranchName).toBe('');
    expect(before.newBranchBase).toBe('');

    enterCreateBranchMode(thread, 'main');
    setNewBranchName(thread, 'feat/now');
    setNewBranchBase(thread, 'dev');
    const after = worktreeIntentForThread(thread);
    expect(after.newBranchName).toBe('feat/now');
    expect(after.newBranchBase).toBe('dev');
  });

  it('setAttachBranch only applies in new-worktree + !creatingBranch', () => {
    const thread = makeThread({ id: 'thread-attach' });

    // local mode: ignored
    setAttachBranch(thread, 'feat/x');
    expect(worktreeIntentForThread(thread).attachBranch).toBe('');

    // new-worktree, !creating: applies
    setThreadEnvMode(thread, 'new-worktree');
    setAttachBranch(thread, 'feat/x');
    expect(worktreeIntentForThread(thread).attachBranch).toBe('feat/x');

    // new-worktree, creating: ignored
    enterCreateBranchMode(thread, 'main');
    setAttachBranch(thread, 'feat/y');
    expect(worktreeIntentForThread(thread).attachBranch).toBe('');
  });

  it('records the LOCAL sentinel as the stored newBranchBase when the user picks it', () => {
    const thread = makeThread({ id: 'thread-local-base' });
    setThreadEnvMode(thread, 'new-worktree');
    enterCreateBranchMode(thread, 'main');

    setNewBranchBase(thread, LOCAL_BASE_SENTINEL);
    expect(worktreeIntentForThread(thread).newBranchBase).toBe(LOCAL_BASE_SENTINEL);
    expect(
      resolveBaseForWire(worktreeIntentForThread(thread).newBranchBase).carryLocalChanges,
    ).toBe(true);

    setNewBranchBase(thread, 'dev');
    expect(worktreeIntentForThread(thread).newBranchBase).toBe('dev');
    expect(
      resolveBaseForWire(worktreeIntentForThread(thread).newBranchBase).carryLocalChanges,
    ).toBe(false);
  });

  it('hasStagedWorktreeIntent answers the send path synchronously', () => {
    const thread = makeThread({ id: 'thread-staged-gate' });
    expect(hasStagedWorktreeIntent(thread)).toBe(false);
    setThreadEnvMode(thread, 'new-worktree');
    expect(hasStagedWorktreeIntent(thread)).toBe(true);
    setThreadEnvMode(thread, 'local');
    expect(hasStagedWorktreeIntent(thread)).toBe(false);
    enterCreateBranchMode(thread, 'main');
    expect(hasStagedWorktreeIntent(thread)).toBe(true);
  });

  it('isLocalBase narrows the sentinel string without raw comparison', () => {
    expect(isLocalBase(LOCAL_BASE_SENTINEL)).toBe(true);
    expect(isLocalBase('main')).toBe(false);
    expect(isLocalBase('')).toBe(false);
    expect(isLocalBase(null)).toBe(false);
    expect(isLocalBase(undefined)).toBe(false);
  });

  it('resolveBaseForWire maps sentinel to (current checkout, carry=true) and otherwise passes through', () => {
    expect(resolveBaseForWire(LOCAL_BASE_SENTINEL)).toEqual({
      baseBranch: '',
      carryLocalChanges: true,
    });
    expect(resolveBaseForWire('dev')).toEqual({
      baseBranch: 'dev',
      carryLocalChanges: false,
    });
    expect(resolveBaseForWire('')).toEqual({
      baseBranch: '',
      carryLocalChanges: false,
    });
  });

  describe('starting a branch from a pane', () => {
    const pane = (thread: Thread, hasChanges: boolean) => ({
      thread,
      gitStatus: { status: { isRepo: true, branch: 'main', hasChanges } as GitStatus },
    });

    it('defaults to the current checkout, carrying a dirty tree only in local mode', () => {
      const clean = makeThread({ id: 'clean' });
      startNewBranch(pane(clean, false));
      expect(worktreeIntentForThread(clean)).toMatchObject({ mode: 'local', creatingBranch: true, newBranchBase: '' });

      const dirty = makeThread({ id: 'dirty' });
      startNewBranch(pane(dirty, true));
      expect(worktreeIntentForThread(dirty).newBranchBase).toBe(LOCAL_BASE_SENTINEL);

      const worktree = makeThread({ id: 'worktree' });
      setThreadEnvMode(worktree, 'new-worktree');
      startNewBranch(pane(worktree, true));
      expect(worktreeIntentForThread(worktree)).toMatchObject({ mode: 'new-worktree', newBranchBase: '' });
    });

    it('keeps a staged base', () => {
      const thread = makeThread();
      enterCreateBranchMode(thread, 'develop');
      startNewBranch(pane(thread, true));
      expect(worktreeIntentForThread(thread).newBranchBase).toBe('develop');
    });

    it('"from here" branches the current checkout in place over a staged worktree and base', () => {
      const thread = makeThread();
      setThreadEnvMode(thread, 'new-worktree');
      enterCreateBranchMode(thread, 'develop');
      setNewBranchName(thread, 'feat/keep');
      startNewBranchFromHere(pane(thread, true));
      expect(worktreeIntentForThread(thread)).toEqual({
        mode: 'local',
        creatingBranch: true,
        newBranchName: 'feat/keep',
        newBranchBase: '',
        attachBranch: '',
      });

      const attaching = makeThread({ id: 'attaching' });
      setThreadEnvMode(attaching, 'new-worktree');
      setAttachBranch(attaching, 'refs/tags/v1');
      startNewBranchFromHere(pane(attaching, false));
      expect(worktreeIntentForThread(attaching)).toMatchObject({ mode: 'local', newBranchBase: '', attachBranch: '' });
    });
  });
});
