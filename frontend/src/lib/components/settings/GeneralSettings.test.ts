import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import GeneralSettings from './GeneralSettings.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';

const BASE_SETTINGS: Settings = {
  theme: 'system',
  timestampFormat: 'locale',
  sansFont: 'geist',
  monoFont: 'geist',
  recentWorkspaces: [],
  diffWordWrap: false,
  backgroundTrayExpanded: false,
  streamingEnabled: true,
  confirmArchive: true,
  confirmDelete: true,
  claudeBinaryPath: 'claude',
  codexBinaryPath: 'codex',
  claudeEnabled: true,
  codexEnabled: true,
  defaultThreadEnvMode: 'local',
  worktreeBranchPrefix: 'ao-',
  textGenerationProvider: 'codex',
  textGenerationModel: '',
  textGenerationReasoningEffort: 'low',
  claudeAutoCompactStandardPercent: 90,
  claudeAutoCompactExtendedPercent: 90,
  codexAutoCompactStandardPercent: 90,
  codexAutoCompactExtendedPercent: 90,
  observabilityTracingEnabled: false,
  observabilityOtlpEndpoint: '',
  observabilityEventLogEnabled: false,
  network: { bindAll: false },
};

async function seed(overrides: Partial<Settings> = {}): Promise<Settings> {
  const merged: Settings = { ...BASE_SETTINGS, ...overrides };
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => {
    const p = (patch as Record<string, unknown>) ?? {};
    return { ...merged, ...p };
  });
  await loadSettings();
  return merged;
}

describe('<GeneralSettings> — Thread defaults section', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the workspace seed settings without chat default controls', async () => {
    const { getByTestId } = render(GeneralSettings);
    expect(getByTestId('settings-thread-defaults')).toBeTruthy();
    expect(getByTestId('settings-default-thread-env-mode')).toBeTruthy();
    expect(getByTestId('settings-worktree-branch-prefix')).toBeTruthy();
  });

  it('dispatches defaultThreadEnvMode patch on change', async () => {
    const { getByTestId } = render(GeneralSettings);
    const select = getByTestId('settings-default-thread-env-mode') as HTMLSelectElement;
    select.value = 'worktree';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ defaultThreadEnvMode: 'worktree' });
  });

  it('dispatches worktreeBranchPrefix patch on blur', async () => {
    const { getByTestId } = render(GeneralSettings);
    const input = getByTestId('settings-worktree-branch-prefix') as HTMLInputElement;
    input.value = 'task-';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ worktreeBranchPrefix: 'task-' });
  });
});

describe('<GeneralSettings> — Font selectors', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders both font selectors with the default values', async () => {
    const { getByTestId } = render(GeneralSettings);
    const sansSelect = getByTestId('settings-sans-font') as HTMLSelectElement;
    const monoSelect = getByTestId('settings-mono-font') as HTMLSelectElement;
    expect(sansSelect.value).toBe('geist');
    expect(monoSelect.value).toBe('geist');
    expect(sansSelect.querySelector('option[value="hack-nerd"]')).toBeTruthy();
    expect(monoSelect.querySelector('option[value="hack-nerd"]')).toBeTruthy();
    expect(sansSelect.querySelector('option[value="system"]')).toBeTruthy();
    expect(monoSelect.querySelector('option[value="system"]')).toBeTruthy();
  });

  it('dispatches sansFont patch on change', async () => {
    const { getByTestId } = render(GeneralSettings);
    const select = getByTestId('settings-sans-font') as HTMLSelectElement;
    select.value = 'hack-nerd';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ sansFont: 'hack-nerd' });
  });

  it('dispatches monoFont patch on change', async () => {
    const { getByTestId } = render(GeneralSettings);
    const select = getByTestId('settings-mono-font') as HTMLSelectElement;
    select.value = 'system';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ monoFont: 'system' });
  });
});
