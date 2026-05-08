import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import SettingsView from './SettingsView.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetKeybindingsStore } from '../../stores/keybindings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
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

async function seed(): Promise<void> {
  setBindingMock('GetSettings', async () => BASE_SETTINGS);
  setBindingMock('UpdateSettings', async () => BASE_SETTINGS);
  setBindingMock('GetProviderStatuses', async () => []);
  setBindingMock('GetModelsForProvider', async () => []);
  setBindingMock('ListDiscussions', async () => []);
  setBindingMock('GetKeybindings', async () => []);
  setBindingMock('ListThreads', async () => []);
  resetKeybindingsStore();
  await loadSettings();
}

describe('<SettingsView> tabs', () => {
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

  it('renders the Keybindings panel when multiple chords target the same command context', async () => {
    setBindingMock('GetKeybindings', async () => [
      {
        key: 'mod+n',
        command: 'thread.new',
        when: '!terminalFocus',
        defaultId: 'thread.new.primary',
        defaultKey: 'mod+n',
      },
      {
        key: 'mod+shift+o',
        command: 'thread.new',
        when: '!terminalFocus',
        defaultId: 'thread.new.alternate',
        defaultKey: 'mod+shift+o',
      },
    ]);

    const { getByRole, findAllByText, findByRole } = render(SettingsView, { onClose: vi.fn() });
    const tab = getByRole('tab', { name: 'Keybindings' });
    await fireEvent.click(tab);

    expect(await findByRole('heading', { name: 'Keybindings' })).toBeInTheDocument();
    expect(await findAllByText('thread.new')).toHaveLength(2);
    expect(getByRole('button', { name: 'Ctrl+N' })).toBeInTheDocument();
    expect(getByRole('button', { name: 'Ctrl+Shift+O' })).toBeInTheDocument();
    expect(getByRole('tab', { name: 'Keybindings' }).getAttribute('aria-selected')).toBe('true');
  });

  it('keeps General as the default active tab', () => {
    const { getByRole } = render(SettingsView, { onClose: vi.fn() });
    const general = getByRole('tab', { name: 'General' });
    expect(general.getAttribute('aria-selected')).toBe('true');
  });
});
