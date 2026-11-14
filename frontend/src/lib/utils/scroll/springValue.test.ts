import { describe, expect, it } from 'vitest';
import { SPRING_MAX_VELOCITY_PX_PER_FRAME, SpringValue } from './motion';

function run(from: number, to: number, framesPerStep = 1): number[] {
  const spring = new SpringValue();
  const values = [from];
  for (let i = 0; i < 2_000 && values.at(-1) !== to; i += 1) {
    values.push(spring.step(values.at(-1)!, to, framesPerStep));
  }
  return values;
}

describe('SpringValue', () => {
  it.each([
    [0, 120],
    [86, 0],
    [0, 12],
    [300, 0],
  ])('travels %d -> %d monotonically and lands exactly', (from, to) => {
    const values = run(from, to);
    expect(values.at(-1)).toBe(to);
    const direction = Math.sign(to - from);
    for (let i = 1; i < values.length; i += 1) {
      const step = (values[i] - values[i - 1]) * direction;
      expect(step).toBeGreaterThan(0);
      expect(step).toBeLessThanOrEqual(SPRING_MAX_VELOCITY_PX_PER_FRAME + 1e-9);
    }
  });

  it('eases in and out instead of covering the distance in a few frames', () => {
    const values = run(0, 120);
    // Unhurried: the onset starts near a pixel a frame and the landing brakes.
    expect(values.length).toBeGreaterThan(20);
    expect(values[1] - values[0]).toBeLessThanOrEqual(1.6);
    const steps = values.slice(1).map((value, index) => value - values[index]);
    expect(Math.max(...steps)).toBeGreaterThan(steps[0] * 2);
    expect(steps.at(-1)!).toBeLessThan(Math.max(...steps));
  });

  it('does not advance further on a long frame than on one frame', () => {
    const one = new SpringValue().step(0, 120, 1);
    const long = new SpringValue().step(0, 120, 3);
    expect(long).toBe(one);
  });

  it('stays put at the target and on a zero-length frame', () => {
    const spring = new SpringValue();
    expect(spring.step(40, 40, 1)).toBe(40);
    expect(spring.step(10, 40, 0)).toBe(10);
  });

  it('turns around when the target moves behind it', () => {
    const spring = new SpringValue();
    let value = 0;
    for (let i = 0; i < 12; i += 1) value = spring.step(value, 200, 1);
    const peak = value;
    for (let i = 0; i < 2_000 && value !== 0; i += 1) value = spring.step(value, 0, 1);
    expect(peak).toBeGreaterThan(0);
    expect(value).toBe(0);
  });

  it('rejects nonfinite input', () => {
    const spring = new SpringValue();
    expect(() => spring.step(Number.NaN, 1, 1)).toThrow(RangeError);
    expect(() => spring.step(0, 1, -1)).toThrow(RangeError);
  });
});
