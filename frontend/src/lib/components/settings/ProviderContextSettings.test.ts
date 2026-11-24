import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ProviderContextSettings from './ProviderContextSettings.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import {
  setBindingMock,
  getBindingMock,
} from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';

const BASE_SETTINGS: Settings = makeSettings({
  claudeAutoCompactExtendedPercent: 85,
  codexAutoCompactStandardPercent: 80,
  codexAutoCompactExtendedPercent: 75,
});

async function seed(overrides: Partial<Settings> = {}): Promise<Settings> {
  const merged = { ...BASE_SETTINGS, ...overrides };
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => {
    const p = (patch as Record<string, unknown>) ?? {};
    return { ...merged, ...p };
  });
  await loadSettings();
  return merged;
}

describe('<ProviderContextSettings>', () => {
  beforeEach(async () => {
    await seed();
  });

  it.each([
    ['claude', '90', '85', '200k'],
    ['codex', '80', '75', '272k'],
  ] as const)('renders the two sliders and the standard window for %s', async (provider, standard, extended, size) => {
    const { getByTestId, getByText } = render(ProviderContextSettings, {
      props: { provider },
    });
    expect(getByTestId(`settings-context-${provider}`)).toBeTruthy();
    expect((getByTestId(`settings-context-${provider}-standard-slider`) as HTMLInputElement).value).toBe(standard);
    expect((getByTestId(`settings-context-${provider}-extended-slider`) as HTMLInputElement).value).toBe(extended);
    expect(getByText(size)).toBeTruthy();
  });

  // The 95 row: the native range input clamps to its min/max attributes,
  // but the onchange handler clamps again as a defence-in-depth pass, so
  // the persisted value is within bounds even if a synthetic event pushes
  // outside.
  it.each([
    ['claude', 'standard', '70', { claudeAutoCompactStandardPercent: 70 }],
    ['codex', 'extended', '60', { codexAutoCompactExtendedPercent: 60 }],
    ['claude', 'standard', '95', { claudeAutoCompactStandardPercent: 90 }],
  ] as const)('persists the %s %s slider set to %s on commit', async (provider, slider, value, patch) => {
    const { getByTestId } = render(ProviderContextSettings, {
      props: { provider },
    });
    const input = getByTestId(`settings-context-${provider}-${slider}-slider`) as HTMLInputElement;
    input.value = value;
    await fireEvent.change(input);

    const update = getBindingMock('UpdateSettings');
    expect(update).toBeDefined();
    expect(update!.mock.calls[0][0]).toEqual(patch);
  });

  it('does not render any model picker, save button, or default toggle', async () => {
    const { queryByText } = render(ProviderContextSettings, {
      props: { provider: 'claude' },
    });
    expect(queryByText(/configure for/i)).toBeNull();
    expect(queryByText(/^save$/i)).toBeNull();
    expect(queryByText(/^default$/i)).toBeNull();
  });
});
