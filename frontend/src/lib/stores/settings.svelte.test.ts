import { describe, expect, it, vi, beforeEach } from 'vitest';
import {
  getSettings,
  loadSettings,
  resetSettingsForTest,
  resyncSettings,
  updateSetting,
} from './settings.svelte';
import type { Settings } from '../types/settings';
import { setBindingMock, getBindingMock } from '../../test/mocks/bindings-app';
import { makeSettings } from '../../test/helpers/settings';

const FULL_SETTINGS: Settings = makeSettings({
  timestampFormat: 'locale',
  recentWorkspaces: ['/tmp/a'],
  diffWordWrap: true,
  claudeBinaryPath: '/usr/local/bin/claude',
  codexBinaryPath: '/usr/local/bin/codex',
  codexEnabled: false,
});

describe('settings store', () => {
  beforeEach(async () => {
    resetSettingsForTest();
    setBindingMock('GetSettings', async () => FULL_SETTINGS);
    setBindingMock('UpdateSettings', async () => FULL_SETTINGS);
    await loadSettings();
  });

  describe('loadSettings()', () => {
    it('an own-undefined wire key does not stomp its default (generated model class shape)', async () => {
      // The wails-generated Settings class declares every field, so a
      // wire-omitted optional key arrives as an OWN property holding
      // undefined — which a bare spread would copy over the default.
      // Field bug 2026-08-22: the untouched compaction slot ("" =
      // built-in default) reached the sprite resolver as undefined and
      // the compaction sprite fell through to the random pool pick.
      setBindingMock('GetSettings', async () => ({
        timestampFormat: 'locale',
        spinnerCompactionAnimation: undefined,
        spinnerVerbsEnabled: undefined,
        network: { bindAll: undefined },
      }));
      await loadSettings();
      expect(getSettings().spinnerCompactionAnimation).toBe('');
      expect(getSettings().spinnerVerbsEnabled).toBe(true);
      expect(getSettings().network.bindAll).toBe(false);
    });

    it('merges the first GetSettings result over defaults for migration', async () => {
      localStorage.clear();
      resetSettingsForTest();
      setBindingMock('GetSettings', async () => ({
        timestampFormat: '24-hour',
        diffWordWrap: true,
      } as Partial<Settings>));
      await loadSettings();
      expect(getSettings().timestampFormat).toBe('24-hour');
      expect(getSettings().diffWordWrap).toBe(true);
      // Unspecified fields fall back to defaults.
      expect(getSettings().sansFont).toBe('geist');
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
      await expect(loadSettings()).resolves.toBe(false);
      consoleErr.mockRestore();
    });
  });

  describe('updateSetting() optimistic update', () => {
    it('applies the change immediately and persists', async () => {
      const serverReturn: Settings = { ...FULL_SETTINGS, timestampFormat: '24-hour' };
      setBindingMock('UpdateSettings', async () => serverReturn);

      const promise = updateSetting('timestampFormat', '24-hour');
      // Optimistic: visible before RPC resolves.
      expect(getSettings().timestampFormat).toBe('24-hour');
      await promise;
      // After resolution the server return wins.
      expect(getSettings().timestampFormat).toBe('24-hour');

      const mock = getBindingMock('UpdateSettings');
      expect(mock).toBeDefined();
      expect(mock!.mock.calls[0][0]).toEqual({ timestampFormat: '24-hour' });
    });

    it('keeps frontend preferences when an offline host cannot mirror them', async () => {
      setBindingMock('UpdateSettings', async () => { throw new Error('rpc fail'); });
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

      const original = getSettings().timestampFormat;
      const newValue: Settings['timestampFormat'] = original === '24-hour' ? 'locale' : '24-hour';

      await updateSetting('timestampFormat', newValue);

      // This frontend is authoritative, including across a restart.
      expect(getSettings().timestampFormat).toBe(newValue);
      resetSettingsForTest();
      expect(getSettings().timestampFormat).toBe(newValue);
      consoleErr.mockRestore();
    });

    it('leaves keys it never patched untouched when it rolls back', async () => {
      // Seed a baseline.
      setBindingMock('GetSettings', async () => ({
        ...FULL_SETTINGS,
        timestampFormat: 'locale',
        diffWordWrap: false,
      }));
      await loadSettings();
      const before = getSettings();

      setBindingMock('UpdateSettings', async () => { throw new Error('fail'); });
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

      await updateSetting('codexEnabled', true);

      expect(getSettings().timestampFormat).toBe(before.timestampFormat);
      expect(getSettings().codexEnabled).toBe(before.codexEnabled);
      expect(getSettings().claudeEnabled).toBe(before.claudeEnabled);
      consoleErr.mockRestore();
    });

    it('restores an absent optional key rather than keeping the failed value', async () => {
      setBindingMock('GetSettings', async () => {
        const seed: Partial<Settings> = { ...FULL_SETTINGS };
        delete seed.claudePromptOverrides;
        return seed;
      });
      await loadSettings();
      expect(getSettings().claudePromptOverrides).toBeUndefined();

      setBindingMock('UpdateSettings', async () => { throw new Error('fail'); });
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

      await updateSetting('claudePromptOverrides', [
        { enabled: true, models: ['m'], prompt: 'p' },
      ]);

      expect(getSettings().claudePromptOverrides).toBeUndefined();
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

  describe('updateSettingsPatch() ordering', () => {
    it('serialises the RPCs so a slow first answer cannot clobber the second', async () => {
      // The backend merges each patch into the persisted file and answers
      // with the whole thing, so a response is only correct if it was
      // produced after every earlier patch landed. Dispatched concurrently,
      // the first call's slow answer would arrive last and persist a
      // snapshot that predates the second write.
      const base: Settings = { ...FULL_SETTINGS, claudeBinaryPath: '/old/claude', codexEnabled: false };
      setBindingMock('GetSettings', async () => base);
      await loadSettings();

      let server: Record<string, unknown> = { ...base };
      let call = 0;
      setBindingMock('UpdateSettings', async (patch: unknown) => {
        server = { ...server, ...(patch as Record<string, unknown>) };
        const snapshot = { ...server };
        const delay = call++ === 0 ? 20 : 0;
        if (delay) await new Promise((resolve) => setTimeout(resolve, delay));
        return snapshot as Partial<Settings>;
      });

      const first = updateSetting('claudeBinaryPath', '/new/claude');
      const second = updateSetting('codexEnabled', true);
      // Both gestures are visible immediately, before either RPC settles.
      expect(getSettings().claudeBinaryPath).toBe('/new/claude');
      expect(getSettings().codexEnabled).toBe(true);

      await Promise.all([first, second]);

      expect(getSettings().claudeBinaryPath).toBe('/new/claude');
      expect(getSettings().codexEnabled).toBe(true);
      expect(call).toBe(2);
    });

    it('does not start the second RPC until the first has answered', async () => {
      const trace: string[] = [];
      setBindingMock('UpdateSettings', async (patch: unknown) => {
        const label = (patch as Settings).claudePromptOverrides?.[0].prompt ?? '?';
        trace.push(`start:${label}`);
        await new Promise((resolve) => setTimeout(resolve, label === 'one' ? 15 : 0));
        trace.push(`end:${label}`);
        return null;
      });

      await Promise.all([
        updateSetting('claudePromptOverrides', [
          { enabled: true, models: ['m'], prompt: 'one' },
        ]),
        updateSetting('claudePromptOverrides', [
          { enabled: true, models: ['m'], prompt: 'two' },
        ]),
      ]);

      expect(trace).toEqual(['start:one', 'end:one', 'start:two', 'end:two']);
    });

    it('rolls back only its own keys, leaving a queued write alone', async () => {
      setBindingMock('UpdateSettings', async (patch: unknown) => {
        if ('claudeBinaryPath' in (patch as Record<string, unknown>)) {
          throw new Error('rpc fail');
        }
        // A real backend would echo the full snapshot; answering with nothing
        // keeps this about what the CLIENT does with the failure rather than
        // about an echo healing it afterwards.
        return null;
      });
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

      const before = getSettings();
      const nextFormat: Settings['claudeBinaryPath'] = before.claudeBinaryPath === '/new/claude' ? '/old/claude' : '/new/claude';

      const failing = updateSetting('claudeBinaryPath', nextFormat);
      // A second gesture writes a different key while the first is in flight.
      const queued = updateSetting('codexEnabled', !before.codexEnabled);
      await Promise.all([failing, queued]);

      expect(getSettings().claudeBinaryPath).toBe(before.claudeBinaryPath);
      expect(getSettings().codexEnabled).toBe(!before.codexEnabled);
      consoleErr.mockRestore();
    });
  });

  describe('resyncSettings() — settings:updated convergence', () => {
    it('re-reads the backend projection so a second client converges', async () => {
      setBindingMock('GetSettings', async () => ({ ...FULL_SETTINGS, claudeAutoCompactStandardPercent: 19 }));
      await resyncSettings();
      expect(getSettings().claudeAutoCompactStandardPercent).toBe(19);
    });

    it('queues behind an in-flight write instead of reverting it', async () => {
      // The echo of our own write arrives while the write is still in
      // flight. An unordered read would be issued against the pre-write
      // state and land after the optimistic apply, discarding the value
      // the user just chose.
      let releaseWrite: (() => void) | undefined;
      const writeLanded = new Promise<void>((resolve) => { releaseWrite = resolve; });
      let readsBeforeWriteSettled = 0;
      let writeSettled = false;
      setBindingMock('UpdateSettings', async () => {
        await writeLanded;
        writeSettled = true;
        return { ...FULL_SETTINGS, claudeAutoCompactStandardPercent: 21 };
      });
      setBindingMock('GetSettings', async () => {
        if (!writeSettled) readsBeforeWriteSettled += 1;
        return { ...FULL_SETTINGS, claudeAutoCompactStandardPercent: 21 };
      });

      const write = updateSetting('claudeAutoCompactStandardPercent', 21);
      const echo = resyncSettings();
      releaseWrite?.();
      await Promise.all([write, echo]);

      expect(readsBeforeWriteSettled).toBe(0);
      expect(getSettings().claudeAutoCompactStandardPercent).toBe(21);
    });

    it('a failed read leaves the store alone and does not poison the queue', async () => {
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
      const before = getSettings().claudeAutoCompactStandardPercent;
      setBindingMock('GetSettings', async () => { throw new Error('offline'); });
      await resyncSettings();
      expect(getSettings().claudeAutoCompactStandardPercent).toBe(before);

      setBindingMock('GetSettings', async () => ({ ...FULL_SETTINGS, claudeAutoCompactStandardPercent: 23 }));
      await resyncSettings();
      expect(getSettings().claudeAutoCompactStandardPercent).toBe(23);
      consoleErr.mockRestore();
    });
  });
});
