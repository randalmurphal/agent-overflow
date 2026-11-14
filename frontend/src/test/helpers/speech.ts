// Scripted speech engines for tests of read-aloud: a device engine and
// OpenAI clip engines that record each playback and let the test report,
// finish or stop it.

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

export interface FakeClipPlayback extends Omit<FakePlayback, 'fail'> {
  backend: string;
  fail: (err: unknown) => void;
  /** Every control call, in order: 'pause', 'resume', 'seek 10', 'skip 2'. */
  calls: string[];
}

/** OpenAI engines per computer that, like the real one, report a piece
 * only when the test says it sounds, and hold, seek and skip within a
 * playback. `holds: false` gives playbacks that can only stop. */
export function fakeClipEngines({ holds = true } = {}): {
  playbacks: FakeClipPlayback[];
  factory: (backend: string) => SpeechEngine;
} {
  const playbacks: FakeClipPlayback[] = [];
  const factory = (backend: string): SpeechEngine => ({
    voices: async () => [],
    onVoicesChanged: () => () => undefined,
    speak: (pieces, from, options, onPiece) => {
      let finish!: (outcome: SpeechOutcome) => void;
      let fail!: (err: unknown) => void;
      const done = new Promise<SpeechOutcome>((resolve, reject) => {
        finish = resolve;
        fail = reject;
      });
      const record: FakeClipPlayback = { backend, pieces, from, options, onPiece, stopped: false, finish, fail, calls: [] };
      playbacks.push(record);
      const stop = (): void => {
        record.stopped = true;
        finish('stopped');
      };
      if (!holds) return { done, stop };
      return {
        done,
        stop,
        pause: () => record.calls.push('pause'),
        resume: () => record.calls.push('resume'),
        seekBack: (seconds) => record.calls.push(`seek ${seconds}`),
        skipTo: (index) => record.calls.push(`skip ${index}`),
      };
    },
  });
  return { playbacks, factory };
}
