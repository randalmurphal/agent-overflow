import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import SettingsView from './SettingsView.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
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
  observabilityTracingEnabled: false,
  observabilityOtlpEndpoint: '',
  observabilityEventLogEnabled: false,
};

async function seed(): Promise<void> {
  setBindingMock('GetSettings', async () => BASE_SETTINGS);
  setBindingMock('UpdateSettings', async () => BASE_SETTINGS);
  setBindingMock('GetProviderStatuses', async () => []);
  setBindingMock('GetModelsForProvider', async () => []);
  setBindingMock('ListDiscussions', async () => []);
  setBindingMock('GetKeybindings', async () => ({}));
  setBindingMock('ListThreads', async () => []);
  await loadSettings();
}

describe('<SettingsView> observability tab', () => {
  beforeEach(async () => {
    await seed();
  });

  it('includes an Observability tab in the nav', async () => {
    const { getByRole } = render(SettingsView, { onClose: vi.fn() });
    const tab = getByRole('tab', { name: 'Observability' });
    expect(tab).toBeInTheDocument();
  });

  it('switches to the Observability panel when clicked', async () => {
    const { getByRole, findByLabelText } = render(SettingsView, { onClose: vi.fn() });
    const tab = getByRole('tab', { name: 'Observability' });
    await fireEvent.click(tab);

    // The OTLP endpoint input is unique to the observability panel.
    const endpoint = await findByLabelText('OTLP endpoint');
    expect(endpoint).toBeInTheDocument();
  });

  it('keeps General as the default active tab', () => {
    const { getByRole } = render(SettingsView, { onClose: vi.fn() });
    const general = getByRole('tab', { name: 'General' });
    expect(general.getAttribute('aria-selected')).toBe('true');
  });
});
