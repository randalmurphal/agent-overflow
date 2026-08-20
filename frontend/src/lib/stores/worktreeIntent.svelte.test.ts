import { beforeEach, describe, expect, it } from 'vitest';

import type { Thread } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { loadSettings, updateSetting } from './settings.svelte';
import {
  LOCAL_BASE_SENTINEL,
  clearWorktreeIntent,
  effectiveBranchForThread,
  effectiveWorkspacePathForThread,
  enterCreateBranchMode,
  exitCreateBranchMode,
  hasStagedWorktreeIntent,
  isLocalBase,
  markWorktreeIntentApplied,
  resetForTest,
  resolveBaseForWire,
  seedDefaultWorktreeIntentForDraft,
  setAttachBranch,
  setNewBranchBase,
  setNewBranchName,
  setThreadEnvMode,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';
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
    enterCreateBranchMode(wtThread, { workspaceDirty: false, currentBranch: 'main' });
    const wt = worktreeIntentForThread(wtThread);
    expect(wt.creatingBranch).toBe(true);
    expect(wt.newBranchName).toMatch(/^ao-[0-9a-f]{8}$/);
    expect(wt.newBranchBase).toBe('main');

    const localThread = makeThread({ id: 'thread-local-create' });
    enterCreateBranchMode(localThread, { workspaceDirty: false, currentBranch: 'main' });
    const local = worktreeIntentForThread(localThread);
    expect(local.mode).toBe('local');
    expect(local.creatingBranch).toBe(true);
    expect(local.newBranchName).toBe('');
    expect(local.newBranchBase).toBe('main');
  });

  it('enterCreateBranchMode pre-selects the LOCAL sentinel when the workspace is dirty', () => {
    const thread = makeThread({ id: 'thread-dirty' });
    enterCreateBranchMode(thread, { workspaceDirty: true, currentBranch: 'main' });
    expect(worktreeIntentForThread(thread).newBranchBase).toBe(LOCAL_BASE_SENTINEL);
  });

  it('exitCreateBranchMode drops creatingBranch + name/base but preserves the workspace mode', () => {
    const thread = makeThread({ id: 'thread-exit' });
    setThreadEnvMode(thread, 'new-worktree');
    enterCreateBranchMode(thread, { workspaceDirty: false, currentBranch: 'main' });
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

    enterCreateBranchMode(thread, { workspaceDirty: false, currentBranch: 'main' });
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
    enterCreateBranchMode(thread, { workspaceDirty: false, currentBranch: 'main' });
    setAttachBranch(thread, 'feat/y');
    expect(worktreeIntentForThread(thread).attachBranch).toBe('');
  });

  it('records the LOCAL sentinel as the stored newBranchBase when the user picks it', () => {
    const thread = makeThread({ id: 'thread-local-base' });
    setThreadEnvMode(thread, 'new-worktree');
    enterCreateBranchMode(thread, { workspaceDirty: false, currentBranch: 'main' });

    setNewBranchBase(thread, LOCAL_BASE_SENTINEL);
    expect(worktreeIntentForThread(thread).newBranchBase).toBe(LOCAL_BASE_SENTINEL);
    expect(
      resolveBaseForWire(worktreeIntentForThread(thread).newBranchBase, 'main').carryLocalChanges,
    ).toBe(true);

    setNewBranchBase(thread, 'dev');
    expect(worktreeIntentForThread(thread).newBranchBase).toBe('dev');
    expect(
      resolveBaseForWire(worktreeIntentForThread(thread).newBranchBase, 'main').carryLocalChanges,
    ).toBe(false);
  });

  it('carries `applied` across edits inside the quadrant and drops it on a quadrant change', () => {
    const thread = makeThread({ id: 'thread-applied' });
    setThreadEnvMode(thread, 'new-worktree');
    enterCreateBranchMode(thread, { workspaceDirty: false, currentBranch: 'main' });
    markWorktreeIntentApplied(thread.id, { worktreePath: '/wt/x', branch: 'feat/x' });
    expect(worktreeIntentForThread(thread).applied).toEqual({
      worktreePath: '/wt/x',
      branch: 'feat/x',
    });

    // Refining the same request keeps what it produced.
    setNewBranchName(thread, 'feat/x2');
    setNewBranchBase(thread, 'dev');
    expect(worktreeIntentForThread(thread).applied?.worktreePath).toBe('/wt/x');

    // Leaving create-branch mode asks for something else.
    exitCreateBranchMode(thread);
    expect(worktreeIntentForThread(thread).applied).toBeNull();
  });

  it('flipping back to local clears an applied worktree', () => {
    const thread = makeThread({ id: 'thread-applied-local' });
    setThreadEnvMode(thread, 'new-worktree');
    markWorktreeIntentApplied(thread.id, { worktreePath: '/wt/y', branch: 'main' });
    expect(worktreeIntentForThread(thread).applied).not.toBeNull();

    setThreadEnvMode(thread, 'local');
    expect(worktreeIntentForThread(thread).applied).toBeNull();
    // Re-selecting the same mode is not a change, so it is not a reset either.
    setThreadEnvMode(thread, 'new-worktree');
    markWorktreeIntentApplied(thread.id, { worktreePath: '/wt/z', branch: 'main' });
    setThreadEnvMode(thread, 'new-worktree');
    expect(worktreeIntentForThread(thread).applied?.worktreePath).toBe('/wt/z');
  });

  it('clearWorktreeIntent drops the applied record with the intent', () => {
    const thread = makeThread({ id: 'thread-applied-clear' });
    setThreadEnvMode(thread, 'new-worktree');
    markWorktreeIntentApplied(thread.id, { worktreePath: '/wt/c', branch: 'main' });
    clearWorktreeIntent(thread.id);
    expect(worktreeIntentForThread(thread).applied).toBeNull();
    expect(worktreeIntentForThread(thread).mode).toBe('local');
  });

  it('effective* helpers prefer the applied workspace over the row\'s own fields', () => {
    const thread = makeThread({ id: 'thread-effective', workspacePath: '/repo', branch: 'main' });
    expect(effectiveWorkspacePathForThread(thread)).toBe('/repo');
    expect(effectiveBranchForThread(thread)).toBe('main');

    setThreadEnvMode(thread, 'new-worktree');
    markWorktreeIntentApplied(thread.id, { worktreePath: '/wt/e', branch: 'feat/e' });
    // The row has not moved (a send binds it); the pane has.
    expect(effectiveWorkspacePathForThread(thread)).toBe('/wt/e');
    expect(effectiveBranchForThread(thread)).toBe('feat/e');

    // A branch-only apply carries no worktree, so the workspace stays put.
    const local = makeThread({ id: 'thread-effective-branch', workspacePath: '/repo', branch: 'main' });
    enterCreateBranchMode(local, { workspaceDirty: false, currentBranch: 'main' });
    markWorktreeIntentApplied(local.id, { worktreePath: '', branch: 'feat/local' });
    expect(effectiveWorkspacePathForThread(local)).toBe('/repo');
    expect(effectiveBranchForThread(local)).toBe('feat/local');

    expect(effectiveWorkspacePathForThread(null)).toBe('');
    expect(effectiveBranchForThread(undefined)).toBe('');
  });

  it('hasStagedWorktreeIntent answers the send path synchronously', () => {
    const thread = makeThread({ id: 'thread-staged-gate' });
    expect(hasStagedWorktreeIntent(thread)).toBe(false);
    setThreadEnvMode(thread, 'new-worktree');
    expect(hasStagedWorktreeIntent(thread)).toBe(true);
    setThreadEnvMode(thread, 'local');
    expect(hasStagedWorktreeIntent(thread)).toBe(false);
    enterCreateBranchMode(thread, { workspaceDirty: false, currentBranch: 'main' });
    expect(hasStagedWorktreeIntent(thread)).toBe(true);
  });

  it('isLocalBase narrows the sentinel string without raw comparison', () => {
    expect(isLocalBase(LOCAL_BASE_SENTINEL)).toBe(true);
    expect(isLocalBase('main')).toBe(false);
    expect(isLocalBase('')).toBe(false);
    expect(isLocalBase(null)).toBe(false);
    expect(isLocalBase(undefined)).toBe(false);
  });

  it('resolveBaseForWire maps sentinel to (currentBranch, carry=true) and otherwise passes through', () => {
    expect(resolveBaseForWire(LOCAL_BASE_SENTINEL, 'main')).toEqual({
      baseBranch: 'main',
      carryLocalChanges: true,
    });
    expect(resolveBaseForWire('dev', 'main')).toEqual({
      baseBranch: 'dev',
      carryLocalChanges: false,
    });
    expect(resolveBaseForWire('', 'main')).toEqual({
      baseBranch: '',
      carryLocalChanges: false,
    });
  });
});
