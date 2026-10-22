// A scripted speech engine for tests of the controls that read aloud: it
// records each playback and lets the test finish or stop it.

import type { SpeechEngine, SpeechOptions, SpeechOutcome } from '../../lib/native/speech';

export interface FakePlayback {
  pieces: string[];
  from: number;
  options: SpeechOptions;
  onPiece: (index: number) => void;
  stopped: boolean;
  finish: (outcome: SpeechOutcome) => void;
  fail: (err: Error) => void;
}

export function fakeSpeechEngine(voiceCount = 1): {
  engine: SpeechEngine;
  playbacks: FakePlayback[];
  /** Change how many voices the device has and announce it, as
   * `voiceschanged` does. */
  setVoiceCount: (count: number) => void;
} {
  const playbacks: FakePlayback[] = [];
  const changed = new Set<() => void>();
  let count = voiceCount;
  const engine: SpeechEngine = {
    voices: async () => Array.from({ length: count }, (_, i) => ({
      id: `v${i}`, name: `Voice ${i}`, lang: 'en-US', isDefault: i === 0,
    })),
    onVoicesChanged: (listener) => {
      changed.add(listener);
      return () => changed.delete(listener);
    },
    speak: (pieces, from, options, onPiece) => {
      let finish!: (outcome: SpeechOutcome) => void;
      let fail!: (err: Error) => void;
      const done = new Promise<SpeechOutcome>((resolve, reject) => {
        finish = resolve;
        fail = reject;
      });
      const record: FakePlayback = { pieces, from, options, onPiece, stopped: false, finish, fail };
      playbacks.push(record);
      onPiece(from);
      return {
        done,
        stop: () => {
          record.stopped = true;
          finish('stopped');
        },
      };
    },
  };
  return {
    engine,
    playbacks,
    setVoiceCount: (next) => {
      count = next;
      for (const listener of changed) listener();
    },
  };
}
