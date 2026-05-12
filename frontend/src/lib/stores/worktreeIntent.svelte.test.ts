import { beforeEach, describe, expect, it } from 'vitest';

import type { Thread } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { loadSettings, updateSetting } from './settings.svelte';
import {
  LOCAL_BASE_SENTINEL,
  isLocalBase,
  resetForTest,
  resolveBaseForWire,
  seedDefaultWorktreeIntentForDraft,
  setThreadEnvMode,
  setWorktreeBaseBranch,
  setWorktreeBranchName,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';
import type { Settings } from '../types/settings';

const SETTINGS: Settings = {
  theme: 'system',
  timestampFormat: 'locale',
  sansFont: 'geist',
  monoFont: 'geist',
  recentWorkspaces: [],
  diffWordWrap: false,
  streamingEnabled: true,
  confirmArchive: true,
  confirmDelete: true,
  claudeBinaryPath: 'claude',
  codexBinaryPath: 'codex',
  claudeEnabled: true,
  codexEnabled: true,
  defaultThreadEnvMode: 'local',
  worktreeBranchPrefix: 'ao-',
  textGenerationProvider: 'codex',
  textGenerationModel: '',
  textGenerationReasoningEffort: 'low',
  claudeAutoCompactStandardPercent: 90,
  claudeAutoCompactExtendedPercent: 90,
  codexAutoCompactStandardPercent: 90,
  codexAutoCompactExtendedPercent: 90,
  observabilityTracingEnabled: false,
  observabilityOtlpEndpoint: '',
  observabilityEventLogEnabled: false,
  network: { bindAll: false },
};

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

  it('materializes default worktree intent when the draft is created', async () => {
    await updateSetting('defaultThreadEnvMode', 'worktree');
    const thread = makeThread();

    seedDefaultWorktreeIntentForDraft(thread);
    await updateSetting('defaultThreadEnvMode', 'local');

    const intent = worktreeIntentForThread(thread);
    expect(intent.mode).toBe('new-worktree');
    expect(intent.baseBranch).toBe('main');
    expect(intent.branchName).toMatch(/^ao-[0-9a-f]{8}$/);
  });

  it('does not let later settings changes affect an unseeded draft', async () => {
    const thread = makeThread({ id: 'thread-2' });

    await updateSetting('defaultThreadEnvMode', 'worktree');

    expect(worktreeIntentForThread(thread).mode).toBe('local');
  });

  it('seeds a generated branch name on local→new-worktree transition', () => {
    const thread = makeThread({ id: 'thread-prefill' });

    setThreadEnvMode(thread, 'new-worktree');

    const intent = worktreeIntentForThread(thread);
    expect(intent.mode).toBe('new-worktree');
    expect(intent.branchName).toMatch(/^ao-[0-9a-f]{8}$/);
  });

  it('regenerates the branch name when the user toggles out and back into worktree mode', () => {
    const thread = makeThread({ id: 'thread-regen' });

    setThreadEnvMode(thread, 'new-worktree');
    const first = worktreeIntentForThread(thread).branchName;

    setThreadEnvMode(thread, 'local');
    setThreadEnvMode(thread, 'new-worktree');
    const second = worktreeIntentForThread(thread).branchName;

    expect(second).toMatch(/^ao-[0-9a-f]{8}$/);
    // 8 hex bits is 32 bits — collision is astronomically unlikely.
    expect(second).not.toBe(first);
  });

  it('preserves a user-typed branch name when staying in worktree mode', () => {
    const thread = makeThread({ id: 'thread-preserve' });

    setThreadEnvMode(thread, 'new-worktree');
    setWorktreeBranchName(thread, 'custom/feature');
    setThreadEnvMode(thread, 'new-worktree');

    expect(worktreeIntentForThread(thread).branchName).toBe('custom/feature');
  });

  it('records the Local sentinel as the stored baseBranch when the user picks it', () => {
    // carryLocalChanges is no longer a stored field — derived at the
    // wire boundary via resolveBaseForWire. The store's contract is
    // that the sentinel survives round-trip; a follow-up
    // resolveBaseForWire call recovers the carry flag.
    const thread = makeThread({ id: 'thread-local-base' });
    setThreadEnvMode(thread, 'new-worktree');

    setWorktreeBaseBranch(thread, LOCAL_BASE_SENTINEL);
    expect(worktreeIntentForThread(thread).baseBranch).toBe(LOCAL_BASE_SENTINEL);
    expect(
      resolveBaseForWire(worktreeIntentForThread(thread).baseBranch, 'main').carryLocalChanges,
    ).toBe(true);

    setWorktreeBaseBranch(thread, 'dev');
    expect(worktreeIntentForThread(thread).baseBranch).toBe('dev');
    expect(
      resolveBaseForWire(worktreeIntentForThread(thread).baseBranch, 'main').carryLocalChanges,
    ).toBe(false);
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
