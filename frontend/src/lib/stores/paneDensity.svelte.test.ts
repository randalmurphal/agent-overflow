import { beforeEach, describe, expect, it } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import {
  getMinPaneWidth,
  getPaneDensityMode,
  setPaneDensityMode,
} from './paneDensity.svelte';
import { loadSettings, resetSettingsForTest } from './settings.svelte';
import { makeSettings } from '../../test/helpers/settings';

describe('paneDensity store', () => {
  beforeEach(() => {
    resetSettingsForTest();
    setBindingMock('UpdateSettings', async (patch: unknown) => ({
      ...makeSettings(),
      ...(patch as Partial<ReturnType<typeof makeSettings>>),
    }));
  });

  it('defaults to compact', () => {
    expect(getPaneDensityMode()).toBe('compact');
    expect(getMinPaneWidth()).toBe(560);
  });

  it('persists selected density through the settings store', async () => {
    await setPaneDensityMode('spacious');

    expect(getPaneDensityMode()).toBe('spacious');
    expect(getMinPaneWidth()).toBe(1400);
  });

  it('uses pane density loaded from persisted settings', async () => {
    setBindingMock('GetSettings', async () => makeSettings({ paneDensity: 'spacious' }));

    await loadSettings();

    expect(getPaneDensityMode()).toBe('spacious');
    expect(getMinPaneWidth()).toBe(1400);
  });
});
