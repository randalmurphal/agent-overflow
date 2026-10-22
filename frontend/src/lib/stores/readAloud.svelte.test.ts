import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import type { SpeechEngine, SpeechOptions, SpeechOutcome } from '../native/speech';
import {
  pauseReadAloud,
  readAloud,
  readAloudAvailable,
  readAloudPhase,
  resetReadAloudForTest,
  restartReadAloud,
  resumeReadAloud,
  toggleReadAloud,
  watchReadAloudVoices,
} from './readAloud.svelte';
import { reloadReadAloudPreferencesForTest, setReadAloudPreference } from './readAloudPreferences.svelte';
import * as toast from './toast.svelte';
import { fakeSpeechEngine } from '../../test/helpers/speech';

interface FakePlayback {
  pieces: string[];
  from: number;
  options: SpeechOptions;
  onPiece: (index: number) => void;
  stopped: boolean;
  finish: (outcome: SpeechOutcome) => void;
  fail: (err: Error) => void;
}

function fakeEngine(voiceCount = 1) {
  const playbacks: FakePlayback[] = [];
  const engine: SpeechEngine = {
    voices: async () => Array.from({ length: voiceCount }, (_, i) => ({ id: `v${i}`, name: `Voice ${i}`, lang: 'en-US', isDefault: i === 0 })),
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

const REPLY = 'First sentence. Second sentence. Third sentence.';

async function settle(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await Promise.resolve();
  flushSync();
}

describe('read-aloud', () => {
  let fake: ReturnType<typeof fakeEngine>;

  beforeEach(async () => {
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    setReadAloudPreference('enabled', true);
    resetReadAloudForTest();
    fake = fakeEngine();
    watchReadAloudVoices(fake.engine);
    await settle();
  });

  afterEach(() => {
    resetReadAloudForTest();
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    vi.restoreAllMocks();
  });

  it('reads a reply sentence by sentence with this device’s preferences', () => {
    setReadAloudPreference('voiceId', 'v0');
    setReadAloudPreference('rate', 1.5);
    setReadAloudPreference('volume', 0.4);
    readAloud('a', REPLY);
    expect(readAloudPhase('a')).toBe('playing');
    expect(readAloudPhase('b')).toBe('idle');
    expect(fake.playbacks[0]).toMatchObject({
      pieces: ['First sentence.', 'Second sentence.', 'Third sentence.'],
      from: 0,
      options: { voiceId: 'v0', rate: 1.5, volume: 0.4 },
    });
  });

  it('resumes at the start of the sentence it paused in, and restarts from the first', async () => {
    readAloud('a', REPLY);
    fake.playbacks[0].onPiece(1);
    pauseReadAloud();
    expect(fake.playbacks[0].stopped).toBe(true);
    await settle();
    expect(readAloudPhase('a')).toBe('paused');

    resumeReadAloud();
    expect(fake.playbacks[1].from).toBe(1);
    expect(readAloudPhase('a')).toBe('playing');

    pauseReadAloud();
    restartReadAloud();
    expect(fake.playbacks[2].from).toBe(0);
  });

  it('reads one reply at a time and forgets the place of the one it stopped', async () => {
    readAloud('a', REPLY);
    fake.playbacks[0].onPiece(2);
    readAloud('b', 'Other reply.');
    expect(fake.playbacks[0].stopped).toBe(true);
    await settle();
    expect(readAloudPhase('a')).toBe('idle');
    expect(readAloudPhase('b')).toBe('playing');

    readAloud('a', REPLY);
    expect(fake.playbacks[2].from).toBe(0);
  });

  it('returns a finished reply to the start', async () => {
    readAloud('a', REPLY);
    fake.playbacks[0].finish('ended');
    await settle();
    expect(readAloudPhase('a')).toBe('idle');
    expect(toggleReadAloud()).toBe(false);
  });

  it('a stopped reading that ends late changes nothing of the next', async () => {
    readAloud('a', REPLY);
    const first = fake.playbacks[0];
    readAloud('b', REPLY);
    first.onPiece(2);
    first.finish('ended');
    await settle();
    expect(readAloudPhase('b')).toBe('playing');
    pauseReadAloud();
    expect(fake.playbacks[1].stopped).toBe(true);
    resumeReadAloud();
    expect(fake.playbacks[2].from).toBe(0);
  });

  it('refuses a speed or volume out of range', () => {
    setReadAloudPreference('rate', 5);
    setReadAloudPreference('volume', -1);
    readAloud('a', REPLY);
    expect(fake.playbacks[0].options).toMatchObject({ rate: 1, volume: 1 });
  });

  it('says so and stops when the voice fails', async () => {
    const shown = vi.spyOn(toast, 'addErrorToast').mockImplementation(() => undefined as never);
    readAloud('a', REPLY);
    fake.playbacks[0].fail(new Error('speech failed: synthesis-failed'));
    await settle();
    expect(readAloudPhase('a')).toBe('idle');
    expect(shown).toHaveBeenCalledOnce();
  });

  it('pauses and resumes from the chord, and does nothing with nothing read', async () => {
    expect(toggleReadAloud()).toBe(false);
    readAloud('a', REPLY);
    expect(toggleReadAloud()).toBe(true);
    await settle();
    expect(readAloudPhase('a')).toBe('paused');
    expect(toggleReadAloud()).toBe(true);
    expect(readAloudPhase('a')).toBe('playing');
  });

  it('reads nothing while off, and turning it off stops a reading', async () => {
    readAloud('a', REPLY);
    setReadAloudPreference('enabled', false);
    flushSync();
    expect(fake.playbacks[0].stopped).toBe(true);
    expect(readAloudPhase('a')).toBe('idle');
    expect(readAloudAvailable()).toBe(false);
    readAloud('a', REPLY);
    expect(fake.playbacks).toHaveLength(1);
  });

  it('is unavailable on a device with no voice', async () => {
    resetReadAloudForTest();
    const silent = fakeEngine(0);
    watchReadAloudVoices(silent.engine);
    await settle();
    expect(readAloudAvailable()).toBe(false);
    readAloud('a', REPLY);
    expect(silent.playbacks).toHaveLength(0);
  });

  it('returns to idle when speech is stopped from outside', async () => {
    readAloud('a', REPLY);
    fake.playbacks[0].finish('stopped');
    await settle();
    expect(readAloudPhase('a')).toBe('idle');
  });

  it('forgets a reading when the device loses its voices, and does not bring it back', async () => {
    resetReadAloudForTest();
    const device = fakeSpeechEngine();
    watchReadAloudVoices(device.engine);
    await settle();
    for (const pausedFirst of [false, true]) {
      readAloud('a', REPLY);
      if (pausedFirst) {
        pauseReadAloud();
        await settle();
      }
      const playback = device.playbacks.at(-1)!;
      device.setVoiceCount(0);
      await settle();
      expect(readAloudPhase('a')).toBe('idle');
      if (!pausedFirst) expect(playback.stopped).toBe(true);
      device.setVoiceCount(1);
      await settle();
      expect(readAloudPhase('a')).toBe('idle');
      expect(toggleReadAloud()).toBe(false);
    }
  });

  it('starts nothing for a reply with nothing to read', () => {
    readAloud('a', '```\ncode only\n```');
    expect(fake.playbacks).toHaveLength(0);
    expect(readAloudPhase('a')).toBe('idle');
  });
});
