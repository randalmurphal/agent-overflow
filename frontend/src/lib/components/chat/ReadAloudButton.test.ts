// The meta row's read-aloud controls: one play button while idle; while the
// reply is being read or paused, previous, back, play / pause, next and
// (paused) restart, each moving the reading through the store, with back
// and next disabled where they have nothing to do.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import ReadAloudButton from './ReadAloudButton.svelte';
import { fakeClipEngines, fakeSpeechEngine } from '../../../test/helpers/speech';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { HOME_BACKEND } from '../../transport/backendKey';
import {
  pauseReadAloud,
  readAloud,
  readAloudPhase,
  readAloudPosition,
  resetReadAloudForTest,
  useOpenAIEngineForTest,
  watchReadAloudVoices,
} from '../../stores/readAloud.svelte';
import { reloadReadAloudPreferencesForTest, setReadAloudPreference } from '../../stores/readAloudPreferences.svelte';
import { refreshReadAloudSource, resetReadAloudSourcesForTest } from '../../stores/readAloudSource.svelte';

// Three sections, split by headings; the device reads them as 2, 1 and 3 sentences.
const REPLY = '## One\n\nFirst part here.\n\n## Two\n\n## Three\n\nLast part. Really last.';
const KEY = 'reply:thread-1:r1';
const OWNER = { computer: HOME_BACKEND };

async function settle(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await Promise.resolve();
  flushSync();
}

describe('<ReadAloudButton>', () => {
  let device: ReturnType<typeof fakeSpeechEngine>;
  let clips: ReturnType<typeof fakeClipEngines>;

  beforeEach(async () => {
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    setReadAloudPreference('enabled', true);
    resetReadAloudForTest();
    resetReadAloudSourcesForTest();
    device = fakeSpeechEngine();
    watchReadAloudVoices(device.engine);
    clips = fakeClipEngines();
    useOpenAIEngineForTest(clips.factory);
    setBindingMock('ReadAloudSource', async () => ({ available: true, voices: [{ id: 'marin', name: 'Marin' }] }));
    await refreshReadAloudSource(HOME_BACKEND);
    await settle();
  });

  afterEach(() => {
    resetReadAloudForTest();
    resetReadAloudSourcesForTest();
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
  });

  const view = () => render(ReadAloudButton, { props: { readingKey: KEY, markdown: REPLY, threadId: 'thread-1' } });
  const button = (v: ReturnType<typeof view>, id: string) => v.getByTestId(`assistant-message-read-aloud${id}`) as HTMLButtonElement;
  const cluster = (v: ReturnType<typeof view>) =>
    Array.from(v.container.querySelectorAll('button')).map((b) => b.getAttribute('data-testid'));

  async function readOpenAI(at: number | null = 0): Promise<void> {
    readAloud(KEY, REPLY, OWNER, 'openai:marin');
    if (at !== null) clips.playbacks.at(-1)!.onPiece(at);
    await settle();
  }

  it('is one play button while idle', async () => {
    const v = view();
    expect(cluster(v)).toEqual(['assistant-message-read-aloud']);
    expect(button(v, '').getAttribute('aria-label')).toBe('Read aloud');
    await fireEvent.click(button(v, ''));
    expect(readAloudPhase(KEY)).toBe('playing');
    expect(device.playbacks[0].from).toBe(0);
  });

  it('shows the cluster in order while reading, back disabled while the first clip is fetched', async () => {
    const v = view();
    await readOpenAI(null);
    expect(cluster(v)).toEqual([
      'assistant-message-read-aloud-previous',
      'assistant-message-read-aloud-back',
      'assistant-message-read-aloud',
      'assistant-message-read-aloud-next',
    ]);
    expect(v.getByTestId('assistant-message-read-aloud-slot').getAttribute('data-waiting')).toBe('true');
    expect(button(v, '-back').disabled).toBe(true);
    expect(button(v, '-previous').disabled).toBe(false);
    expect(button(v, '-next').disabled).toBe(false);
    expect(button(v, '-previous').getAttribute('aria-label')).toBe('Previous section');
    expect(button(v, '-next').getAttribute('aria-label')).toBe('Next section');
  });

  it('moves back 10 seconds, to the next and to the previous section through an OpenAI voice', async () => {
    const v = view();
    await readOpenAI(0);
    const back = button(v, '-back');
    expect(back.disabled).toBe(false);
    expect(back.getAttribute('aria-label')).toBe('Back 10 seconds');
    const playback = clips.playbacks[0];
    await fireEvent.click(back);
    expect(playback.calls).toEqual(['seek 10']);

    // In the first section previous goes to its start.
    await fireEvent.click(button(v, '-previous'));
    expect(playback.calls.at(-1)).toBe('skip 0');
    await fireEvent.click(button(v, '-next'));
    expect(playback.calls.at(-1)).toBe('skip 1');
    expect(readAloudPosition(KEY)?.section).toBe(1);
    playback.onPiece(1);
    await settle();

    // In a middle section both move by one.
    await fireEvent.click(button(v, '-next'));
    expect(playback.calls.at(-1)).toBe('skip 2');
    playback.onPiece(2);
    await settle();
    await fireEvent.click(button(v, '-previous'));
    expect(playback.calls.at(-1)).toBe('skip 1');
  });

  it('disables next in the last section', async () => {
    const v = view();
    await readOpenAI(2);
    expect(readAloudPosition(KEY)).toEqual({ section: 2, sections: 3 });
    expect(button(v, '-next').disabled).toBe(true);
    expect(button(v, '-previous').disabled).toBe(false);
    await fireEvent.click(button(v, '-next'));
    expect(clips.playbacks[0].calls).toEqual([]);
  });

  it('keeps back while an OpenAI reading is paused, with restart last', async () => {
    const v = view();
    await readOpenAI(1);
    pauseReadAloud();
    await settle();
    expect(cluster(v)).toEqual([
      'assistant-message-read-aloud-previous',
      'assistant-message-read-aloud-back',
      'assistant-message-read-aloud',
      'assistant-message-read-aloud-next',
      'assistant-message-read-aloud-restart',
    ]);
    expect(button(v, '').getAttribute('aria-label')).toMatch(/^Resume reading/);
    await fireEvent.click(button(v, '-back'));
    expect(clips.playbacks[0].calls).toEqual(['pause', 'seek 10']);
    expect(readAloudPhase(KEY)).toBe('paused');
  });

  it('repeats the sentence through a device voice, and disables back while it is paused', async () => {
    const v = view();
    await fireEvent.click(button(v, ''));
    device.playbacks[0].onPiece(1);
    await settle();
    const back = button(v, '-back');
    expect(back.getAttribute('aria-label')).toBe('Repeat this sentence');
    expect(back.textContent?.trim()).toBe('');
    expect(back.disabled).toBe(false);
    await fireEvent.click(back);
    expect(device.playbacks[0].stopped).toBe(true);
    expect(device.playbacks[1].from).toBe(1);

    await fireEvent.click(button(v, '-next'));
    expect(device.playbacks.at(-1)!.from).toBe(2);
    await fireEvent.click(button(v, '-previous'));
    expect(device.playbacks.at(-1)!.from).toBe(0);

    await fireEvent.click(button(v, ''));
    await settle();
    expect(readAloudPhase(KEY)).toBe('paused');
    expect(button(v, '-back').disabled).toBe(true);
  });

  it('returns to one play button when the reading ends', async () => {
    const v = view();
    await readOpenAI(2);
    clips.playbacks[0].finish('ended');
    await settle();
    expect(cluster(v)).toEqual(['assistant-message-read-aloud']);
  });
});
