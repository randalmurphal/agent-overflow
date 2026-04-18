import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import GeneralSettings from './GeneralSettings.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';

const BASE_SETTINGS: Settings = {
  theme: 'system',
  timestampFormat: 'locale',
  defaultProvider: 'claude',
  defaultModelClaude: 'claude-sonnet-4-6',
  defaultModelCodex: 'gpt-5.4',
  recentWorkspaces: [],
  diffWordWrap: false,
  streamingEnabled: true,
  confirmArchive: true,
  confirmDelete: true,
  claudeBinaryPath: 'claude',
  codexBinaryPath: 'codex',
  claudeEnabled: true,
  codexEnabled: true,
  defaultMode: 'chat',
  defaultReasoningEffort: 'high',
  defaultFastMode: false,
  defaultContextWindow: 1000000,
  textGenerationProvider: 'codex',
  textGenerationModel: '',
  textGenerationReasoningEffort: 'low',
  observabilityTracingEnabled: false,
  observabilityOtlpEndpoint: '',
  observabilityEventLogEnabled: false,
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

  it('renders the thread-defaults section with all four controls', async () => {
    const { getByTestId } = render(GeneralSettings);
    expect(getByTestId('settings-thread-defaults')).toBeTruthy();
    expect(getByTestId('settings-default-mode')).toBeTruthy();
    expect(getByTestId('settings-default-effort')).toBeTruthy();
    expect(getByTestId('settings-default-context')).toBeTruthy();
  });

  it('offers chat / plan / design as the mode options (not discussion)', async () => {
    const { getByTestId } = render(GeneralSettings);
    const select = getByTestId('settings-default-mode') as HTMLSelectElement;
    const values = Array.from(select.options).map((o) => o.value);
    expect(values).toEqual(['chat', 'plan', 'design']);
  });

  it('offers all five reasoning-effort tiers', async () => {
    const { getByTestId } = render(GeneralSettings);
    const select = getByTestId('settings-default-effort') as HTMLSelectElement;
    const values = Array.from(select.options).map((o) => o.value);
    expect(values).toEqual(['low', 'medium', 'high', 'xhigh', 'max']);
  });

  it('offers only 200k / 1M context-window tiers', async () => {
    const { getByTestId } = render(GeneralSettings);
    const select = getByTestId('settings-default-context') as HTMLSelectElement;
    const values = Array.from(select.options).map((o) => o.value);
    expect(values).toEqual(['200000', '1000000']);
  });

  it('dispatches defaultMode patch on change', async () => {
    const { getByTestId } = render(GeneralSettings);
    const select = getByTestId('settings-default-mode') as HTMLSelectElement;
    select.value = 'plan';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ defaultMode: 'plan' });
  });

  it('dispatches defaultReasoningEffort patch on change', async () => {
    const { getByTestId } = render(GeneralSettings);
    const select = getByTestId('settings-default-effort') as HTMLSelectElement;
    select.value = 'xhigh';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ defaultReasoningEffort: 'xhigh' });
  });

  it('dispatches defaultContextWindow as a number on change', async () => {
    const { getByTestId } = render(GeneralSettings);
    const select = getByTestId('settings-default-context') as HTMLSelectElement;
    select.value = '200000';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    // Value MUST be a number; the settings patch is persisted into an
    // int column on the Go side and a string would fail validation.
    expect(mock!.mock.calls[0][0]).toEqual({ defaultContextWindow: 200000 });
  });
});
