import { beforeEach, describe, expect, it } from 'vitest';

import type { Thread } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { loadSettings, updateSetting } from './settings.svelte';
import {
  resetForTest,
  seedDefaultWorktreeIntentForDraft,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';
import type { Settings } from '../types/settings';

const SETTINGS: Settings = {
  theme: 'system',
  timestampFormat: 'locale',
  defaultProvider: 'claude',
  defaultModelClaude: 'claude-opus-4-7',
  defaultModelCodex: 'gpt-5.5',
  modelContextWindows: {},
  recentWorkspaces: [],
  diffWordWrap: false,
  showEndOfTurnDiffs: true,
  backgroundTrayExpanded: false,
  streamingEnabled: true,
  confirmArchive: true,
  confirmDelete: true,
  claudeBinaryPath: 'claude',
  codexBinaryPath: 'codex',
  claudeEnabled: true,
  codexEnabled: true,
  defaultMode: 'chat',
  defaultRuntimeMode: 'full-access',
  defaultThreadEnvMode: 'local',
  worktreeBranchPrefix: 'ao-',
  defaultReasoningEffort: 'high',
  defaultFastMode: false,
  defaultContextWindow: 1000000,
  textGenerationProvider: 'codex',
  textGenerationModel: '',
  textGenerationReasoningEffort: 'low',
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

    expect(worktreeIntentForThread(thread)).toEqual({
      mode: 'new-worktree',
      baseBranch: 'main',
      branchName: '',
    });
  });

  it('does not let later settings changes affect an unseeded draft', async () => {
    const thread = makeThread({ id: 'thread-2' });

    await updateSetting('defaultThreadEnvMode', 'worktree');

    expect(worktreeIntentForThread(thread).mode).toBe('local');
  });
});
