import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import PerformanceSettings from './PerformanceSettings.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';

async function seed(overrides: Partial<Settings> = {}): Promise<Settings> {
  const merged = makeSettings(overrides);
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => {
    const p = (patch as Record<string, unknown>) ?? {};
    return { ...merged, ...p };
  });
  await loadSettings();
  return merged;
}

describe('<PerformanceSettings>', () => {
  beforeEach(async () => {
    await seed();
  });

  it('dispatches lowPowerMode patch when the toggle is clicked', async () => {
    const { getByRole } = render(PerformanceSettings);
    const toggle = getByRole('switch', { name: 'Toggle Low Power Mode' });
    expect(toggle.getAttribute('aria-checked')).toBe('false');

    await fireEvent.click(toggle);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ lowPowerMode: true });
  });

  it('dispatches streamingEnabled patch from its default-on state', async () => {
    const { getByRole } = render(PerformanceSettings);
    const toggle = getByRole('switch', { name: 'Toggle Streaming' });
    expect(toggle.getAttribute('aria-checked')).toBe('true');

    await fireEvent.click(toggle);

    const mock = getBindingMock('UpdateSettings');
    expect(mock!.mock.calls[0][0]).toEqual({ streamingEnabled: false });
  });

  it('dispatches keepAwakeScreen patch from its default-on state', async () => {
    const { getByRole } = render(PerformanceSettings);
    const toggle = getByRole('switch', { name: 'Toggle Keep-Awake Screen' });
    expect(toggle.getAttribute('aria-checked')).toBe('true');

    await fireEvent.click(toggle);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ keepAwakeScreen: false });
  });
});
