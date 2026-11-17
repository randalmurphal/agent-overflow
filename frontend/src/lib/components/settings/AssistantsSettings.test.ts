import { describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import AssistantsSettings from './AssistantsSettings.svelte';
import ComputerSettingsPage from './ComputerSettingsPage.svelte';
import { applySettingsSnapshot, getSettings, loadSettings, resetSettingsForTest } from '../../stores/settings.svelte';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { __resetRunModeForTest } from '../../transport/runMode';
import { HOME_BACKEND } from '../../transport/backendKey';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import type { Settings } from '../../types/settings';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import { assistantsOn } from '../../stores/assistants.svelte';

async function seed(overrides: Partial<Settings> = {}): Promise<void> {
  const saved = makeSettings(overrides);
  setBindingMock('GetSettings', async () => saved);
  setBindingMock('UpdateSettings', async (patch: unknown) => ({ ...saved, ...(patch as Partial<Settings>) }));
  await loadSettings();
}

// The Assistants switch is this computer's user-tier boolean, on by
// default: the page shows the saved value and sends the one key.
describe('<AssistantsSettings>', () => {
  it('is on by default', async () => {
    await seed();
    const view = render(AssistantsSettings);
    const toggle = (await view.findByRole('switch')) as HTMLButtonElement;
    expect(toggle.getAttribute('aria-checked')).toBe('true');
    expect(toggle.getAttribute('aria-label')).toBe('Toggle assistants');
  });

  it('sends assistantsEnabled false when turned off and true when turned on', async () => {
    await seed();
    const view = render(AssistantsSettings);
    await fireEvent.click(await view.findByRole('switch'));
    expect(getBindingMock('UpdateSettings')!.mock.calls.at(-1)![0]).toEqual({ assistantsEnabled: false });

    await seed({ assistantsEnabled: false });
    const toggle = (await view.findByRole('switch')) as HTMLButtonElement;
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    await fireEvent.click(toggle);
    expect(getBindingMock('UpdateSettings')!.mock.calls.at(-1)![0]).toEqual({ assistantsEnabled: true });
  });

  // The page edits the computer picked at the top of Settings, not this
  // one: its switch shows and writes the laptop's setting.
  it('shows and changes the picked computer\'s switch', async () => {
    await seed();
    stageBackend();
    try {
      applySettingsSnapshot(makeSettings({ assistantsEnabled: false }), 'laptop');
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: AssistantsSettings, needsComputer: false } });
      const toggle = (await view.findByRole('switch')) as HTMLButtonElement;
      expect(toggle.getAttribute('aria-checked')).toBe('false');
      await fireEvent.click(toggle);
      expect(getSettings('laptop').assistantsEnabled).toBe(true);
      expect(getSettings(HOME_BACKEND).assistantsEnabled).toBe(true);
    } finally {
      resetStagedBackends();
      resetSettingsForTest();
    }
  });

  // A computer on a release before assistants (v0.0.19) names no
  // assistantsEnabled: it has no assistants, nothing offers to make one
  // there, and its page says why instead of writing a key it lacks.
  it('offers no assistants on a computer whose release has none', async () => {
    await seed();
    stageBackend();
    try {
      const { assistantsEnabled: _absent, ...older } = makeSettings();
      applySettingsSnapshot(older, 'laptop');
      expect(assistantsOn('laptop')).toBe(false);
      expect(assistantsOn(HOME_BACKEND)).toBe(true);
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: AssistantsSettings, needsComputer: false } });
      const toggle = (await view.findByRole('switch')) as HTMLButtonElement;
      expect(toggle.getAttribute('aria-checked')).toBe('false');
      expect(toggle.disabled).toBe(true);
      expect(view.getByTestId('assistants-older-computer').textContent).toBe('Laptop runs an older version: it has no assistants.');
      await fireEvent.click(toggle);
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
    } finally {
      resetStagedBackends();
      resetSettingsForTest();
    }
  });

  // A standalone frontend's own home is an administrative controller with
  // no assistants: its page cannot write the switch there.
  it('writes nothing to a standalone frontend\'s own home', async () => {
    await seed();
    window.history.replaceState(null, '', `${window.location.pathname}?mode=frontend`);
    __resetRunModeForTest();
    try {
      const view = render(ComputerSettingsPage, { props: { backend: HOME_BACKEND, Page: AssistantsSettings, needsComputer: false } });
      const toggle = (await view.findByRole('switch')) as HTMLButtonElement;
      expect(toggle.disabled).toBe(true);
      await fireEvent.click(toggle);
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
    } finally {
      window.history.replaceState(null, '', window.location.pathname);
      __resetRunModeForTest();
    }
  });

  // User tier (internal/settings/tier.go): a networked page with no host
  // presence and no passkey keeps the switch live.
  it('stays writable off the host without a passkey', async () => {
    await seed();
    setPageGrantsFromBootstrap(true);
    try {
      const view = render(AssistantsSettings);
      const toggle = (await view.findByRole('switch')) as HTMLButtonElement;
      expect(toggle.disabled).toBe(false);
    } finally {
      setPageGrantsFromBootstrap(false);
    }
  });
});
