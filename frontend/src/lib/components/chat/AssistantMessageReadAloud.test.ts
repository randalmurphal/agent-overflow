// The read-aloud control on a finished reply: shown only where this device
// offers it, in the meta row beside Copy, and reading as play, pause or
// resume-plus-restart with the store's phase for that reply.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import AssistantMessage from './AssistantMessage.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { fakeSpeechEngine } from '../../../test/helpers/speech';
import { readAloudPhase, resetReadAloudForTest, watchReadAloudVoices } from '../../stores/readAloud.svelte';
import { reloadReadAloudPreferencesForTest, setReadAloudPreference } from '../../stores/readAloudPreferences.svelte';

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

  it('reads the reply, pauses, resumes from its sentence and restarts from the first', async () => {
    const item = makeItem({ id: 'reply-1', summary: 'First sentence. Second sentence.\n\n```js\nskipped();\n```\n\nThird sentence.' });
    const { getByTestId, queryByTestId } = render(AssistantMessage, { props: { item } });
    const button = await waitFor(() => getByTestId('assistant-message-read-aloud'));
    expect(button.getAttribute('aria-label')).toBe('Read aloud');
    expect(queryByTestId('assistant-message-read-aloud-restart')).toBeNull();

    await fireEvent.click(button);
    expect(readAloudPhase('reply-1')).toBe('playing');
    expect(fake.playbacks[0].pieces).toEqual(['First sentence.', 'Second sentence.', 'Third sentence.']);
    await waitFor(() => expect(button.getAttribute('aria-label')).toMatch(/^Pause reading/));

    fake.playbacks[0].onPiece(1);
    await fireEvent.click(button);
    await settle();
    expect(readAloudPhase('reply-1')).toBe('paused');
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
    expect(readAloudPhase('reply-1')).toBe('playing');
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
    expect(readAloudPhase('reply-2')).toBe('paused');
    await waitFor(() => expect(reveal().classList.contains('opacity-100')).toBe(true));

    // Reading another reply returns this one to the hover chrome.
    fake.playbacks[0].finish('stopped');
    await fireEvent.click(button);
    fake.playbacks.at(-1)!.finish('ended');
    await settle();
    await waitFor(() => expect(reveal().classList.contains('opacity-0')).toBe(true));
  });
});
