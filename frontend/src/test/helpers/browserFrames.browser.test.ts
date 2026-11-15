import { describe, expect, it } from 'vitest';
import { raf, waitFor, withinFrames } from './browserFrames';

describe('browser frame helpers', () => {
  it('waitFor waits out a settle that takes more frames than a frame budget would allow', async () => {
    let frames = 0;
    let counting = true;
    const count = () => {
      if (!counting) return;
      frames++;
      requestAnimationFrame(count);
    };
    requestAnimationFrame(count);
    try {
      await waitFor(() => frames >= 200, 'two hundred frames');
    } finally {
      counting = false;
    }
    expect(frames).toBeGreaterThanOrEqual(200);
  });

  it('waitFor fails with its label once the time bound passes', async () => {
    await expect(waitFor(() => false, 'never', 50)).rejects.toThrow('timed out after 50 ms waiting for: never');
  });

  it('withinFrames holds a claim to its frame count', async () => {
    let frames = 0;
    const tick = async () => {
      await raf();
      frames++;
    };
    void tick().then(tick).then(tick);
    await withinFrames(() => frames >= 3, 'three frames', 10);
    await expect(withinFrames(() => false, 'never', 3)).rejects.toThrow('not within 3 frames: never');
  });
});
