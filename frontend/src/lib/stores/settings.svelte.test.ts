import { describe, expect, it, vi, beforeEach } from 'vitest';
import { getSettings, loadSettings, updateSetting } from './settings.svelte';
import type { Settings } from '../types/settings';
import { setBindingMock, getBindingMock } from '../../test/mocks/bindings-app';

const FULL_SETTINGS: Settings = {
  theme: 'light',
  timestampFormat: 'locale',
  defaultProvider: 'claude',
  defaultModelClaude: 'claude-sonnet-4-6',
  defaultModelCodex: 'gpt-5.4',
  modelContextWindows: {},
  recentWorkspaces: ['/tmp/a'],
  diffWordWrap: true,
  streamingEnabled: true,
  confirmArchive: true,
  confirmDelete: true,
  claudeBinaryPath: '/usr/local/bin/claude',
  codexBinaryPath: '/usr/local/bin/codex',
  claudeEnabled: true,
  codexEnabled: false,
  defaultMode: 'chat',
  defaultRuntimeMode: 'full-access',
  defaultReasoningEffort: 'high',
  defaultFastMode: false,
  defaultContextWindow: 1000000,
  textGenerationProvider: 'codex',
  textGenerationModel: '',
  textGenerationReasoningEffort: 'low',
  observabilityTracingEnabled: false,
  observabilityOtlpEndpoint: '',
  observabilityEventLogEnabled: false,
};

describe('settings store', () => {
  beforeEach(async () => {
    setBindingMock('GetSettings', async () => FULL_SETTINGS);
    setBindingMock('UpdateSettings', async () => FULL_SETTINGS);
    await loadSettings();
  });

  describe('loadSettings()', () => {
    it('merges GetSettings result over defaults', async () => {
      setBindingMock('GetSettings', async () => ({
        theme: 'dark',
        diffWordWrap: true,
      } as Partial<Settings>));
      await loadSettings();
      expect(getSettings().theme).toBe('dark');
      expect(getSettings().diffWordWrap).toBe(true);
      // Unspecified fields fall back to defaults.
      expect(getSettings().timestampFormat).toBe('locale');
      expect(getSettings().claudeEnabled).toBe(true);
    });

    it('does nothing when GetSettings returns null', async () => {
      setBindingMock('GetSettings', async () => null);
      const before = getSettings();
      await loadSettings();
      expect(getSettings()).toBe(before);
    });

    it('toasts on failure but does not throw', async () => {
      setBindingMock('GetSettings', async () => { throw new Error('db down'); });
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
      await expect(loadSettings()).resolves.toBeUndefined();
      consoleErr.mockRestore();
    });
  });

  describe('updateSetting() optimistic update', () => {
    it('applies the change immediately and persists', async () => {
      const serverReturn: Settings = { ...FULL_SETTINGS, theme: 'dark' };
      setBindingMock('UpdateSettings', async () => serverReturn);

      const promise = updateSetting('theme', 'dark');
      // Optimistic: visible before RPC resolves.
      expect(getSettings().theme).toBe('dark');
      await promise;
      // After resolution the server return wins.
      expect(getSettings().theme).toBe('dark');

      const mock = getBindingMock('UpdateSettings');
      expect(mock).toBeDefined();
      expect(mock!.mock.calls[0][0]).toEqual({ theme: 'dark' });
    });

    it('rolls back on RPC failure', async () => {
      setBindingMock('UpdateSettings', async () => { throw new Error('rpc fail'); });
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

      const original = getSettings().theme;
      const newValue: Settings['theme'] = original === 'dark' ? 'light' : 'dark';

      await updateSetting('theme', newValue);

      // Rolled back to original.
      expect(getSettings().theme).toBe(original);
      consoleErr.mockRestore();
    });

    it('rollback restores the entire snapshot, not just the changed field', async () => {
      // Seed a baseline.
      setBindingMock('GetSettings', async () => ({
        ...FULL_SETTINGS,
        theme: 'light',
        diffWordWrap: false,
      }));
      await loadSettings();
      const before = getSettings();

      setBindingMock('UpdateSettings', async () => { throw new Error('fail'); });
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

      await updateSetting('diffWordWrap', true);

      expect(getSettings().theme).toBe(before.theme);
      expect(getSettings().diffWordWrap).toBe(before.diffWordWrap);
      expect(getSettings().claudeEnabled).toBe(before.claudeEnabled);
      consoleErr.mockRestore();
    });

    it('adopts server echo when it differs from the optimistic value', async () => {
      // Server normalises: user asks for codexEnabled=true, server returns false
      // (e.g., because codex binary missing).
      setBindingMock('UpdateSettings', async () => ({
        ...FULL_SETTINGS,
        codexEnabled: false,
      }));

      await updateSetting('codexEnabled', true);
      expect(getSettings().codexEnabled).toBe(false);
    });
  });
});
