import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import VoiceSettings from './VoiceSettings.svelte';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { OBSERVE_SCOPES, pairViewOnly, pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import { makeSettings } from '../../../test/helpers/settings';
import { loadSettings } from '../../stores/settings.svelte';
import type { Settings } from '../../types/settings';
import { flushSync } from 'svelte';
import { fakeSpeechEngine } from '../../../test/helpers/speech';
import {
  readAloudPhase,
  resetReadAloudForTest,
  useOpenAIEngineForTest,
  watchReadAloudVoices,
} from '../../stores/readAloud.svelte';
import { refreshReadAloudSource, resetReadAloudSourcesForTest } from '../../stores/readAloudSource.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { HOST_TIER_REASON } from './settingsComputer';
import type { SpeechEngine } from '../../native/speech';
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

describe('<VoiceSettings> live dictation', () => {
  const keyFile = (state: string, problem?: string) => ({ path: '/data/credentials/openai-api-key', state, problem });

  beforeEach(() => {
    resetBindingMocks();
    resetToLocalPage();
    setBindingMock('DictationSources', async () => ({ subscription: false, live: false, chatGPTSignIn: false }));
    setBindingMock('OpenAIKeyFile', async () => keyFile('present'));
  });
  afterEach(() => {
    resetToLocalPage();
  });

  it('reads the ChatGPT row as Off while only live is on', async () => {
    await seed({ dictationAPIEnabled: true });
    setBindingMock('DictationSources', async () => ({ subscription: false, live: true, chatGPTSignIn: true }));
    const { findByTestId } = render(VoiceSettings);
    await expect.poll(async () => (await findByTestId('voice-dictation-live-status')).textContent?.trim()).toBe('Ready');
    expect((await findByTestId('voice-dictation-status')).textContent?.trim()).toBe('Off');
  });

  it('is off by default with its fields hidden, and turning it on sends only the switch', async () => {
    await seed({});
    const { findByRole, findByTestId, queryByTestId } = render(VoiceSettings);
    const toggle = (await findByRole('switch', { name: 'Toggle live dictation with an OpenAI API key' })) as HTMLButtonElement;
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    await expect.poll(async () => (await findByTestId('voice-dictation-live-status')).textContent?.trim()).toBe('Off');
    expect(queryByTestId('voice-dictation-live-delay')).toBeNull();
    expect(queryByTestId('voice-dictation-live-prompt')).toBeNull();
    expect((await findByTestId('voice-dictation-live-disclosure')).textContent).toMatch(/does not\s+train on API data/);

    await fireEvent.click(toggle);
    expect(getBindingMock('UpdateSettings')!.mock.calls.at(-1)![0]).toEqual({ dictationAPIEnabled: true });
  });

  it('shows the key file with its state, and reads the problem into the status', async () => {
    await seed({ dictationAPIEnabled: true });
    setBindingMock('OpenAIKeyFile', async () => keyFile('refused', 'the key file is readable by others'));
    const { findByTestId, getByRole } = render(VoiceSettings);
    expect((await findByTestId('voice-dictation-live-key-path')).textContent).toBe('/data/credentials/openai-api-key');
    await expect.poll(async () => (await findByTestId('voice-dictation-live-key-line')).textContent?.trim()).toBe('the key file is readable by others');
    await expect.poll(async () => (await findByTestId('voice-dictation-live-status')).textContent?.trim()).toBe('the key file is readable by others');
    expect(getByRole('button', { name: 'Copy key file path' })).toBeTruthy();
    // Nothing here ever shows the key itself: only its path and state.
    expect((await findByTestId('voice-dictation-live-key')).textContent).not.toMatch(/sk-/);
  });

  it('shows the fields when on, sends each as the computer expects, and re-reads the key file on focus', async () => {
    await seed({ dictationAPIEnabled: true, dictationAPIDelay: 'low', dictationAPILanguages: ['en'], dictationAPIKeywords: ['Agent Overflow'] });
    const read = setBindingMock('OpenAIKeyFile', async () => keyFile('missing'));
    const { findByTestId, getByTestId } = render(VoiceSettings);
    await expect.poll(async () => (await findByTestId('voice-dictation-live-status')).textContent?.trim()).toBe('No key file yet.');
    expect((getByTestId('voice-dictation-live-delay') as HTMLSelectElement).value).toBe('low');
    expect((getByTestId('voice-dictation-live-languages') as HTMLInputElement).value).toBe('en');
    expect((getByTestId('voice-dictation-live-keywords') as HTMLTextAreaElement).value).toBe('Agent Overflow');

    const update = getBindingMock('UpdateSettings')!;
    await fireEvent.change(getByTestId('voice-dictation-live-delay'), { target: { value: 'high' } });
    expect(update.mock.calls.at(-1)![0]).toEqual({ dictationAPIDelay: 'high' });
    await fireEvent.change(getByTestId('voice-dictation-live-languages'), { target: { value: 'en, fr de' } });
    expect(update.mock.calls.at(-1)![0]).toEqual({ dictationAPILanguages: ['en', 'fr', 'de'] });
    await fireEvent.change(getByTestId('voice-dictation-live-keywords'), { target: { value: 'Agent Overflow\n\n  Svelte  \n' } });
    expect(update.mock.calls.at(-1)![0]).toEqual({ dictationAPIKeywords: ['Agent Overflow', 'Svelte'] });
    await fireEvent.change(getByTestId('voice-dictation-live-prompt'), { target: { value: 'About Go.' } });
    expect(update.mock.calls.at(-1)![0]).toEqual({ dictationAPIPrompt: 'About Go.' });

    const before = read.mock.calls.length;
    window.dispatchEvent(new Event('focus'));
    await expect.poll(() => read.mock.calls.length).toBe(before + 1);
  });

  it('opens the key file in the editor on this computer and then shows what is there', async () => {
    await seed({ dictationAPIEnabled: true });
    const read = setBindingMock('OpenAIKeyFile', async () => keyFile('missing'));
    const edit = setBindingMock('EditOpenAIKeyFile', async () => {
      read.mockImplementation(async () => keyFile('empty'));
      return keyFile('empty');
    });
    const sources = getBindingMock('DictationSources')!;
    const { findByTestId } = render(VoiceSettings);
    const button = await findByTestId('voice-dictation-live-key-edit');
    const asked = sources.mock.calls.length;
    await fireEvent.click(button);
    expect(edit).toHaveBeenCalledTimes(1);
    await expect.poll(async () => (await findByTestId('voice-dictation-live-key-line')).textContent?.trim()).toBe('The key file is empty.');
    await expect.poll(() => sources.mock.calls.length).toBeGreaterThan(asked);
  });

  it('shows a failed edit beside the row', async () => {
    await seed({ dictationAPIEnabled: true });
    setBindingMock('EditOpenAIKeyFile', async () => { throw new Error('open the OpenAI API key file: no editor'); });
    const { findByTestId } = render(VoiceSettings);
    await fireEvent.click(await findByTestId('voice-dictation-live-key-edit'));
    await expect.poll(async () => (await findByTestId('voice-dictation-live-key-line')).textContent?.trim()).toBe('open the OpenAI API key file: no editor');
  });

  it('tells a remote client where to put the key and offers no editor', async () => {
    await seed({ dictationAPIEnabled: true });
    await pairWithScopes([...OBSERVE_SCOPES, 'threads:operate']);
    const { findByTestId, queryByTestId, getByText } = render(VoiceSettings);
    expect((await findByTestId('voice-dictation-live-key-path')).textContent).toBe('/data/credentials/openai-api-key');
    expect(getByText(/Put the key in this file on that computer/)).toBeTruthy();
    expect(queryByTestId('voice-dictation-live-key-edit')).toBeNull();
    // The tuning fields are host settings, so they are as inert here as
    // the switch, and changing one writes nothing.
    const update = getBindingMock('UpdateSettings')!;
    const writes = update.mock.calls.length;
    for (const id of ['voice-dictation-live-delay', 'voice-dictation-live-languages', 'voice-dictation-live-keywords', 'voice-dictation-live-prompt']) {
      const field = (await findByTestId(id)) as HTMLInputElement;
      expect(field.disabled, id).toBe(true);
    }
    expect(update.mock.calls.length).toBe(writes);
  });

  it('keeps a failed edit on show when a status read lands while it was pending', async () => {
    await seed({ dictationAPIEnabled: true });
    let failEdit!: () => void;
    setBindingMock('EditOpenAIKeyFile', () => new Promise((_resolve, reject) => {
      failEdit = () => reject(new Error('open the OpenAI API key file: no editor'));
    }));
    const { findByTestId } = render(VoiceSettings);
    await fireEvent.click(await findByTestId('voice-dictation-live-key-edit'));
    window.dispatchEvent(new Event('focus'));
    await expect.poll(async () => (await findByTestId('voice-dictation-live-key-line')).textContent?.trim()).toBe('Key present.');
    failEdit();
    await expect.poll(async () => (await findByTestId('voice-dictation-live-key-line')).textContent?.trim()).toBe('open the OpenAI API key file: no editor');
  });

  it('never lets a slow edit reply replace a newer status', async () => {
    await seed({ dictationAPIEnabled: true });
    let state = 'missing';
    setBindingMock('OpenAIKeyFile', async () => keyFile(state));
    let finishEdit!: () => void;
    setBindingMock('EditOpenAIKeyFile', () => new Promise((resolve) => {
      finishEdit = () => resolve(keyFile('empty'));
    }));
    const { findByTestId } = render(VoiceSettings);
    await expect.poll(async () => (await findByTestId('voice-dictation-live-key-line')).textContent?.trim()).toBe('No key file yet.');
    await fireEvent.click(await findByTestId('voice-dictation-live-key-edit'));
    state = 'present';
    window.dispatchEvent(new Event('focus'));
    await expect.poll(async () => (await findByTestId('voice-dictation-live-key-line')).textContent?.trim()).toBe('Key present.');
    finishEdit();
    await new Promise((resolve) => setTimeout(resolve, 0));
    flushSync();
    await expect.poll(async () => (await findByTestId('voice-dictation-live-key-line')).textContent?.trim()).toBe('Key present.');
  });

  it('never lets an older key file read replace a newer one', async () => {
    await seed({ dictationAPIEnabled: true });
    let releaseFirst!: () => void;
    let calls = 0;
    setBindingMock('OpenAIKeyFile', () => {
      calls += 1;
      if (calls === 1) return new Promise((resolve) => { releaseFirst = () => resolve(keyFile('present')); });
      return Promise.resolve(keyFile('missing'));
    });
    const { findByTestId } = render(VoiceSettings);
    await expect.poll(() => calls).toBe(1);
    window.dispatchEvent(new Event('focus'));
    await expect.poll(async () => (await findByTestId('voice-dictation-live-key-line')).textContent?.trim()).toBe('No key file yet.');
    releaseFirst();
    await new Promise((resolve) => setTimeout(resolve, 0));
    flushSync();
    expect((await findByTestId('voice-dictation-live-key-line')).textContent?.trim()).toBe('No key file yet.');
  });
});

describe('<VoiceSettings> OpenAI voices', () => {
  const VOICES = [{ id: 'marin', name: 'Marin' }, { id: 'cedar', name: 'Cedar' }];
  const keyFile = (state: string) => ({ path: '/data/credentials/openai-api-key', state });
  let available: boolean;
  let device: ReturnType<typeof fakeSpeechEngine>;
  let clips: Array<{ backend: string; voice: string; stopped: boolean }>;

  function fakeClipEngine(backend: string): SpeechEngine {
    return {
      voices: async () => [],
      onVoicesChanged: () => () => undefined,
      speak: (_pieces, _from, options) => {
        const record = { backend, voice: options.voiceId, stopped: false };
        clips.push(record);
        let finish!: () => void;
        const done = new Promise<'stopped'>((resolve) => { finish = () => resolve('stopped'); });
        return { done, stop: () => { record.stopped = true; finish(); } };
      },
    };
  }

  async function boot(deviceVoices: number): Promise<void> {
    resetReadAloudForTest();
    device = fakeSpeechEngine(deviceVoices);
    watchReadAloudVoices(device.engine);
    useOpenAIEngineForTest(fakeClipEngine);
    await settleSpeech();
  }

  beforeEach(async () => {
    resetBindingMocks();
    resetToLocalPage();
    resetReadAloudSourcesForTest();
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    available = false;
    clips = [];
    await boot(1);
    setBindingMock('DictationSources', async () => ({ subscription: false, live: false, chatGPTSignIn: false }));
    setBindingMock('OpenAIKeyFile', async () => keyFile('present'));
    setBindingMock('ReadAloudSource', async () => ({ available, voices: VOICES }));
  });
  afterEach(() => {
    resetReadAloudForTest();
    resetReadAloudSourcesForTest();
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    resetToLocalPage();
  });

  const status = async (find: (id: string) => Promise<HTMLElement>) => (await find('voice-read-aloud-api-status')).textContent?.trim();

  it('is off by default, says so, and its switch sends only itself both ways', async () => {
    await seed({});
    const { findByRole, findByTestId } = render(VoiceSettings);
    const toggle = (await findByRole('switch', { name: 'Toggle reading aloud with OpenAI voices' })) as HTMLButtonElement;
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    expect(toggle.disabled).toBe(false);
    await expect.poll(() => status(findByTestId)).toBe('Off');
    const disclosure = (await findByTestId('voice-read-aloud-api-disclosure')).textContent ?? '';
    expect(disclosure).toMatch(/billed to that key/);
    expect(disclosure).toMatch(/up to 30 days/);
    expect(disclosure).toMatch(/AI-generated/);

    const update = getBindingMock('UpdateSettings')!;
    await fireEvent.click(toggle);
    expect(update.mock.calls.at(-1)![0]).toEqual({ readAloudAPIEnabled: true });
    await expect.poll(() => toggle.getAttribute('aria-checked')).toBe('true');
    await fireEvent.click(toggle);
    expect(update.mock.calls.at(-1)![0]).toEqual({ readAloudAPIEnabled: false });
  });

  it('cannot be switched where the host tier is not writable', async () => {
    await seed({});
    await pairWithScopes([...OBSERVE_SCOPES, 'threads:operate']);
    const { findByRole } = render(VoiceSettings);
    const toggle = (await findByRole('switch', { name: 'Toggle reading aloud with OpenAI voices' })) as HTMLButtonElement;
    expect(toggle.disabled).toBe(true);
    expect(toggle.title).toBe(HOST_TIER_REASON);
  });

  it('reads Ready when available, the key file’s problem when it is not, and Checking meanwhile', async () => {
    await seed({ readAloudAPIEnabled: true });
    available = true;
    const ready = render(VoiceSettings);
    await expect.poll(() => status(ready.findByTestId)).toBe('Ready');
    ready.unmount();

    available = false;
    setBindingMock('OpenAIKeyFile', async () => keyFile('missing'));
    const missing = render(VoiceSettings);
    await expect.poll(() => status(missing.findByTestId)).toBe('No key file yet.');
    missing.unmount();

    setBindingMock('OpenAIKeyFile', () => new Promise(() => undefined));
    const checking = render(VoiceSettings);
    await expect.poll(() => status(checking.findByTestId)).toBe('Checking…');
  });

  it('asks nothing of a computer this connection may not operate threads on', async () => {
    await seed({ readAloudAPIEnabled: true });
    await pairViewOnly();
    const read = getBindingMock('ReadAloudSource')!;
    const { queryByTestId, findByRole } = render(VoiceSettings);
    await findByRole('switch', { name: 'Toggle reading aloud with OpenAI voices' });
    await settleSpeech();
    expect(read).not.toHaveBeenCalled();
    expect(queryByTestId('voice-read-aloud-api-status')).toBeNull();
  });

  it('lists the computer’s OpenAI voices in their own group while it offers them', async () => {
    await seed({ readAloudAPIEnabled: true });
    setReadAloudPreference('enabled', true);
    const { findByTestId, queryByTestId } = render(VoiceSettings);
    const select = (await findByTestId('read-aloud-voice-select')) as HTMLSelectElement;
    await settleSpeech();
    expect(queryByTestId('read-aloud-openai-voices')).toBeNull();
    expect(queryByTestId('read-aloud-openai-previews')).toBeNull();

    available = true;
    await refreshReadAloudSource(HOME_BACKEND);
    const group = await findByTestId('read-aloud-openai-voices');
    expect(group.getAttribute('label')).toBe('OpenAI voices on this computer');
    expect([...select.options].map((o) => o.value)).toEqual(['', 'v0', 'openai:marin', 'openai:cedar']);
    await fireEvent.change(select, { target: { value: 'openai:cedar' } });
    expect(readAloudPreferences().voiceId).toBe('openai:cedar');
    expect(queryByTestId('read-aloud-voice-unoffered')).toBeNull();

    available = false;
    await refreshReadAloudSource(HOME_BACKEND);
    await expect.poll(() => queryByTestId('read-aloud-openai-voices')).toBeNull();
    // The saved voice stays selected, with what a reply is read in instead.
    expect(select.value).toBe('openai:cedar');
    expect([...select.options].map((o) => o.textContent?.trim())).toContain('Cedar (OpenAI)');
    expect((await findByTestId('read-aloud-voice-unoffered')).textContent)
      .toMatch(/does not offer Cedar; replies from a computer without it are read in this\s+device’s voice/);
  });

  it('turns on read-aloud on a device with no voices when the computer offers OpenAI voices', async () => {
    await boot(0);
    await seed({ readAloudAPIEnabled: true });
    const { findByLabelText, getByText } = render(VoiceSettings);
    const toggle = (await findByLabelText('Toggle reading replies aloud')) as HTMLButtonElement;
    expect(toggle.disabled).toBe(true);
    expect(getByText('This device has no voices to read with.')).toBeTruthy();

    available = true;
    await refreshReadAloudSource(HOME_BACKEND);
    await expect.poll(() => toggle.disabled).toBe(false);
  });

  it('previews each OpenAI voice without changing the saved voice, switching and stopping', async () => {
    await seed({ readAloudAPIEnabled: true });
    available = true;
    setReadAloudPreference('enabled', true);
    setReadAloudPreference('voiceId', 'v0');
    const { findAllByTestId, findByTestId, unmount } = render(VoiceSettings);
    await findByTestId('read-aloud-openai-previews');
    const [marin, cedar] = await findAllByTestId('read-aloud-openai-preview');
    expect(marin.getAttribute('aria-label')).toBe('Preview Marin');
    expect(marin.textContent?.trim()).toBe('Marin');
    expect(marin.getAttribute('aria-pressed')).toBe('false');

    await fireEvent.click(marin);
    expect(clips).toEqual([{ backend: HOME_BACKEND, voice: 'marin', stopped: false }]);
    expect(readAloudPreferences().voiceId).toBe('v0');
    await expect.poll(() => marin.getAttribute('aria-pressed')).toBe('true');
    expect(marin.getAttribute('aria-label')).toBe('Stop the Marin preview');

    await fireEvent.click(cedar);
    expect(clips[0].stopped).toBe(true);
    expect(clips[1]).toMatchObject({ voice: 'cedar', stopped: false });
    await expect.poll(() => marin.getAttribute('aria-pressed')).toBe('false');
    await expect.poll(() => cedar.getAttribute('aria-pressed')).toBe('true');

    await fireEvent.click(cedar);
    expect(clips[1].stopped).toBe(true);
    expect(readAloudPhase('settings-preview:cedar')).toBe('idle');

    // Try it reads the saved voice, on the device.
    await fireEvent.click(await findByTestId('read-aloud-try'));
    expect(device.playbacks.at(-1)!.options.voiceId).toBe('v0');
    await fireEvent.click(await findByTestId('read-aloud-try'));

    // Leaving the page stops a preview.
    await fireEvent.click(marin);
    expect(clips[2]).toMatchObject({ voice: 'marin', stopped: false });
    unmount();
    expect(clips[2].stopped).toBe(true);
    expect(readAloudPhase('settings-preview:marin')).toBe('idle');
    expect(readAloudPreferences().voiceId).toBe('v0');
  });
});
