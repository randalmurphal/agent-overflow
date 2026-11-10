// Read-aloud in voices a computer makes: each sentence is fetched as a
// complete audio clip and played through one reused audio element.
//
// The element is started inside the click or key that began the reading,
// on a silent clip made here, before anything is awaited. WebKit and
// Android's WebView then let later plays on that element go ahead without
// a gesture, so each sentence can start when its clip arrives.
//
// While a sentence plays, the next one is fetched; at most two clips are
// held. Stopping aborts the fetches, pauses the element and releases every
// object URL and listener.

import type { SpeechEngine, SpeechOutcome, SpeechPlayback, SpeechVoice } from './speech';

/** Fetches one sentence's audio in `voice`. */
export type FetchClip = (text: string, voice: string, signal: AbortSignal) => Promise<Blob>;

/** The part of `HTMLAudioElement` a playback uses. */
export interface ClipAudio {
  src: string;
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
}

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
      // The sentence sounding now; -1 while none is.
      let sounding = -1;
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

      const onEnded = (): void => {
        if (settled || sounding < 0) return;
        const finished = sounding;
        sounding = -1;
        dropClip(finished);
        void playFrom(finished + 1);
      };
      const onError = (): void => {
        if (settled || sounding < 0) return;
        fail(new Error(CLIP_PLAYBACK_FAILED));
      };
      audio.addEventListener('ended', onEnded);
      audio.addEventListener('error', onError);

      function release(): void {
        audio.removeEventListener('ended', onEnded);
        audio.removeEventListener('error', onError);
        for (const index of [...clips.keys()]) dropClip(index);
        dropUnlock();
        sounding = -1;
        audio.pause();
        audio.removeAttribute('src');
        audio.load();
        if (active.get(audio) === playback) active.delete(audio);
      }

      async function playFrom(index: number): Promise<void> {
        if (settled) return;
        if (index >= pieces.length) {
          settle('ended');
          return;
        }
        const clip = fetchClip(index);
        let blob: Blob;
        try {
          blob = await clip.blob;
        } catch (err) {
          if (!settled) fail(err instanceof Error ? err : new Error(String(err)));
          return;
        }
        if (settled) return;
        clip.url = urls.create(blob);
        audio.src = clip.url;
        dropUnlock();
        try {
          await audio.play();
        } catch {
          if (!settled) fail(new Error(CLIP_PLAYBACK_FAILED));
          return;
        }
        if (settled) return;
        sounding = index;
        onPiece(index);
        if (index + 1 < pieces.length) fetchClip(index + 1);
      }

      const playback: SpeechPlayback = {
        done,
        stop: () => settle('stopped'),
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

      void playFrom(from);
      return playback;
    },
  };
}
