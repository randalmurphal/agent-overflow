// Read-aloud in voices a computer makes: each piece (a section of a reply,
// `utils/speakableText.ts`) is fetched as a complete audio clip and played
// through one reused audio element, with SECTION_PAUSE_MS of silence
// between pieces.
//
// The element is started inside the click or key that began the reading,
// on a silent clip made here, before anything is awaited. WebKit and
// Android's WebView then let later plays on that element go ahead without
// a gesture, so each piece can start when its clip arrives, and a resume
// or a skip can play without one.
//
// A playback fetches its first piece and the one after at once, and while a
// piece sounds the next is fetched; at most two clips are held, the one
// sounding (or about to) and the next. Pausing holds the element and the
// clips, so resuming continues mid-clip without fetching again; seeking
// back moves within the clip sounding; skipping keeps the clips it can
// reuse and drops the rest. Stopping aborts the fetches, cancels the pause
// between pieces, pauses the element and releases every object URL and
// listener.

import type { SpeechEngine, SpeechOutcome, SpeechPlayback, SpeechVoice } from './speech';

/** Fetches one piece's audio in `voice`. */
export type FetchClip = (text: string, voice: string, signal: AbortSignal) => Promise<Blob>;

/** The part of `HTMLAudioElement` a playback uses. */
export interface ClipAudio {
  src: string;
  currentTime: number;
  volume: number;
  playbackRate: number;
  defaultPlaybackRate: number;
  preservesPitch: boolean;
  play(): Promise<void>;
  pause(): void;
  load(): void;
  removeAttribute(name: string): void;
  addEventListener(type: 'ended' | 'error', listener: () => void): void;
  removeEventListener(type: 'ended' | 'error', listener: () => void): void;
}

export interface ObjectUrls {
  create(blob: Blob): string;
  revoke(url: string): void;
}

export interface ClipSpeechOptions {
  voices: () => SpeechVoice[];
  fetchClip: FetchClip;
  /** The element playbacks share; the page's own by default. */
  audio?: () => ClipAudio;
  urls?: ObjectUrls;
  /** Schedules the pause between pieces; `setTimeout` by default. */
  timers?: Timers;
}

export interface Timers {
  set(fn: () => void, ms: number): unknown;
  clear(handle: unknown): void;
}

/** The silence between two pieces at normal speed; it shortens as the
 * reading speeds up. */
export const SECTION_PAUSE_MS = 850;

const browserTimers: Timers = {
  set: (fn, ms) => setTimeout(fn, ms),
  clear: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
};

/** Why a clip did not sound, as a person reads it after "Read aloud stopped: ". */
export const CLIP_PLAYBACK_FAILED = 'this device could not play the audio';

let pageAudio: ClipAudio | null = null;

function sharedAudio(): ClipAudio {
  pageAudio ??= new Audio() as ClipAudio;
  return pageAudio;
}

const browserUrls: ObjectUrls = {
  create: (blob) => URL.createObjectURL(blob),
  revoke: (url) => URL.revokeObjectURL(url),
};

// The playback on each element; starting another stops it.
const active = new WeakMap<ClipAudio, SpeechPlayback>();

/** A tenth of a second of silence: 24 kHz mono 16-bit PCM in a WAV file. */
export function silentWav(): Blob {
  const samples = 2400;
  const buffer = new ArrayBuffer(44 + samples * 2);
  const view = new DataView(buffer);
  const ascii = (at: number, text: string): void => {
    for (let i = 0; i < text.length; i += 1) view.setUint8(at + i, text.charCodeAt(i));
  };
  ascii(0, 'RIFF');
  view.setUint32(4, 36 + samples * 2, true);
  ascii(8, 'WAVE');
  ascii(12, 'fmt ');
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, 24000, true);
  view.setUint32(28, 48000, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  ascii(36, 'data');
  view.setUint32(40, samples * 2, true);
  return new Blob([buffer], { type: 'audio/wav' });
}

interface Clip {
  controller: AbortController;
  blob: Promise<Blob>;
  url: string | null;
}

export function clipSpeechEngine(options: ClipSpeechOptions): SpeechEngine {
  const audioOf = options.audio ?? sharedAudio;
  const urls = options.urls ?? browserUrls;
  const timers = options.timers ?? browserTimers;
  return {
    voices: async () => options.voices(),
    onVoicesChanged: () => () => undefined,
    speak: (pieces, from, speech, onPiece) => {
      const audio = audioOf();
      active.get(audio)?.stop();

      let settled = false;
      let settle!: (outcome: SpeechOutcome) => void;
      let fail!: (err: Error) => void;
      const done = new Promise<SpeechOutcome>((resolve, reject) => {
        settle = (outcome) => {
          if (settled) return;
          settled = true;
          release();
          resolve(outcome);
        };
        fail = (err) => {
          if (settled) return;
          settled = true;
          release();
          reject(err);
        };
      });

      const clips = new Map<number, Clip>();
      // The piece sounding now (playing or held by a pause); -1 while none is.
      let sounding = -1;
      // The piece to sound next while none is: its clip is being fetched or
      // started, or the pause between pieces is running.
      let pending = from;
      let paused = false;
      let timer: unknown = null;
      // Bumped by everything that changes what should sound; an awaited
      // step that sees another value is stale and does nothing.
      let turn = 0;
      let unlockUrl: string | null = null;

      const fetchClip = (index: number): Clip => {
        let clip = clips.get(index);
        if (clip) return clip;
        const controller = new AbortController();
        const blob = options.fetchClip(pieces[index], speech.voiceId, controller.signal);
        // A clip dropped before it is awaited must not report a rejection.
        blob.catch(() => undefined);
        clip = { controller, blob, url: null };
        clips.set(index, clip);
        return clip;
      };
      // Fetch `index` and the piece after it.
      const fetchFrom = (index: number): void => {
        fetchClip(index);
        if (index + 1 < pieces.length) fetchClip(index + 1);
      };
      const dropClip = (index: number): void => {
        const clip = clips.get(index);
        if (!clip) return;
        clips.delete(index);
        clip.controller.abort();
        if (clip.url) urls.revoke(clip.url);
      };
      const dropUnlock = (): void => {
        if (unlockUrl) urls.revoke(unlockUrl);
        unlockUrl = null;
      };
      const clearTimer = (): void => {
        if (timer === null) return;
        timers.clear(timer);
        timer = null;
      };

      const onEnded = (): void => {
        if (settled || sounding < 0) return;
        const finished = sounding;
        sounding = -1;
        dropClip(finished);
        if (finished + 1 >= pieces.length) {
          settle('ended');
          return;
        }
        pending = finished + 1;
        fetchClip(pending);
        timer = timers.set(() => {
          timer = null;
          void playPending();
        }, SECTION_PAUSE_MS / (speech.rate > 0 ? speech.rate : 1));
      };
      const onError = (): void => {
        if (settled || sounding < 0) return;
        fail(new Error(CLIP_PLAYBACK_FAILED));
      };
      audio.addEventListener('ended', onEnded);
      audio.addEventListener('error', onError);

      function release(): void {
        clearTimer();
        turn += 1;
        audio.removeEventListener('ended', onEnded);
        audio.removeEventListener('error', onError);
        for (const index of Array.from(clips.keys())) dropClip(index);
        dropUnlock();
        sounding = -1;
        pending = -1;
        audio.pause();
        audio.removeAttribute('src');
        audio.load();
        if (active.get(audio) === playback) active.delete(audio);
      }

      // Sound the pending piece once its clip is here, unless paused.
      async function playPending(): Promise<void> {
        const index = pending;
        if (settled || paused || index < 0) return;
        if (index >= pieces.length) {
          settle('ended');
          return;
        }
        const at = (turn += 1);
        const clip = fetchClip(index);
        let blob: Blob;
        try {
          blob = await clip.blob;
        } catch (err) {
          // A paused or skipped playback meets the failure when it comes
          // back to this clip.
          if (!settled && at === turn) fail(err instanceof Error ? err : new Error(String(err)));
          return;
        }
        if (settled || at !== turn) return;
        clip.url ??= urls.create(blob);
        audio.src = clip.url;
        dropUnlock();
        try {
          await audio.play();
        } catch {
          // A pause or skip interrupts the play it overtook.
          if (!settled && at === turn) fail(new Error(CLIP_PLAYBACK_FAILED));
          return;
        }
        if (settled || at !== turn) return;
        sounding = index;
        pending = -1;
        onPiece(index);
        if (index + 1 < pieces.length) fetchClip(index + 1);
      }

      const playback: SpeechPlayback = {
        done,
        stop: () => settle('stopped'),
        pause: () => {
          if (settled || paused) return;
          paused = true;
          turn += 1;
          clearTimer();
          audio.pause();
        },
        resume: () => {
          if (settled || !paused) return;
          paused = false;
          if (sounding < 0) {
            void playPending();
            return;
          }
          const at = (turn += 1);
          audio.play().catch(() => {
            if (!settled && at === turn) fail(new Error(CLIP_PLAYBACK_FAILED));
          });
        },
        seekBack: (seconds) => {
          if (settled || sounding < 0) return;
          audio.currentTime = Math.max(0, audio.currentTime - seconds);
        },
        skipTo: (index) => {
          if (settled) return;
          const target = Math.max(0, Math.floor(index));
          if (target >= pieces.length) {
            settle('ended');
            return;
          }
          clearTimer();
          turn += 1;
          paused = false;
          // Also a clip whose play() has not resolved: it would start
          // sounding while the destination is fetched.
          audio.pause();
          sounding = -1;
          for (const held of Array.from(clips.keys())) if (held !== target && held !== target + 1) dropClip(held);
          pending = target;
          fetchFrom(target);
          void playPending();
        },
      };
      active.set(audio, playback);

      audio.volume = speech.volume;
      audio.defaultPlaybackRate = speech.rate;
      audio.playbackRate = speech.rate;
      audio.preservesPitch = true;
      // Inside the gesture: start the element on silence so later plays
      // are allowed. A refusal here shows when the first clip plays.
      unlockUrl = urls.create(silentWav());
      audio.src = unlockUrl;
      audio.play().catch(() => undefined);

      if (from < pieces.length) fetchFrom(from);
      void playPending();
      return playback;
    },
  };
}
