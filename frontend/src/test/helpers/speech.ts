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

export function fakeSpeechEngine(voiceCount = 1): { engine: SpeechEngine; playbacks: FakePlayback[] } {
  const playbacks: FakePlayback[] = [];
  const engine: SpeechEngine = {
    voices: async () => Array.from({ length: voiceCount }, (_, i) => ({
      id: `v${i}`, name: `Voice ${i}`, lang: 'en-US', isDefault: i === 0,
    })),
    onVoicesChanged: () => () => undefined,
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
  return { engine, playbacks };
}
