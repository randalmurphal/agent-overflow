import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ProviderContextSettings from './ProviderContextSettings.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import {
  setBindingMock,
  getBindingMock,
} from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';

const BASE_SETTINGS: Settings = {
  theme: 'system',
  timestampFormat: 'locale',
  sansFont: 'geist',
  monoFont: 'geist',
  recentWorkspaces: [],
  diffWordWrap: false,
  streamingEnabled: true,
  confirmArchive: true,
  confirmDelete: true,
  claudeBinaryPath: 'claude',
  codexBinaryPath: 'codex',
  claudeEnabled: true,
  codexEnabled: true,
  defaultThreadEnvMode: 'local',
  worktreeBranchPrefix: 'ao-',
  paneDensity: 'compact',
  textGenerationProvider: 'codex',
  textGenerationModel: '',
  textGenerationReasoningEffort: 'low',
  claudeAutoCompactStandardPercent: 90,
  claudeAutoCompactExtendedPercent: 85,
  codexAutoCompactStandardPercent: 80,
  codexAutoCompactExtendedPercent: 75,
  observabilityTracingEnabled: false,
  observabilityOtlpEndpoint: '',
  observabilityEventLogEnabled: false,
  network: { bindAll: false },
  retention: { days: 30 },
  gitlabSelfHostedHosts: [],
};

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

  it('renders the two sliders for the Claude provider', async () => {
    const { getByTestId } = render(ProviderContextSettings, {
      props: { provider: 'claude' },
    });
    const standard = getByTestId(
      'settings-context-claude-standard-slider',
    ) as HTMLInputElement;
    const extended = getByTestId(
      'settings-context-claude-extended-slider',
    ) as HTMLInputElement;
    expect(standard.value).toBe('90');
    expect(extended.value).toBe('85');
  });

  it('renders the two sliders for the Codex provider', async () => {
    const { getByTestId } = render(ProviderContextSettings, {
      props: { provider: 'codex' },
    });
    const standard = getByTestId(
      'settings-context-codex-standard-slider',
    ) as HTMLInputElement;
    const extended = getByTestId(
      'settings-context-codex-extended-slider',
    ) as HTMLInputElement;
    expect(standard.value).toBe('80');
    expect(extended.value).toBe('75');
  });

  it('shows 200k as the standard window for Claude', async () => {
    const { getByTestId, getByText } = render(ProviderContextSettings, {
      props: { provider: 'claude' },
    });
    expect(getByTestId('settings-context-claude')).toBeTruthy();
    expect(getByText('200k')).toBeTruthy();
  });

  it('shows 272k as the standard window for Codex', async () => {
    const { getByTestId, getByText } = render(ProviderContextSettings, {
      props: { provider: 'codex' },
    });
    expect(getByTestId('settings-context-codex')).toBeTruthy();
    expect(getByText('272k')).toBeTruthy();
  });

  it('persists the standard slider on commit (change event)', async () => {
    const { getByTestId } = render(ProviderContextSettings, {
      props: { provider: 'claude' },
    });
    const slider = getByTestId(
      'settings-context-claude-standard-slider',
    ) as HTMLInputElement;
    slider.value = '70';
    await fireEvent.change(slider);

    const update = getBindingMock('UpdateSettings');
    expect(update).toBeDefined();
    expect(update!.mock.calls[0][0]).toEqual({
      claudeAutoCompactStandardPercent: 70,
    });
  });

  it('persists the extended slider on commit', async () => {
    const { getByTestId } = render(ProviderContextSettings, {
      props: { provider: 'codex' },
    });
    const slider = getByTestId(
      'settings-context-codex-extended-slider',
    ) as HTMLInputElement;
    slider.value = '60';
    await fireEvent.change(slider);

    const update = getBindingMock('UpdateSettings');
    expect(update).toBeDefined();
    expect(update!.mock.calls[0][0]).toEqual({
      codexAutoCompactExtendedPercent: 60,
    });
  });

  it('clamps an out-of-range slider value to 1..90 on commit', async () => {
    const { getByTestId } = render(ProviderContextSettings, {
      props: { provider: 'claude' },
    });
    const slider = getByTestId(
      'settings-context-claude-standard-slider',
    ) as HTMLInputElement;
    // The native range input clamps to its min/max attributes, but the
    // onchange handler clamps again as a defence-in-depth pass; assert
    // the persisted value is within bounds even if a synthetic event
    // pushes outside.
    slider.value = '95';
    await fireEvent.change(slider);

    const update = getBindingMock('UpdateSettings');
    expect(update).toBeDefined();
    const patch = update!.mock.calls[0][0] as Record<string, number>;
    expect(patch.claudeAutoCompactStandardPercent).toBeLessThanOrEqual(90);
    expect(patch.claudeAutoCompactStandardPercent).toBeGreaterThanOrEqual(1);
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
