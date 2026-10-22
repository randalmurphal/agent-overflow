import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import VoiceSettings from './VoiceSettings.svelte';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { pairViewOnly, resetToLocalPage } from '../../../test/helpers/scopes';
import { makeSettings } from '../../../test/helpers/settings';
import { loadSettings } from '../../stores/settings.svelte';
import type { Settings } from '../../types/settings';

async function seed(overrides: Partial<Settings>): Promise<void> {
  const merged: Settings = { ...makeSettings(), ...overrides };
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => ({ ...merged, ...(patch as object) }));
  await loadSettings();
}

describe('<VoiceSettings>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetToLocalPage();
  });
  afterEach(() => {
    resetToLocalPage();
  });

  it('is off by default and turning it on sends only the switch', async () => {
    await seed({});
    setBindingMock('DictationSources', async () => ({ subscription: false, chatGPTSignIn: true }));
    const { findByRole, findByTestId } = render(VoiceSettings);
    const toggle = (await findByRole('switch')) as HTMLButtonElement;
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    expect((await findByTestId('voice-dictation-status')).textContent?.trim()).toBe('Off');

    await fireEvent.click(toggle);
    expect(getBindingMock('UpdateSettings')!.mock.calls.at(-1)![0]).toEqual({ dictationSubscriptionEnabled: true });
  });

  it('says dictation is ready when it is on and Codex is signed in with ChatGPT', async () => {
    await seed({ dictationSubscriptionEnabled: true });
    setBindingMock('DictationSources', async () => ({ subscription: true, chatGPTSignIn: true }));
    const { findByTestId, getByTestId } = render(VoiceSettings);
    const status = await findByTestId('voice-dictation-status');
    await expect.poll(() => status.textContent?.trim()).toBe('Ready');
    expect(getByTestId('voice-dictation-disclosure').textContent).toMatch(/up to 30 days/);
  });

  it('says what is missing when it is on without a ChatGPT sign-in', async () => {
    await seed({ dictationSubscriptionEnabled: true });
    setBindingMock('DictationSources', async () => ({ subscription: false, chatGPTSignIn: false }));
    const { findByTestId } = render(VoiceSettings);
    const status = await findByTestId('voice-dictation-status');
    await expect.poll(() => status.textContent?.trim()).toBe('Codex is not signed in with ChatGPT');
  });

  it('asks nothing of a computer this connection may not dictate on', async () => {
    await seed({ dictationSubscriptionEnabled: true });
    const read = setBindingMock('DictationSources', async () => ({ subscription: true, chatGPTSignIn: true }));
    await pairViewOnly();
    const { queryByTestId, getByText } = render(VoiceSettings);
    expect(queryByTestId('voice-dictation-status')).toBeNull();
    expect(getByText(/does not have permission to dictate/)).toBeTruthy();
    expect(read).not.toHaveBeenCalled();
  });
});
