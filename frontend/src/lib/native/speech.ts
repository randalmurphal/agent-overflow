// The device's own voice: what read-aloud speaks through.
//
// In a browser and the desktop webviews that is `speechSynthesis`, the
// operating system's voices. Android's WebView has none, so the phone shell
// speaks through Android's TextToSpeech in the `Speech` plugin, which also
// keeps a reading going while the app is in the background.
//
// Playback is a list of pieces (sentences) spoken in order from an index.
// Stopping is the only interruption the engines offer: pausing is stopping
// and remembering the piece, so both engines resume at the start of a
// sentence (mid-sentence resume is unreliable across OS voices).

import { isNativeShell, nativePlatform } from './platform';
import { speechPlugin, type SpeechPlugin } from './plugins';

export interface SpeechVoice {
  id: string;
  name: string;
  lang: string;
  isDefault: boolean;
}

export interface SpeechOptions {
  /** A voice id from `voices()`; '' is the system default. */
  voiceId: string;
  /** 1 is normal speed. */
  rate: number;
  /** 0 to 1. */
  volume: number;
}

export type SpeechOutcome = 'ended' | 'stopped';

export interface SpeechPlayback {
  /** 'ended' when the last piece finished, 'stopped' after `stop()`;
   * rejects when the engine fails. */
  readonly done: Promise<SpeechOutcome>;
  stop(): void;
}

export interface SpeechEngine {
  /** The voices this device offers; none means it cannot speak. */
  voices(): Promise<SpeechVoice[]>;
  /** Calls `changed` when the voices may have changed. */
  onVoicesChanged(changed: () => void): () => void;
  /** Speak `pieces` in order from `from`, reporting each piece as it
   * starts. Starting another playback stops this one. */
  speak(pieces: string[], from: number, options: SpeechOptions, onPiece: (index: number) => void): SpeechPlayback;
}

/** This device's speech engine, or null when it has none. */
export function speechEngine(): SpeechEngine | null {
  if (isNativeShell()) return nativePlatform() === 'android' ? androidEngine : null;
  if (typeof window === 'undefined' || !('speechSynthesis' in window)) return null;
  return webSpeechEngine(window.speechSynthesis);
}

// ---- speechSynthesis --------------------------------------------------------

/** The `speechSynthesis` engine. Exported for tests. */
export function webSpeechEngine(synth: SpeechSynthesis): SpeechEngine {
  let active: { stop(): void } | null = null;
  const browserVoices = (): SpeechSynthesisVoice[] => {
    try {
      return synth.getVoices();
    } catch {
      return [];
    }
  };
  return {
    voices: async () => browserVoices().map((voice) => ({
      id: voice.voiceURI,
      name: voice.name,
      lang: voice.lang,
      isDefault: voice.default,
    })),
    onVoicesChanged: (changed) => {
      synth.addEventListener('voiceschanged', changed);
      return () => synth.removeEventListener('voiceschanged', changed);
    },
    speak: (pieces, from, options, onPiece) => {
      active?.stop();
      let settle!: (outcome: SpeechOutcome) => void;
      let fail!: (err: Error) => void;
      let settled = false;
      const done = new Promise<SpeechOutcome>((resolve, reject) => {
        settle = (outcome) => {
          if (settled) return;
          settled = true;
          resolve(outcome);
        };
        fail = (err) => {
          if (settled) return;
          settled = true;
          reject(err);
        };
      });
      const voice = browserVoices().find((v) => v.voiceURI === options.voiceId) ?? null;
      // A queue left by a page that went away blocks every new utterance
      // in some engines until it is cancelled.
      synth.cancel();
      const speakFrom = (index: number): void => {
        if (settled) return;
        if (index >= pieces.length) {
          settle('ended');
          return;
        }
        const utterance = new SpeechSynthesisUtterance(pieces[index]);
        if (voice) utterance.voice = voice;
        utterance.rate = options.rate;
        utterance.volume = options.volume;
        utterance.onend = () => speakFrom(index + 1);
        utterance.onerror = (event) => {
          // Cancelling speech reports its own utterance as interrupted.
          if (settled || event.error === 'interrupted' || event.error === 'canceled') settle('stopped');
          else fail(new Error(`speech failed: ${event.error}`));
        };
        onPiece(index);
        synth.speak(utterance);
      };
      const playback: SpeechPlayback = {
        done,
        stop: () => {
          if (settled) return;
          settle('stopped');
          synth.cancel();
        },
      };
      active = playback;
      void done.finally(() => {
        if (active === playback) active = null;
      }).catch(() => undefined);
      speakFrom(from);
      return playback;
    },
  };
}

// ---- Android TextToSpeech ---------------------------------------------------

let nextSession = 0;

/** Plays through the `Speech` plugin. Each playback is a session; events
 * for any other session are a stopped playback's and are ignored. */
function nativeEngine(plugin: () => Promise<SpeechPlugin | null>): SpeechEngine {
  let active: { session: string; stop(): void } | null = null;
  return {
    voices: async () => {
      const speech = await plugin();
      if (!speech) return [];
      try {
        return (await speech.voices()).voices;
      } catch (err) {
        console.warn('native: the speech plugin listed no voices', err);
        return [];
      }
    },
    onVoicesChanged: () => () => undefined,
    speak: (pieces, from, options, onPiece) => {
      active?.stop();
      const session = `s${(nextSession += 1)}`;
      let settled = false;
      let settle!: (outcome: SpeechOutcome) => void;
      let fail!: (err: Error) => void;
      const done = new Promise<SpeechOutcome>((resolve, reject) => {
        settle = (outcome) => {
          if (!settled) {
            settled = true;
            resolve(outcome);
          }
        };
        fail = (err) => {
          if (!settled) {
            settled = true;
            reject(err);
          }
        };
      });
      const listeners: Array<{ remove: () => Promise<void> }> = [];
      const release = (): void => {
        for (const listener of listeners.splice(0)) void listener.remove();
      };
      void done.finally(release).catch(() => undefined);
      const playback = {
        session,
        done,
        stop: () => {
          if (settled) return;
          settle('stopped');
          void plugin().then((speech) => speech?.stop({ session }));
        },
      };
      active = playback;
      void (async () => {
        const speech = await plugin();
        if (!speech) {
          fail(new Error('this device has no speech engine'));
          return;
        }
        listeners.push(await speech.addListener('progress', (event) => {
          if (event.session === session && !settled) onPiece(event.index);
        }));
        listeners.push(await speech.addListener('done', (event) => {
          if (event.session !== session) return;
          if (event.outcome === 'error') fail(new Error(event.message || 'speech failed'));
          else settle(event.outcome);
        }));
        if (settled) {
          release();
          return;
        }
        await speech.speak({ session, pieces, from, ...options });
      })().catch((err: unknown) => fail(err instanceof Error ? err : new Error(String(err))));
      return playback;
    },
  };
}

const androidEngine = nativeEngine(speechPlugin);

/** The plugin engine over a given plugin. Exported for tests. */
export function nativeSpeechEngineForTest(plugin: SpeechPlugin): SpeechEngine {
  return nativeEngine(async () => plugin);
}
