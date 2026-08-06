import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import SettingsView from './SettingsView.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetKeybindingsStore } from '../../stores/keybindings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';
import {
  SETTINGS_SECTION_GROUPS,
  SETTINGS_SECTION_IDS,
  SETTINGS_SECTIONS,
} from './sections';

const BASE_SETTINGS: Settings = makeSettings();

async function seed(): Promise<void> {
  setBindingMock('GetSettings', async () => BASE_SETTINGS);
  setBindingMock('UpdateSettings', async () => BASE_SETTINGS);
  setBindingMock('Version', async () => '0.0.1');
  setBindingMock('GetProviderStatuses', async () => []);
  setBindingMock('GetModelsForProvider', async () => []);
  setBindingMock('ListDiscussions', async () => []);
  setBindingMock('GetKeybindings', async () => ({ bindings: [] }));
  setBindingMock('ListThreads', async () => []);
  setBindingMock('ListArchivedThreads', async () => []);
  resetKeybindingsStore();
  await loadSettings();
}

describe('settings section map', () => {
  it('groups every section under exactly one nav cluster, in render order', () => {
    expect(SETTINGS_SECTION_GROUPS.map((g) => g.label)).toEqual([
      'App',
      'Agents',
      'Workspace',
      'Data',
    ]);
    expect(SETTINGS_SECTION_GROUPS.map((g) => g.sections.map((s) => s.id))).toEqual([
      ['general', 'keybindings', 'updates'],
      ['providers', 'discussions'],
      ['projects', 'git', 'editor', 'network'],
      ['observability', 'storage'],
    ]);
  });

  it('derives the keyboard-nav order from the same grouped list', () => {
    expect(SETTINGS_SECTION_IDS).toHaveLength(SETTINGS_SECTIONS.length);
    expect(new Set(SETTINGS_SECTION_IDS)).toEqual(new Set(SETTINGS_SECTIONS.map((s) => s.id)));
  });
});

describe('<SettingsView> tabs', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders every section as a tab plus a decorative group label', async () => {
    const { getAllByRole, getByText } = render(SettingsView, { onClose: vi.fn() });
    expect(getAllByRole('tab')).toHaveLength(SETTINGS_SECTIONS.length);
    for (const group of SETTINGS_SECTION_GROUPS) {
      const label = getByText(group.label);
      expect(label.getAttribute('aria-hidden')).toBe('true');
      // Group labels must not be tabs, or roving focus would land on them.
      expect(label.getAttribute('role')).toBeNull();
    }
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

  it('renders the Git panel with the sync toggle and the GitLab host editor', async () => {
    const { getByRole, findByTestId } = render(SettingsView, { onClose: vi.fn() });
    await fireEvent.click(getByRole('tab', { name: 'Git' }));

    expect(await findByTestId('settings-git-sync')).toBeInTheDocument();
    expect(await findByTestId('settings-gitlab-hosts')).toBeInTheDocument();
  });

  it('renders the Storage panel with retention above the archive list', async () => {
    const { getByRole, findByTestId } = render(SettingsView, { onClose: vi.fn() });
    await fireEvent.click(getByRole('tab', { name: 'Storage' }));

    const retention = await findByTestId('settings-retention');
    const archived = await findByTestId('settings-archived-threads');
    expect(retention.compareDocumentPosition(archived)).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    );
  });

  it('renders the Keybindings panel when multiple chords target the same command context', async () => {
    setBindingMock('GetKeybindings', async () => ({
      bindings: [
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
      ],
    }));

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

  it('rovers arrow/Home/End across group boundaries', async () => {
    const { getByRole } = render(SettingsView, { onClose: vi.fn() });
    const general = getByRole('tab', { name: 'General' });

    // General → Keybindings → Updates → Providers: the third press crosses
    // from the App cluster into Agents without stopping on a group label.
    await fireEvent.keyDown(general, { key: 'ArrowDown' });
    await fireEvent.keyDown(getByRole('tab', { name: 'Keybindings' }), { key: 'ArrowDown' });
    await fireEvent.keyDown(getByRole('tab', { name: 'Updates' }), { key: 'ArrowDown' });
    expect(getByRole('tab', { name: 'Providers' }).getAttribute('aria-selected')).toBe('true');

    await fireEvent.keyDown(getByRole('tab', { name: 'Providers' }), { key: 'End' });
    expect(getByRole('tab', { name: 'Storage' }).getAttribute('aria-selected')).toBe('true');

    // ArrowDown wraps forward off the last tab back to the first.
    await fireEvent.keyDown(getByRole('tab', { name: 'Storage' }), { key: 'ArrowDown' });
    expect(getByRole('tab', { name: 'General' }).getAttribute('aria-selected')).toBe('true');

    await fireEvent.keyDown(getByRole('tab', { name: 'General' }), { key: 'ArrowUp' });
    expect(getByRole('tab', { name: 'Storage' }).getAttribute('aria-selected')).toBe('true');

    await fireEvent.keyDown(getByRole('tab', { name: 'Storage' }), { key: 'Home' });
    expect(getByRole('tab', { name: 'General' }).getAttribute('aria-selected')).toBe('true');
  });
});
