import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ThreadSettings from './ThreadSettings.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';
import { getSettings } from '../../stores/settings.svelte';

async function seed(overrides: Partial<Settings> = {}): Promise<Settings> {
  const merged = makeSettings(overrides);
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => {
    const p = (patch as Record<string, unknown>) ?? {};
    return { ...merged, ...p };
  });
  await loadSettings();
  return merged;
}

describe('<ThreadSettings> — New threads', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the new-thread seed settings without chat default controls', async () => {
    const { getByTestId } = render(ThreadSettings);
    expect(getByTestId('settings-thread-defaults')).toBeTruthy();
    expect(getByTestId('settings-default-thread-env-mode')).toBeTruthy();
  });

  it('dispatches the auto-pin setting from its default-on state', async () => {
    const { getByRole } = render(ThreadSettings);
    const toggle = getByRole('switch', { name: 'Toggle Auto-Pin New Threads' });
    expect(toggle.getAttribute('aria-checked')).toBe('true');

    await fireEvent.click(toggle);

    expect(getSettings().autoPinNewThreads).toBe(false);
    expect(getBindingMock('UpdateSettings')).not.toHaveBeenCalled();
  });

  it('dispatches defaultThreadEnvMode patch on change', async () => {
    const { getByTestId } = render(ThreadSettings);
    const select = getByTestId('settings-default-thread-env-mode') as HTMLSelectElement;
    select.value = 'worktree';
    await fireEvent.change(select);

    expect(getSettings().defaultThreadEnvMode).toBe('worktree');
    expect(getBindingMock('UpdateSettings')).not.toHaveBeenCalled();
  });
});

describe('<ThreadSettings> — Safety checks', () => {
  beforeEach(async () => {
    await seed();
  });

  it('dispatches confirmArchive and confirmDelete patches', async () => {
    const { getByRole } = render(ThreadSettings);

    await fireEvent.click(getByRole('switch', { name: 'Toggle Archive Confirmation' }));
    expect(getSettings().confirmArchive).toBe(false);

    await fireEvent.click(getByRole('switch', { name: 'Toggle Delete Confirmation' }));
    expect(getSettings().confirmDelete).toBe(false);
    expect(getBindingMock('UpdateSettings')).not.toHaveBeenCalled();
  });
});
