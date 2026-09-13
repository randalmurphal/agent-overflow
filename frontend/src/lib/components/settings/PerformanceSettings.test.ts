import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import PerformanceSettings from './PerformanceSettings.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import { HOST_TIER_REASON } from './settingsComputer';
import PerformanceSettingsOnComputer from './__tests__/PerformanceSettingsOnComputer.svelte';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { applySettingsSnapshot } from '../../stores/settings.svelte';

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

  it('renders keep-awake inert, saying why, off the host while the device keys stay live', async () => {
    // keepAwake* are host tier; lowPowerMode and streamingEnabled are not.
    setPageGrantsFromBootstrap(true);
    try {
      const { getByRole } = render(PerformanceSettings);
      const keepAwake = getByRole('switch', { name: 'Toggle Keep-Awake Screen' }) as HTMLButtonElement;
      expect(keepAwake.disabled).toBe(true);
      expect(keepAwake.title).toBe(HOST_TIER_REASON);
      const lowPower = getByRole('switch', { name: 'Toggle Low Power Mode' }) as HTMLButtonElement;
      expect(lowPower.disabled).toBe(false);
    } finally {
      setPageGrantsFromBootstrap(false);
    }
  });

  it('keeps keep-awake live for an attached computer off the host: presence is the server\'s call there', async () => {
    // The page knows whether it sits at its OWN backend; whether an attached
    // computer sees this connection as present (a loopback peer is the
    // host) is judged there per connection and carried in no snapshot, so
    // the control stays live and a refusal runs the passkey ceremony.
    const gpu = 'gpu-backend';
    stageBackend({ id: gpu, backendId: gpu, name: 'GPU' });
    applySettingsSnapshot(makeSettings(), gpu);
    setPageGrantsFromBootstrap(true);
    try {
      const { getByRole } = render(PerformanceSettingsOnComputer, { props: { backend: gpu } });
      const keepAwake = getByRole('switch', { name: 'Toggle Keep-Awake Screen' }) as HTMLButtonElement;
      expect(keepAwake.disabled).toBe(false);
      expect(keepAwake.title).toBe('');
    } finally {
      setPageGrantsFromBootstrap(false);
      resetStagedBackends();
    }
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
