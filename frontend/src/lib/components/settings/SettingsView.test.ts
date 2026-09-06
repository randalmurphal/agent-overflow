import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import SettingsView from './SettingsView.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { resetKeybindingsStore } from '../../stores/keybindings.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
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
  setBindingMock('ListProviderAccounts', async () => []);
  setBindingMock('ListDiscussions', async () => []);
  setBindingMock('GetKeybindings', async () => ({ bindings: [] }));
  setBindingMock('ListThreads', async () => []);
  setBindingMock('ListArchivedThreads', async () => []);
  // The notifications block below reads the push status on mount.
  setBindingMock('GetPushSenderStatus', async () => ({
    configured: false,
    projectId: '',
    clientEmail: '',
    lastError: '',
    registeredDevices: 0,
  }));
  setBindingMock('GetThemeFiles', async () => ({
    dir: '/tmp/themes',
    themes: [],
    appearance: { mode: 'system', uiTheme: 'default', codeTheme: 'github' },
  }));
  resetKeybindingsStore();
  await loadSettings();
}

describe('settings section map', () => {
  it('groups every page under exactly one nav cluster, in render order', () => {
    expect(SETTINGS_SECTION_GROUPS.map((g) => g.label)).toEqual([
      'Appearance',
      'App',
      'Agents',
      'Workspace',
      'Data',
    ]);
    expect(SETTINGS_SECTION_GROUPS.map((g) => g.sections.map((s) => s.id))).toEqual([
      ['theme', 'typography', 'chat', 'spinner'],
      ['threads', 'performance', 'keybindings', 'notifications', 'updates'],
      ['claude', 'codex', 'commit-messages', 'browser', 'discussions'],
      ['projects', 'git', 'editor', 'systems'],
      ['observability', 'storage'],
    ]);
  });

  it('derives the keyboard-nav order from the same grouped list', () => {
    expect(SETTINGS_SECTION_IDS).toHaveLength(SETTINGS_SECTIONS.length - 1);
    expect(new Set(SETTINGS_SECTION_IDS)).toEqual(new Set(SETTINGS_SECTIONS.filter((s) => s.id !== 'remote').map((s) => s.id)));
  });

  it('gives every page a one-line description for the page header', () => {
    for (const section of SETTINGS_SECTIONS) {
      expect(section.description.length, section.id).toBeGreaterThan(0);
    }
  });
});

describe('<SettingsView> tabs', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders every page as a tab plus a decorative group label', async () => {
    const { getAllByRole, getByText } = render(SettingsView, { onClose: vi.fn() });
    expect(getAllByRole('tab')).toHaveLength(SETTINGS_SECTION_IDS.length);
    for (const group of SETTINGS_SECTION_GROUPS) {
      const label = getByText(group.label);
      expect(label.getAttribute('aria-hidden')).toBe('true');
      // Group labels must not be tabs, or roving focus would land on them.
      expect(label.getAttribute('role')).toBeNull();
    }
  });

  it('renders the active page title and description above the page', async () => {
    const { getByTestId, getByRole } = render(SettingsView, { onClose: vi.fn() });
    const header = getByTestId('settings-page-header');
    expect(header).toHaveTextContent('Theme');
    expect(header).toHaveTextContent('Light or dark mode');

    await fireEvent.click(getByRole('tab', { name: 'Git' }));
    expect(getByTestId('settings-page-header')).toHaveTextContent('Git');
  });

  it('switches to the Observability page when clicked', async () => {
    const { getByRole, findByLabelText } = render(SettingsView, { onClose: vi.fn() });
    await fireEvent.click(getByRole('tab', { name: 'Observability' }));

    // The OTLP endpoint input is unique to the observability page.
    expect(await findByLabelText('OTLP endpoint')).toBeInTheDocument();
  });

  it('renders the Git page with the sync toggle and the GitLab host editor', async () => {
    const { getByRole, findByTestId } = render(SettingsView, { onClose: vi.fn() });
    await fireEvent.click(getByRole('tab', { name: 'Git' }));

    expect(await findByTestId('settings-git-sync')).toBeInTheDocument();
    expect(await findByTestId('settings-gitlab-hosts')).toBeInTheDocument();
  });

  it('renders the Storage page with retention above the archive list', async () => {
    const { getByRole, findByTestId } = render(SettingsView, { onClose: vi.fn() });
    await fireEvent.click(getByRole('tab', { name: 'Storage' }));

    const retention = await findByTestId('settings-retention');
    const archived = await findByTestId('settings-archived-threads');
    expect(retention.compareDocumentPosition(archived)).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    );
  });

  it('renders built-in browser controls', async () => {
    const { getByRole, findByTestId } = render(SettingsView, { onClose: vi.fn() });
    await fireEvent.click(getByRole('tab', { name: 'Browser' }));
    expect(await findByTestId('settings-browser')).toBeInTheDocument();
    expect(getByRole('switch', { name: 'Toggle Built-in Browser Tools' })).toHaveAttribute('aria-checked', 'true');
  });

  it('requires confirmation before clearing browser site data', async () => {
    setBindingMock('ClearBrowserSiteData', async () => undefined);
    const { getByRole, findByRole } = render(SettingsView, { onClose: vi.fn() });
    await fireEvent.click(getByRole('tab', { name: 'Browser' }));
    await fireEvent.click(await findByRole('button', { name: 'Clear site data' }));
    expect(getBindingMock('ClearBrowserSiteData')).not.toHaveBeenCalled();
    await fireEvent.click(getByRole('button', { name: 'Clear now' }));
    expect(getBindingMock('ClearBrowserSiteData')).toHaveBeenCalledOnce();
  });

  it('renders the Keybindings page when multiple chords target the same command context', async () => {
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

    const { getByRole, findAllByText } = render(SettingsView, { onClose: vi.fn() });
    await fireEvent.click(getByRole('tab', { name: 'Keybindings' }));

    expect(getByRole('heading', { name: 'Keybindings' })).toBeInTheDocument();
    expect(await findAllByText('thread.new')).toHaveLength(2);
    expect(getByRole('button', { name: 'Ctrl+N' })).toBeInTheDocument();
    expect(getByRole('button', { name: 'Ctrl+Shift+O' })).toBeInTheDocument();
    expect(getByRole('tab', { name: 'Keybindings' }).getAttribute('aria-selected')).toBe('true');
  });

  it('keeps Theme as the default active tab', () => {
    const { getByRole } = render(SettingsView, { onClose: vi.fn() });
    expect(getByRole('tab', { name: 'Theme' }).getAttribute('aria-selected')).toBe('true');
  });

  it('rovers arrow/Home/End across group boundaries', async () => {
    const { getByRole } = render(SettingsView, { onClose: vi.fn() });

    // Updates → Claude Code crosses from the App cluster into Agents without
    // stopping on a group label.
    await fireEvent.keyDown(getByRole('tab', { name: 'Theme' }), { key: 'End' });
    expect(getByRole('tab', { name: 'Storage' }).getAttribute('aria-selected')).toBe('true');

    await fireEvent.keyDown(getByRole('tab', { name: 'Storage' }), { key: 'Home' });
    expect(getByRole('tab', { name: 'Theme' }).getAttribute('aria-selected')).toBe('true');

    await fireEvent.click(getByRole('tab', { name: 'Updates' }));
    await fireEvent.keyDown(getByRole('tab', { name: 'Updates' }), { key: 'ArrowDown' });
    expect(getByRole('tab', { name: 'Claude Code' }).getAttribute('aria-selected')).toBe('true');

    // ArrowDown wraps forward off the last tab back to the first, ArrowUp back.
    await fireEvent.click(getByRole('tab', { name: 'Storage' }));
    await fireEvent.keyDown(getByRole('tab', { name: 'Storage' }), { key: 'ArrowDown' });
    expect(getByRole('tab', { name: 'Theme' }).getAttribute('aria-selected')).toBe('true');
    await fireEvent.keyDown(getByRole('tab', { name: 'Theme' }), { key: 'ArrowUp' });
    expect(getByRole('tab', { name: 'Storage' }).getAttribute('aria-selected')).toBe('true');
  });
});

describe('<SettingsView> search', () => {
  beforeEach(async () => {
    await seed();
  });

  it('replaces the tabs with results while a query is typed, and restores them on clear', async () => {
    const { getByTestId, queryAllByRole, getAllByTestId, queryByTestId } = render(SettingsView, {
      onClose: vi.fn(),
    });
    const input = getByTestId('settings-search') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'font' } });

    expect(queryAllByRole('tab')).toHaveLength(0);
    const hits = getAllByTestId('settings-search-hit');
    expect(hits[0]).toHaveTextContent('UI font');
    expect(hits[0]).toHaveTextContent('Typography');

    await fireEvent.click(getByTestId('settings-search-clear'));
    expect(queryByTestId('settings-search-results')).toBeNull();
    expect(queryAllByRole('tab')).toHaveLength(SETTINGS_SECTION_IDS.length);
  });

  it('opens the hit page and flashes the field', async () => {
    const { getByTestId, getAllByTestId, container } = render(SettingsView, { onClose: vi.fn() });
    const input = getByTestId('settings-search') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'low power' } });
    await fireEvent.click(getAllByTestId('settings-search-hit')[0]);

    expect(getByTestId('settings-page-header')).toHaveTextContent('Performance');
    await waitFor(() => {
      const field = container.querySelector('[data-settings-field="performance.low-power-mode"]');
      expect(field).not.toBeNull();
      expect(field!.classList.contains('settings-field-flash')).toBe(true);
    });
  });

  it('walks results with the arrow keys and opens the highlighted one on Enter', async () => {
    const { getByTestId, getAllByTestId } = render(SettingsView, { onClose: vi.fn() });
    const input = getByTestId('settings-search') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'font' } });
    await fireEvent.keyDown(input, { key: 'ArrowDown' });
    const hits = getAllByTestId('settings-search-hit');
    expect(hits[1].getAttribute('aria-selected')).toBe('true');
    expect(hits[1]).toHaveTextContent('Code font');

    await fireEvent.keyDown(input, { key: 'Enter' });
    expect(getByTestId('settings-page-header')).toHaveTextContent('Typography');
  });

  it('opens a page hit at the page itself', async () => {
    const { getByTestId, getAllByTestId } = render(SettingsView, { onClose: vi.fn() });
    const input = getByTestId('settings-search') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'keybind' } });
    await fireEvent.click(getAllByTestId('settings-search-hit')[0]);
    expect(getByTestId('settings-page-header')).toHaveTextContent('Keybindings');
  });

  it('clears the query on Escape and keeps the press from reaching the overlay', async () => {
    const { getByTestId, queryAllByRole } = render(SettingsView, { onClose: vi.fn() });
    const input = getByTestId('settings-search') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'font' } });

    const escape = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    input.dispatchEvent(escape);
    expect(escape.defaultPrevented).toBe(true);
    await waitFor(() => expect(queryAllByRole('tab')).toHaveLength(SETTINGS_SECTION_IDS.length));

    // With no query the press is left alone so `settings.close` can act on it.
    const escapeAgain = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    input.dispatchEvent(escapeAgain);
    expect(escapeAgain.defaultPrevented).toBe(false);
  });

  it('says so when nothing matches', async () => {
    const { getByTestId } = render(SettingsView, { onClose: vi.fn() });
    const input = getByTestId('settings-search') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'qwertyuiop' } });
    expect(getByTestId('settings-search-results')).toHaveTextContent('No settings match.');
  });
});
