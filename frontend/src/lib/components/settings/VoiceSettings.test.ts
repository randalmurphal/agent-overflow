import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import VoiceSettings from './VoiceSettings.svelte';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { pairViewOnly, resetToLocalPage } from '../../../test/helpers/scopes';
import { makeSettings } from '../../../test/helpers/settings';
import { loadSettings } from '../../stores/settings.svelte';
import type { Settings } from '../../types/settings';
import { flushSync } from 'svelte';
import { fakeSpeechEngine } from '../../../test/helpers/speech';
import { readAloudPhase, resetReadAloudForTest, watchReadAloudVoices } from '../../stores/readAloud.svelte';
import {
  readAloudPreferences,
  reloadReadAloudPreferencesForTest,
  setReadAloudPreference,
} from '../../stores/readAloudPreferences.svelte';

async function settleSpeech(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await Promise.resolve();
  flushSync();
}

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
    const toggle = (await findByRole('switch', { name: 'Toggle dictation with ChatGPT' })) as HTMLButtonElement;
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

describe('<VoiceSettings> read aloud', () => {
  let fake: ReturnType<typeof fakeSpeechEngine>;

  beforeEach(async () => {
    resetBindingMocks();
    resetToLocalPage();
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    resetReadAloudForTest();
    fake = fakeSpeechEngine(2);
    watchReadAloudVoices(fake.engine);
    await settleSpeech();
    await seed({});
    setBindingMock('DictationSources', async () => ({ subscription: false, chatGPTSignIn: false }));
  });
  afterEach(() => {
    resetReadAloudForTest();
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    resetToLocalPage();
  });

  it('is this device’s preference: off by default, with the other controls waiting on the switch', async () => {
    const { findByLabelText, getByTestId, getByText } = render(VoiceSettings);
    const toggle = (await findByLabelText('Toggle reading replies aloud')) as HTMLButtonElement;
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    expect(getByText(/Saved on this device, not the computer/)).toBeTruthy();
    expect((getByTestId('read-aloud-voice-select') as HTMLSelectElement).disabled).toBe(true);
    expect((getByTestId('read-aloud-try') as HTMLButtonElement).disabled).toBe(true);

    await fireEvent.click(toggle);
    expect(readAloudPreferences().enabled).toBe(true);
    expect(toggle.getAttribute('aria-checked')).toBe('true');
    expect((getByTestId('read-aloud-voice-select') as HTMLSelectElement).disabled).toBe(false);
    expect((getByTestId('read-aloud-rate-slider') as HTMLInputElement).disabled).toBe(false);
    expect((getByTestId('read-aloud-try') as HTMLButtonElement).disabled).toBe(false);
  });

  it('offers this device’s voices with the system default first, and keeps speed and volume', async () => {
    setReadAloudPreference('enabled', true);
    const { findByTestId, getByTestId } = render(VoiceSettings);
    const select = (await findByTestId('read-aloud-voice-select')) as HTMLSelectElement;
    expect([...select.options].map((o) => o.value)).toEqual(['', 'v0', 'v1']);
    await fireEvent.change(select, { target: { value: 'v1' } });
    expect(readAloudPreferences().voiceId).toBe('v1');

    await fireEvent.input(getByTestId('read-aloud-rate-slider'), { target: { value: '1.5' } });
    await fireEvent.input(getByTestId('read-aloud-volume-slider'), { target: { value: '0.25' } });
    expect(readAloudPreferences()).toMatchObject({ rate: 1.5, volume: 0.25 });
    expect(getByTestId('read-aloud-rate-value').textContent?.trim()).toBe('1.5×');
    expect(getByTestId('read-aloud-volume-value').textContent?.trim()).toBe('25%');
  });

  it('reads a sample with the current settings, stops it on a second press, and on leaving the page', async () => {
    setReadAloudPreference('enabled', true);
    setReadAloudPreference('voiceId', 'v1');
    setReadAloudPreference('rate', 0.8);
    const { findByTestId, unmount } = render(VoiceSettings);
    const tryIt = (await findByTestId('read-aloud-try')) as HTMLButtonElement;
    await fireEvent.click(tryIt);
    expect(fake.playbacks[0]).toMatchObject({ from: 0, options: { voiceId: 'v1', rate: 0.8, volume: 1 } });
    expect(fake.playbacks[0].pieces.join(' ')).toMatch(/read aloud/);
    expect(readAloudPhase('settings-sample')).toBe('playing');
    await expect.poll(() => tryIt.textContent?.trim()).toBe('Stop');

    await fireEvent.click(tryIt);
    expect(fake.playbacks[0].stopped).toBe(true);
    expect(readAloudPhase('settings-sample')).toBe('idle');

    await fireEvent.click(tryIt);
    expect(readAloudPhase('settings-sample')).toBe('playing');
    unmount();
    expect(fake.playbacks[1].stopped).toBe(true);
    expect(readAloudPhase('settings-sample')).toBe('idle');
  });

  it('says so and disables the controls when this device has no voices', async () => {
    resetReadAloudForTest();
    watchReadAloudVoices(fakeSpeechEngine(0).engine);
    await settleSpeech();
    setReadAloudPreference('enabled', true);
    const { findByTestId, getByText, getByLabelText } = render(VoiceSettings);
    const section = await findByTestId('settings-read-aloud-section');
    expect(section.dataset.hasVoices).toBe('false');
    expect(getByText('This device has no voices to read with.')).toBeTruthy();
    expect((getByLabelText('Toggle reading replies aloud') as HTMLButtonElement).disabled).toBe(true);
    expect((await findByTestId('read-aloud-voice-select') as HTMLSelectElement).disabled).toBe(true);
    expect((await findByTestId('read-aloud-try') as HTMLButtonElement).disabled).toBe(true);
  });
});
