import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ProviderSettings from './ProviderSettings.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';

const BASE_SETTINGS: Settings = {
  theme: 'system',
  timestampFormat: 'locale',
  defaultProvider: 'claude',
  defaultModelClaude: 'claude-opus-4-7',
  defaultModelCodex: 'gpt-5.5',
  modelContextWindows: {},
  recentWorkspaces: [],
  diffWordWrap: false,
  showEndOfTurnDiffs: true,
  backgroundTrayExpanded: false,
  streamingEnabled: true,
  confirmArchive: true,
  confirmDelete: true,
  claudeBinaryPath: 'claude',
  codexBinaryPath: 'codex',
  claudeEnabled: true,
  codexEnabled: true,
  defaultMode: 'chat',
  defaultRuntimeMode: 'full-access',
  defaultThreadEnvMode: 'local',
  worktreeBranchPrefix: 'ao-',
  defaultReasoningEffort: 'high',
  defaultFastMode: false,
  defaultContextWindow: 1000000,
  textGenerationProvider: 'codex',
  textGenerationModel: '',
  textGenerationReasoningEffort: 'low',
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
  setBindingMock('GetProviderStatuses', async () => []);
  setBindingMock('GetModelsForProvider', async () => []);
  await loadSettings();
  return merged;
}

describe('<ProviderSettings> — Text generation section', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the text-generation section with all three controls', async () => {
    const { getByTestId } = render(ProviderSettings);
    expect(getByTestId('settings-text-generation')).toBeTruthy();
    expect(getByTestId('settings-textgen-provider')).toBeTruthy();
    expect(getByTestId('settings-textgen-model')).toBeTruthy();
    expect(getByTestId('settings-textgen-effort')).toBeTruthy();
  });

  it('lists codex and claude as the provider options', async () => {
    const { getByTestId } = render(ProviderSettings);
    const select = getByTestId('settings-textgen-provider') as HTMLSelectElement;
    const values = Array.from(select.options).map((o) => o.value);
    expect(values.sort()).toEqual(['claude', 'codex']);
  });

  it('shows the codex default model in the placeholder when provider is codex', async () => {
    await seed({ textGenerationProvider: 'codex' });
    const { getByTestId } = render(ProviderSettings);
    const input = getByTestId('settings-textgen-model') as HTMLInputElement;
    expect(input.placeholder).toContain('gpt-5.4-mini');
  });

  it('shows the claude default model in the placeholder when provider is claude', async () => {
    await seed({ textGenerationProvider: 'claude' });
    const { getByTestId } = render(ProviderSettings);
    const input = getByTestId('settings-textgen-model') as HTMLInputElement;
    expect(input.placeholder).toContain('claude-haiku-4-5');
  });

  it('dispatches textGenerationProvider patch on change', async () => {
    const { getByTestId } = render(ProviderSettings);
    const select = getByTestId('settings-textgen-provider') as HTMLSelectElement;
    select.value = 'claude';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ textGenerationProvider: 'claude' });
  });

  it('dispatches textGenerationModel patch on change', async () => {
    const { getByTestId } = render(ProviderSettings);
    const input = getByTestId('settings-textgen-model') as HTMLInputElement;
    input.value = 'gpt-5.4';
    await fireEvent.change(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ textGenerationModel: 'gpt-5.4' });
  });

  it('lists all five reasoning-effort tiers', async () => {
    const { getByTestId } = render(ProviderSettings);
    const select = getByTestId('settings-textgen-effort') as HTMLSelectElement;
    const values = Array.from(select.options).map((o) => o.value);
    expect(values).toEqual(['low', 'medium', 'high', 'xhigh', 'max']);
  });

  it('dispatches textGenerationReasoningEffort patch on change', async () => {
    const { getByTestId } = render(ProviderSettings);
    const select = getByTestId('settings-textgen-effort') as HTMLSelectElement;
    select.value = 'medium';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ textGenerationReasoningEffort: 'medium' });
  });
});
