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
import { render } from '@testing-library/svelte';
import { assistantsOn } from './assistants.svelte';
import ComputerSettingsPage from '../components/settings/ComputerSettingsPage.svelte';
import AssistantsSettings from '../components/settings/AssistantsSettings.svelte';
import AgentProfilesSettings from '../components/settings/AgentProfilesSettings.svelte';
import MemorySettings from '../components/settings/MemorySettings.svelte';
import type { TransportHello } from '../transport/wsClient';
import type { Settings } from '../types/settings';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { makeSettings } from '../../test/helpers/settings';
import { SETTINGS_AFTER_V0_0_19, settingsAsV0019 } from '../../test/helpers/olderComputer';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';

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
    // Home is decided by its own answer too: an installed app's home can be older.
    applySettingsSnapshot(settingsAsV0019());
    for (const key of SETTINGS_AFTER_V0_0_19) expect(computerHasSetting(key, HOME_BACKEND), key).toBe(false);
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
});

// The Android app runs its own installed frontend, so its home computer can
// be on v0.0.19: home's pages and assistants follow home's answer.
describe('a home computer on v0.0.19', () => {
  beforeEach(() => {
    resetSettingsForTest();
    setBindingMock('GetSettings', async () => settingsAsV0019());
    setBindingMock('UpdateSettings', async () => settingsAsV0019());
  });

  afterEach(() => resetSettingsForTest());

  it('reads assistants as off and offers no editor on the pages for keys it lacks', async () => {
    await loadSettings();
    expect(assistantsOn(HOME_BACKEND)).toBe(false);
    for (const [Page, lacks] of [
      [AssistantsSettings, 'assistants'], [AgentProfilesSettings, 'agent profiles'], [MemorySettings, 'memory settings'],
    ] as const) {
      const view = render(ComputerSettingsPage, { props: { backend: HOME_BACKEND, Page, needsComputer: false } });
      expect((await view.findByTestId('settings-older-computer')).textContent).toBe(`This computer runs an older version: it has no ${lacks}.`);
      expect(view.container.querySelectorAll('input, textarea, select, button, [role="switch"]'), lacks).toHaveLength(0);
      view.unmount();
    }
    expect(getBindingMock('UpdateSettings')).not.toHaveBeenCalled();
  });
});
