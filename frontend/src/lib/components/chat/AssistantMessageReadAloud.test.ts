// The read-aloud control on a finished reply: shown only where this device
// offers it, in the meta row beside Copy, and reading as play, pause or
// resume-plus-restart with the store's phase for that reply.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import AssistantMessage from './AssistantMessage.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { fakeSpeechEngine } from '../../../test/helpers/speech';
import {
  readAloudPhase,
  replyReadingKey,
  resetReadAloudForTest,
  useOpenAIEngineForTest,
  watchReadAloudVoices,
} from '../../stores/readAloud.svelte';
import { refreshReadAloudSource, resetReadAloudSourcesForTest } from '../../stores/readAloudSource.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { HOME_BACKEND } from '../../transport/backendKey';
import { reloadReadAloudPreferencesForTest, setReadAloudPreference } from '../../stores/readAloudPreferences.svelte';
import { __resetEntityIndexForTest, noteThread } from '../../transport/entityIndex';
import { REMOTE_BACKEND_UUID, resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import type { SpeechEngine } from '../../native/speech';

async function settle(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await Promise.resolve();
  flushSync();
}

describe('<AssistantMessage> read aloud', () => {
  let fake: ReturnType<typeof fakeSpeechEngine>;

  beforeEach(async () => {
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    resetReadAloudForTest();
    fake = fakeSpeechEngine();
    watchReadAloudVoices(fake.engine);
    setReadAloudPreference('enabled', true);
    await settle();
  });

  afterEach(() => {
    resetReadAloudForTest();
    resetReadAloudSourcesForTest();
    resetStagedBackends();
    __resetEntityIndexForTest();
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
  });

  it('shows no control while read-aloud is off or the device has no voice', async () => {
    setReadAloudPreference('enabled', false);
    const off = render(AssistantMessage, { props: { item: makeItem({ summary: 'The answer.' }) } });
    await waitFor(() => expect(off.getByTestId('assistant-message-copy-slot')).toBeTruthy());
    expect(off.queryByTestId('assistant-message-read-aloud')).toBeNull();
    off.unmount();

    setReadAloudPreference('enabled', true);
    resetReadAloudForTest();
    watchReadAloudVoices(fakeSpeechEngine(0).engine);
    await settle();
    const mute = render(AssistantMessage, { props: { item: makeItem({ summary: 'The answer.' }) } });
    await waitFor(() => expect(mute.getByTestId('assistant-message-copy-slot')).toBeTruthy());
    expect(mute.queryByTestId('assistant-message-read-aloud')).toBeNull();
  });

  it('takes the copy gate: no control on a streaming or empty reply', async () => {
    const streaming = render(AssistantMessage, {
      props: { item: makeItem({ summary: 'Still coming', status: 'streaming' }) },
    });
    await waitFor(() => expect(streaming.getByTestId('assistant-message-meta')).toBeTruthy());
    expect(streaming.queryByTestId('assistant-message-read-aloud')).toBeNull();
    streaming.unmount();

    const blank = render(AssistantMessage, { props: { item: makeItem({ summary: '   ' }) } });
    expect(blank.queryByTestId('assistant-message-read-aloud')).toBeNull();
  });

  // A narration row is prose the reader sees with a Copy control, so it is
  // read on request like any reply; nothing reads a row unasked.
  it('offers a summarized narration row and reads its text', async () => {
    const item = makeItem({ id: 'note-1', summary: 'Checked the config. Running the tests next.', meta: '{"narration":true}' });
    const { getByTestId } = render(AssistantMessage, { props: { item } });
    await waitFor(() => expect(getByTestId('assistant-message-summarized')).toBeTruthy());
    await fireEvent.click(getByTestId('assistant-message-read-aloud'));
    expect(readAloudPhase(replyReadingKey('thread-1', 'note-1'))).toBe('playing');
    expect(fake.playbacks[0].pieces).toEqual(['Checked the config.', 'Running the tests next.']);
  });

  it('reads the reply, pauses, resumes from its sentence and restarts from the first', async () => {
    const item = makeItem({ id: 'reply-1', summary: 'First sentence. Second sentence.\n\n```js\nskipped();\n```\n\nThird sentence.' });
    const { getByTestId, queryByTestId } = render(AssistantMessage, { props: { item } });
    const button = await waitFor(() => getByTestId('assistant-message-read-aloud'));
    expect(button.getAttribute('aria-label')).toBe('Read aloud');
    expect(queryByTestId('assistant-message-read-aloud-restart')).toBeNull();

    await fireEvent.click(button);
    expect(readAloudPhase(replyReadingKey('thread-1', 'reply-1'))).toBe('playing');
    expect(fake.playbacks[0].pieces).toEqual(['First sentence.', 'Second sentence.', 'Third sentence.']);
    await waitFor(() => expect(button.getAttribute('aria-label')).toMatch(/^Pause reading/));

    fake.playbacks[0].onPiece(1);
    await fireEvent.click(button);
    await settle();
    expect(readAloudPhase(replyReadingKey('thread-1', 'reply-1'))).toBe('paused');
    await waitFor(() => expect(button.getAttribute('aria-label')).toMatch(/^Resume reading/));
    const restart = getByTestId('assistant-message-read-aloud-restart');
    expect(restart.getAttribute('aria-label')).toBe('Read from the start');

    await fireEvent.click(button);
    expect(fake.playbacks[1].from).toBe(1);
    await waitFor(() => expect(queryByTestId('assistant-message-read-aloud-restart')).toBeNull());

    await fireEvent.click(button);
    await settle();
    await fireEvent.click(getByTestId('assistant-message-read-aloud-restart'));
    expect(fake.playbacks[2].from).toBe(0);
    expect(readAloudPhase(replyReadingKey('thread-1', 'reply-1'))).toBe('playing');
  });

  it('stays visible while its reply is being read, and hides with the hover chrome otherwise', async () => {
    const { getByTestId, container } = render(AssistantMessage, {
      props: { item: makeItem({ id: 'reply-2', summary: 'A sentence.' }) },
    });
    const button = await waitFor(() => getByTestId('assistant-message-read-aloud'));
    const reveal = (): HTMLElement => container.querySelector('[data-testid="assistant-message-read-aloud-slot"]')!.parentElement!;
    expect(reveal().classList.contains('opacity-0')).toBe(true);

    await fireEvent.click(button);
    await waitFor(() => expect(reveal().classList.contains('opacity-100')).toBe(true));

    await fireEvent.click(button);
    await settle();
    expect(readAloudPhase(replyReadingKey('thread-1', 'reply-2'))).toBe('paused');
    await waitFor(() => expect(reveal().classList.contains('opacity-100')).toBe(true));

    // Reading another reply returns this one to the hover chrome.
    fake.playbacks[0].finish('stopped');
    await fireEvent.click(button);
    fake.playbacks.at(-1)!.finish('ended');
    await settle();
    await waitFor(() => expect(reveal().classList.contains('opacity-0')).toBe(true));
  });

  it('tells apart replies with the same item id in different threads', async () => {
    const a = within(render(AssistantMessage, { props: { item: makeItem({ id: 'text:0:1', threadId: 'thread-a', summary: 'From A.' }) } }).container);
    const b = within(render(AssistantMessage, { props: { item: makeItem({ id: 'text:0:1', threadId: 'thread-b', summary: 'From B.' }) } }).container);
    const buttonA = await waitFor(() => a.getByTestId('assistant-message-read-aloud'));
    const buttonB = await waitFor(() => b.getByTestId('assistant-message-read-aloud'));

    await fireEvent.click(buttonA);
    await settle();
    expect(b.getByTestId('assistant-message-read-aloud-slot').getAttribute('data-phase')).toBe('idle');

    await fireEvent.click(buttonB);
    await settle();
    expect(fake.playbacks.at(-1)!.pieces).toEqual(['From B.']);
    expect(a.getByTestId('assistant-message-read-aloud-slot').getAttribute('data-phase')).toBe('idle');
    expect(buttonA.getAttribute('aria-label')).toBe('Read aloud');
  });

  it('offers the control through the computer’s OpenAI voice on a device with none, and waits for its first sentence', async () => {
    resetReadAloudForTest();
    watchReadAloudVoices(fakeSpeechEngine(0).engine);
    const clips: Array<{ voice: string; onPiece: (index: number) => void }> = [];
    useOpenAIEngineForTest(() => ({
      voices: async () => [],
      onVoicesChanged: () => () => undefined,
      speak: (_pieces, _from, options, onPiece) => {
        clips.push({ voice: options.voiceId, onPiece });
        return { done: new Promise(() => undefined), stop: () => undefined };
      },
    }));
    await settle();
    const item = makeItem({ id: 'reply-4', summary: 'A sentence.' });
    const mute = render(AssistantMessage, { props: { item } });
    await waitFor(() => expect(mute.getByTestId('assistant-message-copy-slot')).toBeTruthy());
    expect(mute.queryByTestId('assistant-message-read-aloud')).toBeNull();
    mute.unmount();

    setBindingMock('ReadAloudSource', async () => ({ available: true, voices: [{ id: 'marin', name: 'Marin' }] }));
    await refreshReadAloudSource(HOME_BACKEND);
    const { getByTestId } = render(AssistantMessage, { props: { item } });
    const button = await waitFor(() => getByTestId('assistant-message-read-aloud'));
    await fireEvent.click(button);
    expect(clips.map((c) => c.voice)).toEqual(['marin']);
    const slot = getByTestId('assistant-message-read-aloud-slot');
    await waitFor(() => expect(slot.getAttribute('data-waiting')).toBe('true'));
    expect(slot.getAttribute('data-phase')).toBe('playing');
    expect(button.querySelector('svg')?.getAttribute('viewBox')).toBe('0 0 24 24');
    clips[0].onPiece(0);
    await waitFor(() => expect(slot.getAttribute('data-waiting')).toBe('false'));
    expect(button.getAttribute('aria-label')).toMatch(/^Pause reading/);
  });

  // Reading in an OpenAI voice spends the computer's API key, so a thread
  // two computers claim never reads through either of them.
  it('reads a contested thread in the device voice and asks no computer for an OpenAI voice', async () => {
    const minted: string[] = [];
    const clipEngine = (backend: string): SpeechEngine => ({
      voices: async () => [],
      onVoicesChanged: () => () => undefined,
      speak: () => {
        minted.push(backend);
        return { done: new Promise(() => undefined), stop: () => undefined };
      },
    });
    useOpenAIEngineForTest(clipEngine);
    setBindingMock('ReadAloudSource', async () => ({ available: true, voices: [{ id: 'marin', name: 'Marin' }] }));
    await refreshReadAloudSource(HOME_BACKEND);
    setReadAloudPreference('voiceId', 'openai:marin');
    stageBackend({ id: 'laptop', backendId: REMOTE_BACKEND_UUID });
    noteThread('thread-c', HOME_BACKEND, 1);
    noteThread('thread-c', 'laptop', 1);

    const item = makeItem({ id: 'reply-c', threadId: 'thread-c', summary: 'A sentence.' });
    const contested = render(AssistantMessage, { props: { item } });
    await fireEvent.click(await waitFor(() => contested.getByTestId('assistant-message-read-aloud')));
    expect(readAloudPhase(replyReadingKey('thread-c', 'reply-c'))).toBe('playing');
    expect(fake.playbacks[0].options.voiceId).toBe('');
    expect(minted).toEqual([]);
    contested.unmount();

    // With no device voice there is nothing to read it with.
    resetReadAloudForTest();
    watchReadAloudVoices(fakeSpeechEngine(0).engine);
    useOpenAIEngineForTest(clipEngine);
    await settle();
    const mute = render(AssistantMessage, { props: { item } });
    await waitFor(() => expect(mute.getByTestId('assistant-message-copy-slot')).toBeTruthy());
    expect(mute.queryByTestId('assistant-message-read-aloud')).toBeNull();
    mute.unmount();

    // A thread HOME verifiably owns reads in its OpenAI voice.
    const owned = makeItem({ id: 'reply-o', threadId: 'thread-o', summary: 'A sentence.' });
    noteThread('thread-o', HOME_BACKEND, 1);
    const verified = render(AssistantMessage, { props: { item: owned } });
    await fireEvent.click(await waitFor(() => verified.getByTestId('assistant-message-read-aloud')));
    expect(minted).toEqual([HOME_BACKEND]);
  });

  // A reading through a computer belongs to that computer only while it
  // verifiably owns the thread.
  it('stops a paused OpenAI reading once its thread is contested, and reads again in the device voice from the start', async () => {
    const minted: string[] = [];
    useOpenAIEngineForTest((backend: string): SpeechEngine => ({
      voices: async () => [],
      onVoicesChanged: () => () => undefined,
      speak: (_pieces, _from, _options, onPiece) => {
        minted.push(backend);
        onPiece(_from);
        return { done: new Promise(() => undefined), stop: () => undefined };
      },
    }));
    setBindingMock('ReadAloudSource', async () => ({ available: true, voices: [{ id: 'marin', name: 'Marin' }] }));
    await refreshReadAloudSource(HOME_BACKEND);
    setReadAloudPreference('voiceId', 'openai:marin');
    noteThread('thread-p', HOME_BACKEND, 1);

    const item = makeItem({ id: 'reply-p', threadId: 'thread-p', summary: 'First sentence. Second sentence.' });
    const { getByTestId } = render(AssistantMessage, { props: { item } });
    const button = await waitFor(() => getByTestId('assistant-message-read-aloud'));
    const slot = getByTestId('assistant-message-read-aloud-slot');
    await fireEvent.click(button);
    expect(minted).toEqual([HOME_BACKEND]);
    await fireEvent.click(button);
    await settle();
    await waitFor(() => expect(slot.getAttribute('data-phase')).toBe('paused'));

    stageBackend({ id: 'laptop', backendId: REMOTE_BACKEND_UUID });
    noteThread('thread-p', 'laptop', 1);
    await waitFor(() => expect(slot.getAttribute('data-phase')).toBe('idle'));

    await fireEvent.click(getByTestId('assistant-message-read-aloud'));
    expect(readAloudPhase(replyReadingKey('thread-p', 'reply-p'))).toBe('playing');
    expect(fake.playbacks[0]).toMatchObject({ from: 0, options: { voiceId: '' } });
    expect(minted).toEqual([HOME_BACKEND]);
  });

  it('returns to idle when speech is cancelled from outside', async () => {
    const { getByTestId } = render(AssistantMessage, { props: { item: makeItem({ id: 'reply-3', summary: 'A sentence.' }) } });
    const button = await waitFor(() => getByTestId('assistant-message-read-aloud'));
    await fireEvent.click(button);
    fake.playbacks[0].finish('stopped');
    await settle();
    await waitFor(() => expect(button.getAttribute('aria-label')).toBe('Read aloud'));
  });
});
