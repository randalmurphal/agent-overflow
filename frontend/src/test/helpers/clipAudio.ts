// Fakes for the clip engine (`native/clipSpeech.ts`): an audio element
// whose plays the test starts, object URLs it can count, a fetcher whose
// clips the test delivers, and timers it fires by hand.

import { expect } from 'vitest';
import { clipSpeechEngine, type ClipAudio, type FetchClip, type Timers } from '../../lib/native/clipSpeech';
import type { SpeechOptions } from '../../lib/native/speech';

type AudioEvent = 'ended' | 'error';

export async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
}

export function fakeAudio() {
  const listeners: Record<AudioEvent, Set<() => void>> = { ended: new Set(), error: new Set() };
  const plays: Array<{ src: string; resolve: () => void; reject: (err: Error) => void }> = [];
  const log: string[] = [];
  const audio = {
    src: '',
    currentTime: 0,
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
      for (const listener of Array.from(listeners[type])) listener();
    },
    /** Start the clip play() was last called on. */
    startLatest: async () => {
      plays.at(-1)!.resolve();
      await settle();
    },
  };
}

export function fakeUrls() {
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

export interface Fetch {
  text: string;
  voice: string;
  signal: AbortSignal;
  blob: Blob;
  resolve: () => Promise<void>;
  reject: (err: Error) => Promise<void>;
}

export function fakeFetcher() {
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
  /** The fetch of `text`; the latest when it was fetched more than once. */
  const of = (text: string): Fetch => fetches.filter((f) => f.text === text).at(-1)!;
  /** Fetches whose clip is still held: not aborted. */
  const open = (): string[] => fetches.filter((f) => !f.signal.aborted).map((f) => f.text);
  return { fetches, fetchClip, of, open };
}

export function fakeTimers() {
  const scheduled: Array<{ fn: () => void; ms: number; cleared: boolean; fired: boolean }> = [];
  const timers: Timers = {
    set: (fn, ms) => {
      const entry = { fn, ms, cleared: false, fired: false };
      scheduled.push(entry);
      return entry;
    },
    clear: (handle) => {
      (handle as { cleared: boolean }).cleared = true;
    },
  };
  const live = () => scheduled.filter((t) => !t.cleared && !t.fired);
  return {
    timers,
    scheduled,
    live,
    /** Fire the one live timer. */
    fire: async () => {
      const [entry] = live();
      expect(live()).toHaveLength(1);
      entry.fired = true;
      entry.fn();
      await settle();
    },
  };
}

export const OPTIONS: SpeechOptions = { voiceId: 'marin', rate: 1.5, volume: 0.4 };

export function setupClipEngine(options: SpeechOptions = OPTIONS) {
  const element = fakeAudio();
  const urls = fakeUrls();
  const fetcher = fakeFetcher();
  const clock = fakeTimers();
  const engine = clipSpeechEngine({
    voices: () => [], fetchClip: fetcher.fetchClip, audio: () => element.audio, urls: urls.urls, timers: clock.timers,
  });
  const pieces: number[] = [];
  const speak = (sections: string[], from = 0) => engine.speak(sections, from, options, (index) => pieces.push(index));
  return { element, urls, fetcher, clock, engine, pieces, speak };
}

export type ClipEngineSetup = ReturnType<typeof setupClipEngine>;

/** Nothing of a playback is left: no listener, object URL, open fetch or
 * timer, and the element is paused with no source. */
export function expectReleased(t: ClipEngineSetup): void {
  expect(t.element.listenerCount()).toBe(0);
  expect(t.urls.live.size).toBe(0);
  for (const fetch of t.fetcher.fetches) expect(fetch.signal.aborted, fetch.text).toBe(true);
  expect(t.clock.live()).toHaveLength(0);
  expect(t.element.audio.paused).toBe(true);
  expect(t.element.audio.src).toBe('');
}
