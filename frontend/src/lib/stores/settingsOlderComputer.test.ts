// A computer on v0.0.19 answers its settings without the keys this build
// added. Its answer is what says it lacks one: nothing is written there for a
// key it lacks, by a page or by this window's mirror of its own preferences,
// which v0.0.19 would class host tier and refuse for want of a step-up.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  applySettingsSnapshot, computerHasSetting, loadSettings, mirrorFrontendPreferences,
  resetSettingsForTest, updateSetting, updateSettingsPatch,
} from './settings.svelte';
import { getPinnedBackend } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { setCarriedSessionScopes } from '../transport/scopes';
import type { TransportHello } from '../transport/wsClient';
import type { Settings } from '../types/settings';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { makeSettings } from '../../test/helpers/settings';
import { SETTINGS_AFTER_V0_0_19, settingsAsV0019 } from '../../test/helpers/olderComputer';
import { setBindingMock } from '../../test/mocks/bindings-app';

let sent: Array<{ backend: BackendKey; patch: Partial<Settings> }>;

/** Patches UpdateSettings carried to `backend`. */
function sentTo(backend: BackendKey): Partial<Settings>[] {
  return sent.filter((call) => call.backend === backend).map((call) => call.patch);
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await new Promise((resolve) => setTimeout(resolve, 0));
}

describe('a computer on v0.0.19, by its settings answer', () => {
  beforeEach(async () => {
    resetSettingsForTest();
    sent = [];
    setBindingMock('GetSettings', async () => (getPinnedBackend() === 'laptop' ? settingsAsV0019() : makeSettings()));
    setBindingMock('UpdateSettings', async (patch: unknown) => {
      sent.push({ backend: getPinnedBackend() ?? HOME_BACKEND, patch: patch as Partial<Settings> });
      return getPinnedBackend() === 'laptop' ? settingsAsV0019() : makeSettings(patch as Partial<Settings>);
    });
    await loadSettings();
    stageBackend({ id: 'laptop', hello: { backendId: 'laptop' } as TransportHello });
    setCarriedSessionScopes('laptop', ['settings:read', 'settings:write']);
  });

  afterEach(() => {
    resetStagedBackends();
    resetSettingsForTest();
  });

  it('lacks each key its answer does not name, and has one it omits when empty', async () => {
    expect(computerHasSetting('sharedMemoryEnabled', 'laptop'), 'before its answer').toBe(true);
    await loadSettings('laptop');
    for (const key of SETTINGS_AFTER_V0_0_19) expect(computerHasSetting(key, 'laptop'), key).toBe(false);
    expect(computerHasSetting('claudeEnabled', 'laptop')).toBe(true);
    // Go's omitempty leaves an empty value out of every answer, this build's too.
    const empty = settingsAsV0019();
    delete empty.spinnerCustomVerbs;
    applySettingsSnapshot(empty, 'laptop');
    expect(computerHasSetting('spinnerCustomVerbs', 'laptop')).toBe(true);
    // This window's home serves this frontend: it has every key.
    applySettingsSnapshot(settingsAsV0019());
    for (const key of SETTINGS_AFTER_V0_0_19) expect(computerHasSetting(key, HOME_BACKEND), key).toBe(true);
  });

  it('writes no key the computer lacks, and the rest of the patch as before', async () => {
    await loadSettings('laptop');
    await updateSettingsPatch({ sharedMemoryEnabled: true, claudeEnabled: false }, 'laptop');
    await updateSetting('subThreadLevels', 2, 'laptop');
    expect(sentTo('laptop')).toEqual([{ claudeEnabled: false }]);
  });

  it('mirrors this window\'s preferences there once it has read what the computer has, less the keys it lacks', async () => {
    mirrorFrontendPreferences('laptop');
    await settle();
    expect(sentTo('laptop'), 'mirrored before the computer answered what it has').toEqual([]);
    await loadSettings('laptop');
    await settle();
    expect(sentTo('laptop')).toHaveLength(1);
    expect(sentTo('laptop')[0]).not.toHaveProperty('notifyThreadAttention');
    expect(sentTo('laptop')[0]).toHaveProperty('fontSize');
    // A change of a preference it lacks reaches the computers that have it.
    await updateSetting('notifyThreadAttention', false);
    await settle();
    expect(sentTo('laptop')).toHaveLength(1);
    expect(sentTo(HOME_BACKEND).at(-1)).toEqual({ notifyThreadAttention: false });
  });

  it('mirrors at once to a computer this window cannot read the settings of', async () => {
    setCarriedSessionScopes('laptop', ['threads:read']);
    mirrorFrontendPreferences('laptop');
    await settle();
    expect(sentTo('laptop')).toHaveLength(1);
  });
});
