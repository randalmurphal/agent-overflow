// The clip engine: sentences fetched as audio clips and played in order on
// one element, started on silence inside the gesture, one clip fetched
// ahead, and nothing left behind once a playback is over.

import { describe, expect, it } from 'vitest';
import { CLIP_PLAYBACK_FAILED, clipSpeechEngine, type ClipAudio, type FetchClip } from './clipSpeech';
import type { SpeechOptions } from './speech';

type AudioEvent = 'ended' | 'error';

function fakeAudio() {
  const listeners: Record<AudioEvent, Set<() => void>> = { ended: new Set(), error: new Set() };
  const plays: Array<{ src: string; resolve: () => void; reject: (err: Error) => void }> = [];
  const log: string[] = [];
  const audio = {
    src: '',
    volume: 1,
    playbackRate: 1,
    defaultPlaybackRate: 1,
    preservesPitch: false,
    paused: true,
    play(): Promise<void> {
      log.push(`play ${audio.src}`);
      audio.paused = false;
      return new Promise<void>((resolve, reject) => plays.push({ src: audio.src, resolve, reject }));
    },
    pause(): void {
      log.push('pause');
      audio.paused = true;
    },
    load(): void {
      log.push('load');
    },
    removeAttribute(name: string): void {
      if (name === 'src') audio.src = '';
    },
    addEventListener(type: AudioEvent, listener: () => void): void {
      listeners[type].add(listener);
    },
    removeEventListener(type: AudioEvent, listener: () => void): void {
      listeners[type].delete(listener);
    },
  };
  return {
    audio: audio as typeof audio & ClipAudio,
    plays,
    log,
    listenerCount: () => listeners.ended.size + listeners.error.size,
    emit: (type: AudioEvent) => {
      for (const listener of [...listeners[type]]) listener();
    },
    /** Start the clip play() was last called on. */
    startLatest: async () => {
      plays.at(-1)!.resolve();
      await settle();
    },
  };
}

function fakeUrls() {
  let next = 0;
  const live = new Map<string, Blob>();
  const revoked: string[] = [];
  return {
    live,
    revoked,
    urls: {
      create: (blob: Blob) => {
        next += 1;
        const url = `blob:clip-${next}`;
        live.set(url, blob);
        return url;
      },
      revoke: (url: string) => {
        live.delete(url);
        revoked.push(url);
      },
    },
    urlOf: (blob: Blob) => [...live].find(([, b]) => b === blob)?.[0],
  };
}

interface Fetch {
  text: string;
  voice: string;
  signal: AbortSignal;
  blob: Blob;
  resolve: () => Promise<void>;
  reject: (err: Error) => Promise<void>;
}

function fakeFetcher() {
  const fetches: Fetch[] = [];
  const fetchClip: FetchClip = (text, voice, signal) => new Promise<Blob>((resolve, reject) => {
    const blob = new Blob([text], { type: 'audio/wav' });
    fetches.push({
      text,
      voice,
      signal,
      blob,
      resolve: async () => {
        resolve(blob);
        await settle();
      },
      reject: async (err) => {
        reject(err);
        await settle();
      },
    });
  });
  return { fetches, fetchClip };
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
}

const OPTIONS: SpeechOptions = { voiceId: 'marin', rate: 1.5, volume: 0.4 };

function setup() {
  const element = fakeAudio();
  const urls = fakeUrls();
  const fetcher = fakeFetcher();
  const engine = clipSpeechEngine({ voices: () => [], fetchClip: fetcher.fetchClip, audio: () => element.audio, urls: urls.urls });
  const pieces: number[] = [];
  const speak = (sentences: string[], from = 0) => engine.speak(sentences, from, OPTIONS, (index) => pieces.push(index));
  return { element, urls, fetcher, engine, pieces, speak };
}

/** Nothing of a playback is left: no listener, object URL or open fetch,
 * and the element is paused with no source. */
function expectReleased(t: ReturnType<typeof setup>): void {
  expect(t.element.listenerCount()).toBe(0);
  expect(t.urls.live.size).toBe(0);
  for (const fetch of t.fetcher.fetches) expect(fetch.signal.aborted, fetch.text).toBe(true);
  expect(t.element.audio.paused).toBe(true);
  expect(t.element.audio.src).toBe('');
}

describe('clipSpeechEngine', () => {
  it('starts the element on silence before anything is awaited, with the speed and volume', () => {
    const t = setup();
    t.speak(['One.', 'Two.']);
    // Synchronously, inside the caller's gesture.
    expect(t.element.plays).toHaveLength(1);
    const unlock = t.urls.live.get(t.element.plays[0].src)!;
    expect(unlock.type).toBe('audio/wav');
    expect(unlock.size).toBe(44 + 2400 * 2);
    expect(t.fetcher.fetches.map((f) => [f.text, f.voice])).toEqual([['One.', 'marin']]);
    expect(t.element.audio).toMatchObject({ volume: 0.4, playbackRate: 1.5, defaultPlaybackRate: 1.5, preservesPitch: true });
  });

  it('plays the sentences in order, reporting each as it sounds and fetching exactly one ahead', async () => {
    const t = setup();
    const playback = t.speak(['One.', 'Two.', 'Three.']);
    const [one] = t.fetcher.fetches;
    await one.resolve();
    // The clip is on the element but has not started: nothing is reported
    // and nothing more is fetched.
    expect(t.element.audio.src).toBe(t.urls.urlOf(one.blob));
    expect(t.pieces).toEqual([]);
    expect(t.fetcher.fetches).toHaveLength(1);
    // The silent clip's URL is released once a clip replaces it.
    expect(t.urls.live.size).toBe(1);

    await t.element.startLatest();
    expect(t.pieces).toEqual([0]);
    expect(t.fetcher.fetches.map((f) => f.text)).toEqual(['One.', 'Two.']);

    const two = t.fetcher.fetches[1];
    await two.resolve();
    // Two arrived while One plays: it waits, and Three is not fetched.
    expect(t.element.audio.src).toBe(t.urls.urlOf(one.blob));
    expect(t.fetcher.fetches).toHaveLength(2);
    expect(t.urls.live.size).toBe(1);

    const oneUrl = t.urls.urlOf(one.blob)!;
    t.element.emit('ended');
    await settle();
    expect(t.urls.revoked).toContain(oneUrl);
    expect(t.element.audio.src).toBe(t.urls.urlOf(two.blob));
    await t.element.startLatest();
    expect(t.pieces).toEqual([0, 1]);
    expect(t.fetcher.fetches.map((f) => f.text)).toEqual(['One.', 'Two.', 'Three.']);

    // Two ends before Three has arrived: the element waits for it.
    t.element.emit('ended');
    await settle();
    expect(t.pieces).toEqual([0, 1]);
    await t.fetcher.fetches[2].resolve();
    await t.element.startLatest();
    expect(t.pieces).toEqual([0, 1, 2]);
    t.element.emit('ended');
    await expect(playback.done).resolves.toBe('ended');
    expectReleased(t);
  });

  it('reads from the sentence it is asked to start at', async () => {
    const t = setup();
    t.speak(['One.', 'Two.', 'Three.'], 1);
    expect(t.fetcher.fetches.map((f) => f.text)).toEqual(['Two.']);
    await t.fetcher.fetches[0].resolve();
    await t.element.startLatest();
    expect(t.pieces).toEqual([1]);
  });

  it('stops mid-fetch: the fetch is aborted and a late clip is never played', async () => {
    const t = setup();
    const playback = t.speak(['One.', 'Two.']);
    const [one] = t.fetcher.fetches;
    playback.stop();
    await expect(playback.done).resolves.toBe('stopped');
    expect(one.signal.aborted).toBe(true);
    await one.resolve();
    expect(t.element.plays).toHaveLength(1);
    expect(t.pieces).toEqual([]);
    expectReleased(t);
  });

  it('stops mid-play: the element pauses and the clip fetched ahead is aborted', async () => {
    const t = setup();
    const playback = t.speak(['One.', 'Two.', 'Three.']);
    await t.fetcher.fetches[0].resolve();
    await t.element.startLatest();
    const ahead = t.fetcher.fetches[1];
    playback.stop();
    await expect(playback.done).resolves.toBe('stopped');
    expect(ahead.signal.aborted).toBe(true);
    expect(t.element.log.at(-2)).toBe('pause');
    // An ended that arrives after the stop changes nothing.
    t.element.emit('ended');
    await settle();
    expect(t.fetcher.fetches).toHaveLength(2);
    expectReleased(t);
  });

  it('rejects with the fetch’s reason when a clip cannot be fetched', async () => {
    const t = setup();
    const playback = t.speak(['One.', 'Two.']);
    await t.fetcher.fetches[0].reject(new Error('OpenAI did not read the text as written'));
    await expect(playback.done).rejects.toThrow('OpenAI did not read the text as written');
    expectReleased(t);
  });

  it('rejects when the fetch ahead fails, once the sentence before it ends', async () => {
    const t = setup();
    const playback = t.speak(['One.', 'Two.']);
    await t.fetcher.fetches[0].resolve();
    await t.element.startLatest();
    await t.fetcher.fetches[1].reject(new Error('this computer is already reading 4 sentences aloud; try again shortly'));
    expect(t.pieces).toEqual([0]);
    t.element.emit('ended');
    await expect(playback.done).rejects.toThrow('this computer is already reading 4 sentences aloud');
    expectReleased(t);
  });

  it('rejects when the element refuses to play a clip, or fails while playing one', async () => {
    const refused = setup();
    const first = refused.speak(['One.']);
    await refused.fetcher.fetches[0].resolve();
    refused.element.plays.at(-1)!.reject(new DOMException('not allowed', 'NotAllowedError'));
    await expect(first.done).rejects.toThrow(CLIP_PLAYBACK_FAILED);
    expectReleased(refused);

    const broken = setup();
    const second = broken.speak(['One.', 'Two.']);
    // An error while the silent clip is on the element is not a failure.
    broken.element.emit('error');
    await settle();
    await broken.fetcher.fetches[0].resolve();
    await broken.element.startLatest();
    broken.element.emit('error');
    await expect(second.done).rejects.toThrow(CLIP_PLAYBACK_FAILED);
    expectReleased(broken);
  });

  it('starting another playback stops this one, and repeated starts and stops leave nothing', async () => {
    const t = setup();
    const first = t.speak(['One.', 'Two.']);
    await t.fetcher.fetches[0].resolve();
    await t.element.startLatest();
    const second = t.speak(['Other.']);
    await expect(first.done).resolves.toBe('stopped');
    expect(t.fetcher.fetches[0].signal.aborted).toBe(true);
    expect(t.fetcher.fetches[1].signal.aborted).toBe(true);
    // The new playback unlocked the element again and fetched its own.
    expect(t.fetcher.fetches.at(-1)!.text).toBe('Other.');
    expect(t.element.listenerCount()).toBe(2);

    for (let i = 0; i < 5; i += 1) t.speak(['Again.']).stop();
    second.stop();
    await expect(second.done).resolves.toBe('stopped');
    expectReleased(t);
  });

  it('a second engine on the same element stops the first engine’s playback', async () => {
    const t = setup();
    const other = clipSpeechEngine({ voices: () => [], fetchClip: t.fetcher.fetchClip, audio: () => t.element.audio, urls: t.urls.urls });
    const first = t.speak(['One.']);
    const second = other.speak(['Two.'], 0, OPTIONS, () => undefined);
    await expect(first.done).resolves.toBe('stopped');
    second.stop();
    await expect(second.done).resolves.toBe('stopped');
    expectReleased(t);
  });
});
