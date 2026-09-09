import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import SidebarSettings from './SidebarSettings.svelte';
import ProjectSortMenu from '../sidebar/ProjectSortMenu.svelte';
import { getSettings } from '../../stores/settings.svelte';
import { getProjectSortMode, getShowProviderIcons, resetSidebarForTest } from '../../stores/sidebar.svelte';
import { loadSettingsFixture } from '../../../test/helpers/settingsFixture';
import { makeSettings } from '../../../test/helpers/settings';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { readFrontendValue } from '../../stores/frontendStorage';

beforeEach(async () => {
  resetSidebarForTest();
  setBindingMock('GetSettings', async () => makeSettings({ autoPinNewThreads: false }));
  setBindingMock('UpdateSettings', async () => makeSettings());
  await loadSettingsFixture();
});

describe('Sidebar settings', () => {
  it('defaults icons off and persists both directions across remounts without a backend write', async () => {
    let view = render(SidebarSettings);
    const toggle = view.getByRole('switch', { name: 'Toggle Provider Icons' });
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    await fireEvent.click(toggle);
    expect(getShowProviderIcons()).toBe(true);
    expect(readFrontendValue('sidebar:showProviderIcons')).toBe(true);
    view.unmount();

    view = render(SidebarSettings);
    const restored = view.getByRole('switch', { name: 'Toggle Provider Icons' });
    expect(restored.getAttribute('aria-checked')).toBe('true');
    await fireEvent.click(restored);
    expect(getShowProviderIcons()).toBe(false);
    expect(readFrontendValue('sidebar:showProviderIcons')).toBe(false);
    expect(getBindingMock('UpdateSettings')).not.toHaveBeenCalled();
  });

  it('preserves the existing auto-pin preference and edits the same setting', async () => {
    const view = render(SidebarSettings);
    const toggle = view.getByRole('switch', { name: 'Toggle Auto-Pin New Threads' });
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    await fireEvent.click(toggle);
    expect(getSettings().autoPinNewThreads).toBe(true);
  });

  it('keeps project order in sync with the sidebar menu in both directions', async () => {
    const view = render(SidebarSettings);
    const menu = render(ProjectSortMenu);
    const select = view.getByRole('combobox', { name: 'Project order' }) as HTMLSelectElement;
    await fireEvent.change(select, { target: { value: 'manual' } });
    expect(getProjectSortMode()).toBe('manual');
    expect(getSettings().projectSortMode).toBe('manual');

    await fireEvent.click(menu.getByRole('button', { name: 'Sort Projects (Manual)' }));
    await fireEvent.click(menu.getByRole('menuitem', { name: 'Created' }));
    expect(select.value).toBe('createdAt');
    expect(getSettings().projectSortMode).toBe('createdAt');
  });

  it('shows a failed save beside the control and clears it after a successful save', async () => {
    const view = render(SidebarSettings);
    const storage = vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('Storage full');
    });
    try {
      await fireEvent.click(view.getByRole('switch', { name: 'Toggle Provider Icons' }));
      expect(view.getByRole('alert').textContent).toContain('Could not save');
    } finally {
      storage.mockRestore();
    }
    await fireEvent.click(view.getByRole('switch', { name: 'Toggle Provider Icons' }));
    expect(view.queryByRole('alert')).toBeNull();
    expect(readFrontendValue('sidebar:showProviderIcons')).toBe(false);
  });
});
