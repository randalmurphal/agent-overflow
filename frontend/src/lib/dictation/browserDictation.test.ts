import { describe, expect, it } from 'vitest';
import { loudestStretch, recordingLoudness } from './browserDictation';
import { SILENCE_PEAK } from './dictationSession.svelte';

const RATE = 8000;

/** `seconds` of silence with `amplitude` from `from` to `to` seconds. */
function audio(seconds: number, bursts: Array<{ from: number; to: number; amplitude: number }>, channels = 1) {
  const samples = new Float32Array(Math.round(seconds * RATE));
  for (const { from, to, amplitude } of bursts) samples.fill(amplitude, Math.round(from * RATE), Math.round(to * RATE));
  const silent = new Float32Array(samples.length);
  return {
    numberOfChannels: channels,
    sampleRate: RATE,
    // The sound is in the last channel, so every channel must be measured.
    getChannelData: (channel: number) => (channel === channels - 1 ? samples : silent),
  };
}

describe('loudestStretch', () => {
  it('measures silence as silence', () => {
    expect(loudestStretch(audio(2, []))).toBe(0);
  });

  it('hears a sound a fiftieth of a second long anywhere in a long recording', () => {
    for (const at of [0, 0.0107, 0.5, 1.2345, 599.97]) {
      const loudness = loudestStretch(audio(600, [{ from: at, to: at + 0.02, amplitude: 0.1 }]));
      expect(loudness, `a burst at ${at}s`).toBeGreaterThanOrEqual(0.08);
    }
  });

  it('measures a steady level as that level', () => {
    expect(loudestStretch(audio(1, [{ from: 0, to: 1, amplitude: 0.25 }]))).toBeCloseTo(0.25, 5);
  });

  it('does not take a few samples at the very end for a loud moment', () => {
    const tail = 4 / RATE;
    expect(loudestStretch(audio(1, [{ from: 1 - tail, to: 1, amplitude: 0.1 }]))).toBeLessThan(SILENCE_PEAK);
  });

  it('measures every channel', () => {
    expect(loudestStretch(audio(1, [{ from: 0.4, to: 0.6, amplitude: 0.2 }], 2))).toBeCloseTo(0.2, 5);
  });
});

describe('recordingLoudness', () => {
  it('answers the loudness of what the engine decoded', async () => {
    const decoded = audio(1, [{ from: 0.2, to: 0.5, amplitude: 0.3 }]);
    expect(await recordingLoudness(new Blob(['x']), 1000, async () => decoded)).toBeCloseTo(0.3, 5);
  });

  it('rejects when the engine cannot decode the recording', async () => {
    await expect(recordingLoudness(new Blob(['x']), 1000, async () => { throw new Error('not audio'); })).rejects.toThrow('not audio');
  });

  it('gives up on a decode that never finishes', async () => {
    await expect(recordingLoudness(new Blob(['x']), 20, () => new Promise(() => {}))).rejects.toThrow('too long');
  });
});
