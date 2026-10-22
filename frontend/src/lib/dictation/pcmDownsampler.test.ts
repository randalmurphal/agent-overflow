import { describe, expect, it } from 'vitest';
import { LIVE_SAMPLE_RATE, PIECE_SAMPLES, PcmDownsampler } from './pcmDownsampler';

function capture(rate: number) {
  const pieces: Int16Array[] = [];
  const downsampler = new PcmDownsampler(rate, (piece) => pieces.push(piece));
  return { downsampler, pieces, samples: () => pieces.flatMap((piece) => Array.from(piece)) };
}

describe('PcmDownsampler', () => {
  it('halves 48 kHz into 100 ms pieces of 24 kHz samples', () => {
    const { downsampler, pieces } = capture(48_000);
    // 250 ms of input in render-quantum blocks.
    for (let i = 0; i < 48_000 / 4 / 128 + 1; i++) downsampler.push(new Float32Array(128).fill(0.5));
    expect(pieces).toHaveLength(2);
    expect(pieces.every((piece) => piece.length === PIECE_SAMPLES)).toBe(true);
    expect(pieces[0][0]).toBe(Math.round(0.5 * 0x7fff));
  });

  it('averages the inputs each output spans', () => {
    const { downsampler, samples } = capture(48_000);
    const input = new Float32Array(PIECE_SAMPLES * 2);
    for (let i = 0; i < input.length; i++) input[i] = i % 2 === 0 ? 1 : -1;
    downsampler.push(input);
    // A tone at the input's Nyquist rate, which 24 kHz cannot carry, averages
    // to silence rather than aliasing.
    expect(samples().every((sample) => sample === 0)).toBe(true);
  });

  it('keeps the rate exact from 44.1 kHz', () => {
    const { downsampler, pieces } = capture(44_100);
    for (let i = 0; i < 44_100 / 128; i++) downsampler.push(new Float32Array(128));
    downsampler.flush();
    const total = pieces.reduce((sum, piece) => sum + piece.length, 0);
    expect(Math.abs(total - LIVE_SAMPLE_RATE)).toBeLessThanOrEqual(Math.ceil(LIVE_SAMPLE_RATE / 44_100 * 128));
  });

  it('repeats samples from a lower input rate', () => {
    const { downsampler, samples } = capture(12_000);
    downsampler.push(new Float32Array([0.25, -0.25]));
    downsampler.flush();
    const quarter = Math.round(0.25 * 0x7fff);
    expect(samples()).toEqual([quarter, quarter, -Math.round(0.25 * 0x8000), -Math.round(0.25 * 0x8000)]);
  });

  it('clamps and spans the full 16-bit range', () => {
    const { downsampler, samples } = capture(24_000);
    downsampler.push(new Float32Array([2, -2, 1, -1]));
    downsampler.flush();
    expect(samples()).toEqual([0x7fff, -0x8000, 0x7fff, -0x8000]);
  });

  it('flushes the part-filled piece once, and nothing when empty', () => {
    const { downsampler, pieces } = capture(24_000);
    downsampler.flush();
    expect(pieces).toHaveLength(0);
    downsampler.push(new Float32Array(10));
    downsampler.flush();
    downsampler.flush();
    expect(pieces.map((piece) => piece.length)).toEqual([10]);
  });

  it('refuses a rate it cannot step', () => {
    expect(() => new PcmDownsampler(0, () => {})).toThrow();
  });
});
