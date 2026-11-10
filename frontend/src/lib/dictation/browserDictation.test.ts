import { describe, expect, it } from 'vitest';
import { loudestStretch, recordingMeasurer } from './browserDictation';
import { SILENCE_PEAK } from './dictationSession.svelte';

const RATE = 48_000;
/** The samples loudness is measured over, as the level meter reads them. */
const BLOCK = 1024;

/** `length` samples of silence with `amplitude` over each burst's samples. */
function audio(length: number, bursts: Array<{ from: number; to: number; amplitude: number }>, channels = 1) {
  const samples = new Float32Array(length);
  for (const { from, to, amplitude } of bursts) samples.fill(amplitude, from, to);
  const silent = new Float32Array(length);
  return {
    numberOfChannels: channels,
    sampleRate: RATE,
    // The sound is in the last channel, so every channel must be measured.
    getChannelData: (channel: number) => (channel === channels - 1 ? samples : silent),
  };
}

describe('loudestStretch', () => {
  it('measures silence as silence', () => {
    expect(loudestStretch(audio(2 * RATE, []))).toBe(0);
  });

  it('hears a sound just over the threshold wherever it falls', () => {
    const length = 60 * RATE;
    for (const from of [0, 1, 256, 511, 512, 513, 1023, 1024, 77_777, length - BLOCK]) {
      const loudness = loudestStretch(audio(length, [{ from, to: from + BLOCK, amplitude: 0.021 }]));
      expect(loudness, `a sound from sample ${from}`).toBeCloseTo(0.021, 6);
    }
  });

  it('measures a steady level as that level', () => {
    expect(loudestStretch(audio(RATE, [{ from: 0, to: RATE, amplitude: 0.25 }]))).toBeCloseTo(0.25, 5);
  });

  it('does not take a few samples for a loud moment', () => {
    for (const from of [0, 5000, RATE - 4]) {
      const loudness = loudestStretch(audio(RATE, [{ from, to: from + 4, amplitude: 0.1 }]));
      expect(loudness, `a click at sample ${from}`).toBeLessThan(SILENCE_PEAK);
    }
    // Shorter than one stretch altogether.
    expect(loudestStretch(audio(4, [{ from: 0, to: 4, amplitude: 0.1 }]))).toBeLessThan(SILENCE_PEAK);
  });

  it('measures every channel', () => {
    expect(loudestStretch(audio(RATE, [{ from: 20_000, to: 30_000, amplitude: 0.2 }], 2))).toBeCloseTo(0.2, 5);
  });
});

describe('recordingMeasurer', () => {
  const recording = new Blob(['x']);
  const loud = audio(RATE, [{ from: 10_000, to: 20_000, amplitude: 0.3 }]);

  it('answers the loudness of what the engine decoded', async () => {
    expect(await recordingMeasurer(async () => loud, 1000)(recording)).toBeCloseTo(0.3, 5);
  });

  it('rejects when the engine cannot decode the recording', async () => {
    const measure = recordingMeasurer(async () => { throw new Error('not audio'); }, 1000);
    await expect(measure(recording)).rejects.toThrow('not audio');
  });

  it('rejects, and measures the next recording, when decoding throws at once', async () => {
    let fails = true;
    const measure = recordingMeasurer(() => {
      if (fails) throw new Error('no decoder');
      return Promise.resolve(loud);
    }, 1000);
    await expect(measure(recording)).rejects.toThrow('no decoder');
    fails = false;
    expect(await measure(recording)).toBeCloseTo(0.3, 5);
  });

  it('gives up on a decode that does not finish, and decodes nothing beside it', async () => {
    const decodes: Array<(decoded: typeof loud) => void> = [];
    const measure = recordingMeasurer(() => new Promise((resolve) => { decodes.push(resolve); }), 20);
    await expect(measure(recording)).rejects.toThrow('too long');
    // The engine is still decoding the first.
    await expect(measure(recording)).rejects.toThrow('still being measured');
    expect(decodes).toHaveLength(1);
    // Once it settles, the next recording is measured.
    decodes[0](loud);
    await new Promise((resolve) => setTimeout(resolve, 0));
    const next = measure(recording);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(decodes).toHaveLength(2);
    decodes[1](loud);
    expect(await next).toBeCloseTo(0.3, 5);
  });

  it('measures one recording after another', async () => {
    const measure = recordingMeasurer(async () => loud, 1000);
    expect(await measure(recording)).toBeCloseTo(0.3, 5);
    expect(await measure(recording)).toBeCloseTo(0.3, 5);
  });
});
