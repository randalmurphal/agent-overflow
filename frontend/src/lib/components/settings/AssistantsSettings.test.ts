import { describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import AssistantsSettings from './AssistantsSettings.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import type { Settings } from '../../types/settings';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';

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
