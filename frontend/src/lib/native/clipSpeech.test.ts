// The clip engine: sections fetched as audio clips and played in order on
// one element, started on silence inside the gesture, the first two fetched
// at once and then one ahead, a pause between sections, and nothing left
// behind once a playback is over. Pause, seek and skip are in
// `clipSpeech.controls.test.ts`.

import { describe, expect, it } from 'vitest';
import { CLIP_PLAYBACK_FAILED, SECTION_PAUSE_MS, clipSpeechEngine } from './clipSpeech';
import { OPTIONS, expectReleased, settle, setupClipEngine } from '../../test/helpers/clipAudio';

describe('clipSpeechEngine', () => {
  it('starts the element on silence before anything is awaited, with the speed and volume', () => {
    const t = setupClipEngine();
    t.speak(['One.', 'Two.', 'Three.']);
    // Synchronously, inside the caller's gesture.
    expect(t.element.plays).toHaveLength(1);
    const unlock = t.urls.live.get(t.element.plays[0].src)!;
    expect(unlock.type).toBe('audio/wav');
    expect(unlock.size).toBe(44 + 2400 * 2);
    expect(t.element.audio).toMatchObject({ volume: 0.4, playbackRate: 1.5, defaultPlaybackRate: 1.5, preservesPitch: true });
  });

  it('fetches the first section and the one after at once', () => {
    const t = setupClipEngine();
    t.speak(['One.', 'Two.', 'Three.']);
    expect(t.fetcher.fetches.map((f) => [f.text, f.voice])).toEqual([['One.', 'marin'], ['Two.', 'marin']]);
    const single = setupClipEngine();
    single.speak(['Only.']);
    expect(single.fetcher.fetches.map((f) => f.text)).toEqual(['Only.']);
  });

  it('plays the sections in order with a pause between, reporting each as it sounds and fetching one ahead', async () => {
    const t = setupClipEngine();
    const playback = t.speak(['One.', 'Two.', 'Three.']);
    const one = t.fetcher.of('One.');
    await one.resolve();
    // The clip is on the element but has not started: nothing is reported.
    expect(t.element.audio.src).toBe(t.urls.urlOf(one.blob));
    expect(t.pieces).toEqual([]);
    expect(t.fetcher.fetches).toHaveLength(2);
    // The silent clip's URL is released once a clip replaces it.
    expect(t.urls.live.size).toBe(1);

    await t.element.startLatest();
    expect(t.pieces).toEqual([0]);
    expect(t.fetcher.fetches.map((f) => f.text)).toEqual(['One.', 'Two.']);

    const two = t.fetcher.of('Two.');
    await two.resolve();
    // Two arrived while One plays: it waits, and Three is not fetched.
    expect(t.element.audio.src).toBe(t.urls.urlOf(one.blob));
    expect(t.fetcher.fetches).toHaveLength(2);

    const oneUrl = t.urls.urlOf(one.blob)!;
    t.element.emit('ended');
    await settle();
    expect(t.urls.revoked).toContain(oneUrl);
    // The pause between sections: nothing plays until it is over.
    expect(t.clock.live().map((timer) => timer.ms)).toEqual([SECTION_PAUSE_MS / OPTIONS.rate]);
    expect(t.element.plays).toHaveLength(2);
    await t.clock.fire();
    expect(t.element.audio.src).toBe(t.urls.urlOf(two.blob));
    await t.element.startLatest();
    expect(t.pieces).toEqual([0, 1]);
    expect(t.fetcher.fetches.map((f) => f.text)).toEqual(['One.', 'Two.', 'Three.']);

    // Two ends before Three has arrived: the element waits for it.
    t.element.emit('ended');
    await t.clock.fire();
    expect(t.pieces).toEqual([0, 1]);
    await t.fetcher.of('Three.').resolve();
    await t.element.startLatest();
    expect(t.pieces).toEqual([0, 1, 2]);
    // The last section ends the playback without a pause after it.
    t.element.emit('ended');
    await expect(playback.done).resolves.toBe('ended');
    expect(t.clock.scheduled).toHaveLength(2);
    expectReleased(t);
  });

  it('scales the pause between sections with the speed', async () => {
    for (const rate of [0.5, 1, 2]) {
      const t = setupClipEngine({ ...OPTIONS, rate });
      t.speak(['One.', 'Two.']);
      await t.fetcher.of('One.').resolve();
      await t.element.startLatest();
      t.element.emit('ended');
      expect(t.clock.live().map((timer) => timer.ms)).toEqual([SECTION_PAUSE_MS / rate]);
    }
  });

  it('holds at most two clips at any time', async () => {
    const t = setupClipEngine();
    const playback = t.speak(['A.', 'B.', 'C.', 'D.']);
    const held = (): number => t.fetcher.open().length;
    for (const text of ['A.', 'B.', 'C.', 'D.']) {
      expect(held()).toBeLessThanOrEqual(2);
      await t.fetcher.of(text).resolve();
      if (text !== 'A.') await t.clock.fire();
      await t.element.startLatest();
      expect(held()).toBeLessThanOrEqual(2);
      expect(t.urls.live.size).toBeLessThanOrEqual(2);
      t.element.emit('ended');
      await settle();
    }
    await expect(playback.done).resolves.toBe('ended');
    expectReleased(t);
  });

  it('reads from the section it is asked to start at', async () => {
    const t = setupClipEngine();
    t.speak(['One.', 'Two.', 'Three.'], 1);
    expect(t.fetcher.fetches.map((f) => f.text)).toEqual(['Two.', 'Three.']);
    await t.fetcher.of('Two.').resolve();
    await t.element.startLatest();
    expect(t.pieces).toEqual([1]);
  });

  it('stops mid-fetch: the fetches are aborted and a late clip is never played', async () => {
    const t = setupClipEngine();
    const playback = t.speak(['One.', 'Two.']);
    playback.stop();
    await expect(playback.done).resolves.toBe('stopped');
    await t.fetcher.of('One.').resolve();
    expect(t.element.plays).toHaveLength(1);
    expect(t.pieces).toEqual([]);
    expectReleased(t);
  });

  it('stops mid-play: the element pauses and the clip fetched ahead is aborted', async () => {
    const t = setupClipEngine();
    const playback = t.speak(['One.', 'Two.', 'Three.']);
    await t.fetcher.of('One.').resolve();
    await t.element.startLatest();
    playback.stop();
    await expect(playback.done).resolves.toBe('stopped');
    expect(t.element.log.at(-2)).toBe('pause');
    // An ended that arrives after the stop changes nothing.
    t.element.emit('ended');
    await settle();
    expect(t.fetcher.fetches).toHaveLength(2);
    expectReleased(t);
  });

  it('stops in the pause between sections: the timer is cancelled and nothing more plays', async () => {
    const t = setupClipEngine();
    const playback = t.speak(['One.', 'Two.']);
    await t.fetcher.of('One.').resolve();
    await t.element.startLatest();
    await t.fetcher.of('Two.').resolve();
    t.element.emit('ended');
    expect(t.clock.live()).toHaveLength(1);
    playback.stop();
    await expect(playback.done).resolves.toBe('stopped');
    expectReleased(t);
  });

  it('rejects with the fetch’s reason when a clip cannot be fetched', async () => {
    const t = setupClipEngine();
    const playback = t.speak(['One.', 'Two.']);
    await t.fetcher.of('One.').reject(new Error('OpenAI did not read the text as written'));
    await expect(playback.done).rejects.toThrow('OpenAI did not read the text as written');
    expectReleased(t);
  });

  it('rejects when the fetch ahead fails, once the section before it ends', async () => {
    const t = setupClipEngine();
    const playback = t.speak(['One.', 'Two.']);
    await t.fetcher.of('One.').resolve();
    await t.element.startLatest();
    await t.fetcher.of('Two.').reject(new Error('this computer is already reading 4 sentences aloud; try again shortly'));
    expect(t.pieces).toEqual([0]);
    t.element.emit('ended');
    await t.clock.fire();
    await expect(playback.done).rejects.toThrow('this computer is already reading 4 sentences aloud');
    expectReleased(t);
  });

  it('rejects when the element refuses to play a clip, or fails while playing one', async () => {
    const refused = setupClipEngine();
    const first = refused.speak(['One.']);
    await refused.fetcher.of('One.').resolve();
    refused.element.plays.at(-1)!.reject(new DOMException('not allowed', 'NotAllowedError'));
    await expect(first.done).rejects.toThrow(CLIP_PLAYBACK_FAILED);
    expectReleased(refused);

    const broken = setupClipEngine();
    const second = broken.speak(['One.', 'Two.']);
    // An error while the silent clip is on the element is not a failure.
    broken.element.emit('error');
    await settle();
    await broken.fetcher.of('One.').resolve();
    await broken.element.startLatest();
    broken.element.emit('error');
    await expect(second.done).rejects.toThrow(CLIP_PLAYBACK_FAILED);
    expectReleased(broken);
  });

  it('starting another playback stops this one, and repeated starts and stops leave nothing', async () => {
    const t = setupClipEngine();
    const first = t.speak(['One.', 'Two.']);
    await t.fetcher.of('One.').resolve();
    await t.element.startLatest();
    const second = t.speak(['Other.']);
    await expect(first.done).resolves.toBe('stopped');
    expect(t.fetcher.of('One.').signal.aborted).toBe(true);
    expect(t.fetcher.of('Two.').signal.aborted).toBe(true);
    // The new playback unlocked the element again and fetched its own.
    expect(t.fetcher.fetches.at(-1)!.text).toBe('Other.');
    expect(t.element.listenerCount()).toBe(2);

    for (let i = 0; i < 5; i += 1) t.speak(['Again.']).stop();
    second.stop();
    await expect(second.done).resolves.toBe('stopped');
    expectReleased(t);
  });

  it('a second engine on the same element stops the first engine’s playback', async () => {
    const t = setupClipEngine();
    const other = clipSpeechEngine({ voices: () => [], fetchClip: t.fetcher.fetchClip, audio: () => t.element.audio, urls: t.urls.urls });
    const first = t.speak(['One.']);
    const second = other.speak(['Two.'], 0, OPTIONS, () => undefined);
    await expect(first.done).resolves.toBe('stopped');
    second.stop();
    await expect(second.done).resolves.toBe('stopped');
    expectReleased(t);
  });
});
