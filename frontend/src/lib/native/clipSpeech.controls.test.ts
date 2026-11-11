// The clip engine's controls: pause holds the element and its clips, resume
// continues without fetching again, seekBack moves within the clip
// sounding, and skipTo reuses what is fetched and drops the rest, from
// every state a playback can be in.

import { describe, expect, it } from 'vitest';
import { CLIP_PLAYBACK_FAILED } from './clipSpeech';
import { expectReleased, settle, setupClipEngine, type ClipEngineSetup } from '../../test/helpers/clipAudio';

const SECTIONS = ['One.', 'Two.', 'Three.', 'Four.'];

/** A playback with One sounding. */
async function sounding(t: ClipEngineSetup) {
  const playback = t.speak(SECTIONS);
  await t.fetcher.of('One.').resolve();
  await t.element.startLatest();
  expect(t.pieces).toEqual([0]);
  return playback;
}

describe('clip engine controls', () => {
  it('pause then resume mid-clip continues the same clip without fetching or reporting again', async () => {
    const t = setupClipEngine();
    const playback = await sounding(t);
    const src = t.element.audio.src;
    t.element.audio.currentTime = 4.2;
    playback.pause!();
    expect(t.element.audio.paused).toBe(true);
    // Pausing twice is harmless.
    playback.pause!();
    expect(t.element.log.filter((entry) => entry === 'pause')).toHaveLength(1);

    playback.resume!();
    playback.resume!();
    expect(t.element.plays).toHaveLength(3);
    await t.element.startLatest();
    expect(t.element.audio).toMatchObject({ src, currentTime: 4.2, paused: false });
    expect(t.fetcher.fetches.map((f) => f.text)).toEqual(['One.', 'Two.']);
    expect(t.pieces).toEqual([0]);
    playback.stop();
    expectReleased(t);
  });

  it('pause while fetching holds; the clip arriving while paused waits for resume', async () => {
    const t = setupClipEngine();
    const playback = t.speak(SECTIONS);
    playback.pause!();
    await t.fetcher.of('One.').resolve();
    // Only the silent unlock was ever played.
    expect(t.element.plays).toHaveLength(1);
    expect(t.pieces).toEqual([]);
    playback.resume!();
    await settle();
    expect(t.element.plays).toHaveLength(2);
    expect(t.element.audio.src).toBe(t.urls.urlOf(t.fetcher.of('One.').blob));
    await t.element.startLatest();
    expect(t.pieces).toEqual([0]);
    expect(t.fetcher.fetches).toHaveLength(2);
    playback.stop();
    expectReleased(t);
  });

  it('resume before the clip arrives plays it when it comes', async () => {
    const t = setupClipEngine();
    const playback = t.speak(SECTIONS);
    playback.pause!();
    playback.resume!();
    await t.fetcher.of('One.').resolve();
    await t.element.startLatest();
    expect(t.pieces).toEqual([0]);
    playback.stop();
    expectReleased(t);
  });

  it('a pause that overtakes a starting play does not fail the playback', async () => {
    const t = setupClipEngine();
    const playback = t.speak(SECTIONS);
    await t.fetcher.of('One.').resolve();
    playback.pause!();
    // The browser rejects a play() a pause interrupted.
    t.element.plays.at(-1)!.reject(new DOMException('interrupted', 'AbortError'));
    await settle();
    playback.resume!();
    await settle();
    await t.element.startLatest();
    expect(t.pieces).toEqual([0]);
    playback.stop();
    await expect(playback.done).resolves.toBe('stopped');
  });

  it('pause during the wait between sections cancels it; resume plays the next at once', async () => {
    const t = setupClipEngine();
    const playback = await sounding(t);
    await t.fetcher.of('Two.').resolve();
    t.element.emit('ended');
    expect(t.clock.live()).toHaveLength(1);
    playback.pause!();
    expect(t.clock.live()).toHaveLength(0);
    await settle();
    expect(t.pieces).toEqual([0]);
    playback.resume!();
    await settle();
    await t.element.startLatest();
    expect(t.pieces).toEqual([0, 1]);
    expect(t.clock.live()).toHaveLength(0);
    playback.stop();
    expectReleased(t);
  });

  it('a failed play on resume fails the playback', async () => {
    const t = setupClipEngine();
    const playback = await sounding(t);
    playback.pause!();
    playback.resume!();
    t.element.plays.at(-1)!.reject(new DOMException('not allowed', 'NotAllowedError'));
    await expect(playback.done).rejects.toThrow(CLIP_PLAYBACK_FAILED);
    expectReleased(t);
  });

  it('a fetch that fails while paused fails the playback on resume', async () => {
    const t = setupClipEngine();
    const playback = t.speak(SECTIONS);
    playback.pause!();
    await t.fetcher.of('One.').reject(new Error('OpenAI did not read the text as written'));
    playback.resume!();
    await expect(playback.done).rejects.toThrow('OpenAI did not read the text as written');
    expectReleased(t);
  });

  it('seekBack moves back within the clip sounding, to its start at most, playing or paused', async () => {
    const t = setupClipEngine();
    const idle = t.speak(SECTIONS);
    t.element.audio.currentTime = 3;
    // Nothing sounds yet: a no-op.
    idle.seekBack!(10);
    expect(t.element.audio.currentTime).toBe(3);
    idle.stop();

    const playback = await sounding(t);
    t.element.audio.currentTime = 25;
    playback.seekBack!(10);
    expect(t.element.audio.currentTime).toBe(15);
    playback.seekBack!(10);
    playback.seekBack!(10);
    expect(t.element.audio.currentTime).toBe(0);
    t.element.audio.currentTime = 12;
    playback.pause!();
    playback.seekBack!(10);
    expect(t.element.audio).toMatchObject({ currentTime: 2, paused: true });
    playback.stop();
  });

  it('skipTo forward to a prefetched clip plays it without fetching it again', async () => {
    const t = setupClipEngine();
    const playback = await sounding(t);
    const two = t.fetcher.of('Two.');
    await two.resolve();
    playback.skipTo!(1);
    // One is dropped; Two is reused and Three fetched after it.
    expect(t.fetcher.of('One.').signal.aborted).toBe(true);
    expect(t.fetcher.fetches.filter((f) => f.text === 'Two.')).toHaveLength(1);
    expect(t.fetcher.open()).toEqual(['Two.', 'Three.']);
    await settle();
    expect(t.element.audio.src).toBe(t.urls.urlOf(two.blob));
    await t.element.startLatest();
    expect(t.pieces).toEqual([0, 1]);
    playback.stop();
    expectReleased(t);
  });

  it('skipTo backward fetches the target and the one after, and drops the others', async () => {
    const t = setupClipEngine();
    const playback = t.speak(SECTIONS, 2);
    await t.fetcher.of('Three.').resolve();
    await t.element.startLatest();
    playback.skipTo!(0);
    expect(t.fetcher.open()).toEqual(['One.', 'Two.']);
    expect(t.urls.live.size).toBe(0);
    await t.fetcher.of('One.').resolve();
    await t.element.startLatest();
    expect(t.pieces).toEqual([2, 0]);
    playback.stop();
    expectReleased(t);
  });

  it('skipTo the section sounding plays it again from its start with the clip it has', async () => {
    const t = setupClipEngine();
    const playback = await sounding(t);
    playback.skipTo!(0);
    await settle();
    expect(t.fetcher.fetches.filter((f) => f.text === 'One.')).toHaveLength(1);
    await t.element.startLatest();
    expect(t.pieces).toEqual([0, 0]);
    playback.stop();
    expectReleased(t);
  });

  it('skipTo while paused or in the wait between sections plays the target', async () => {
    const t = setupClipEngine();
    const playback = await sounding(t);
    playback.pause!();
    playback.skipTo!(2);
    await t.fetcher.of('Three.').resolve();
    await t.element.startLatest();
    expect(t.pieces).toEqual([0, 2]);
    expect(t.element.audio.paused).toBe(false);

    t.element.emit('ended');
    expect(t.clock.live()).toHaveLength(1);
    playback.skipTo!(1);
    expect(t.clock.live()).toHaveLength(0);
    await t.fetcher.of('Two.').resolve();
    await t.element.startLatest();
    expect(t.pieces).toEqual([0, 2, 1]);
    playback.stop();
    expectReleased(t);
  });

  it('skipTo twice in a row sounds only the last target', async () => {
    const t = setupClipEngine();
    const playback = await sounding(t);
    playback.skipTo!(2);
    playback.skipTo!(3);
    expect(t.fetcher.open()).toEqual(['Four.']);
    await t.fetcher.of('Three.').resolve();
    await t.fetcher.of('Four.').resolve();
    await t.element.startLatest();
    expect(t.pieces).toEqual([0, 3]);
    playback.stop();
    expectReleased(t);
  });

  it('skipTo past the end ends the playback; a negative index starts from the first', async () => {
    const t = setupClipEngine();
    const playback = await sounding(t);
    playback.skipTo!(-3);
    await settle();
    await t.element.startLatest();
    expect(t.pieces).toEqual([0, 0]);
    playback.skipTo!(SECTIONS.length);
    await expect(playback.done).resolves.toBe('ended');
    expectReleased(t);
  });

  it('a stale clip from before a skip never plays or reports', async () => {
    const t = setupClipEngine();
    const playback = t.speak(SECTIONS);
    playback.skipTo!(2);
    // One was dropped; its late arrival changes nothing.
    await t.fetcher.of('One.').resolve();
    expect(t.element.plays).toHaveLength(1);
    await t.fetcher.of('Three.').resolve();
    await t.element.startLatest();
    expect(t.pieces).toEqual([2]);
    playback.stop();
    expectReleased(t);
  });

  it('a clip dropped by a skip that rejects as aborted does not fail the playback', async () => {
    const t = setupClipEngine();
    const playback = t.speak(SECTIONS);
    playback.skipTo!(2);
    expect(t.fetcher.of('One.').signal.aborted).toBe(true);
    await t.fetcher.of('One.').reject(new DOMException('aborted', 'AbortError'));
    await t.fetcher.of('Three.').resolve();
    await t.element.startLatest();
    expect(t.pieces).toEqual([2]);
    playback.stop();
    await expect(playback.done).resolves.toBe('stopped');
  });

  it('stop releases everything from every state', async () => {
    const states: Array<(t: ClipEngineSetup) => Promise<{ stop(): void; done: Promise<unknown> }>> = [
      async (t) => t.speak(SECTIONS),
      async (t) => sounding(t),
      async (t) => {
        const playback = await sounding(t);
        playback.pause!();
        return playback;
      },
      async (t) => {
        const playback = t.speak(SECTIONS);
        playback.pause!();
        await t.fetcher.of('One.').resolve();
        return playback;
      },
      async (t) => {
        const playback = await sounding(t);
        await t.fetcher.of('Two.').resolve();
        t.element.emit('ended');
        return playback;
      },
      async (t) => {
        const playback = await sounding(t);
        await t.fetcher.of('Two.').resolve();
        t.element.emit('ended');
        playback.pause!();
        return playback;
      },
      async (t) => {
        const playback = await sounding(t);
        playback.skipTo!(2);
        return playback;
      },
    ];
    for (const reach of states) {
      const t = setupClipEngine();
      const playback = await reach(t);
      playback.stop();
      await expect(playback.done).resolves.toBe('stopped');
      expectReleased(t);
      // Controls after the stop do nothing.
      const controls = playback as ReturnType<ClipEngineSetup['speak']>;
      controls.resume!();
      controls.skipTo!(1);
      await settle();
      expectReleased(t);
    }
  });
});
