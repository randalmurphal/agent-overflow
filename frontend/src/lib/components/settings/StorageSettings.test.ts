import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import StorageSettings from './StorageSettings.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import { HOST_TIER_REASON } from './settingsComputer';

async function seed(overrides: Partial<Settings> = {}): Promise<Settings> {
  const merged = makeSettings(overrides);
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => {
    const p = (patch as Record<string, unknown>) ?? {};
    return { ...merged, ...p };
  });
  setBindingMock('ListArchivedThreads', async () => []);
  await loadSettings();
  return merged;
}

describe('<StorageSettings> — Retention', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the retention input with the default value', async () => {
    const { getByTestId } = render(StorageSettings);
    expect(getByTestId('settings-retention')).toBeTruthy();
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    expect(input.value).toBe('30');
  });

  it('renders the retention input inert, saying why, off the host without a passkey', async () => {
    // `retention` is host tier (internal/settings/tier.go): the backend takes
    // the write only under a step-up proof, which a networked page with no
    // host presence and no passkey cannot give.
    setPageGrantsFromBootstrap(true);
    try {
      const { getByTestId } = render(StorageSettings);
      const input = getByTestId('settings-retention-days') as HTMLInputElement;
      expect(input.disabled).toBe(true);
      expect(input.title).toBe(HOST_TIER_REASON);
    } finally {
      setPageGrantsFromBootstrap(false);
    }
  });

  it('shows the disabled-cleanup hint when retention days is 0', async () => {
    await seed({ retention: { days: 0 } });
    const { getByText } = render(StorageSettings);
    expect(getByText(/automatic cleanup is disabled/i)).toBeTruthy();
  });

  it('dispatches retention patch on blur with parsed integer', async () => {
    const { getByTestId } = render(StorageSettings);
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = '7';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ retention: { days: 7 } });
  });

  it('coerces non-numeric input to 0 (disabled)', async () => {
    const { getByTestId } = render(StorageSettings);
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = 'banana';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ retention: { days: 0 } });
  });

  it('coerces negative input to 0 (disabled)', async () => {
    const { getByTestId } = render(StorageSettings);
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = '-5';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ retention: { days: 0 } });
  });

  it('clamps above the Go-side retention cap', async () => {
    const { getByTestId } = render(StorageSettings);
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = '99999';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock!.mock.calls[0][0]).toEqual({ retention: { days: 36500 } });
  });
});

describe('<StorageSettings> — Archived threads', () => {
  beforeEach(async () => {
    await seed();
  });

  it('mounts the archive list under the retention section', async () => {
    const { findByTestId } = render(StorageSettings);
    expect(await findByTestId('settings-archived-threads')).toBeTruthy();
  });
});
