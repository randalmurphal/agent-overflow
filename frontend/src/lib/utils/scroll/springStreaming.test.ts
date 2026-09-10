import { describe, expect, it } from 'vitest';
import { frame, makeHarness } from './springTestHarness';

const rates = [60, 90, 120, 144, 165, 240, 360, 480];

interface Sample {
  time: number;
  position: number;
  distance: number;
  velocity: number;
  acceleration: number;
  jerk: number;
}

function stream(hz: number, intervals: number[], lines: number[], quantum = 1e-8): Sample[] {
  const h = makeHarness({ quantum });
  h.setTarget(lines[0]);
  h.spring.start();
  const samples: Sample[] = [];
  let next = intervals[0];
  let delivery = 0;
  let velocity = 0;
  let acceleration = 0;
  for (let tick = 1; tick <= hz * 10; tick++) {
    const time = tick * 1000 / hz;
    while (time >= next - 1e-6) {
      delivery++;
      h.setTarget(h.getTarget() + lines[delivery % lines.length]);
      h.spring.markTargetChanged();
      next += intervals[delivery % intervals.length];
    }
    frame(1000 / hz);
    const fraction = tick === 1 ? 1 : 60 / hz;
    const nextVelocity = h.velocity();
    const nextAcceleration = (nextVelocity - velocity) / fraction;
    samples.push({
      time,
      position: h.getScrollTop(),
      distance: h.getTarget() - h.getScrollTop(),
      velocity: nextVelocity,
      acceleration: nextAcceleration,
      jerk: (nextAcceleration - acceleration) / fraction,
    });
    velocity = nextVelocity;
    acceleration = nextAcceleration;
  }
  h.setLiveContentActive(false);
  let settlingTicks = 0;
  for (; settlingTicks < hz * 2 && h.getScrollTop() !== h.getTarget(); settlingTicks++) frame(1000 / hz);
  expect(h.getScrollTop()).toBe(h.getTarget());
  expect(settlingTicks * 1000 / hz).toBeLessThan(650);
  expect(h.refusalEvents).toHaveLength(0);
  h.spring.cancel();
  return samples;
}

describe('streamed glide continuity and tracking', () => {
  it.each(rates)('rounds both acceleration directions through whole line cycles at %iHz', (hz) => {
    const samples = stream(hz, [1000 / 7.5], [20]);
    const warm = samples.filter((sample) => sample.time >= 2000);
    expect(warm.some((sample) => sample.acceleration > 0.02)).toBe(true);
    expect(warm.some((sample) => sample.acceleration < -0.02)).toBe(true);
    for (const sample of warm) {
      expect(Math.abs(sample.jerk), `jerk at ${sample.time}ms`).toBeLessThanOrEqual(0.105);
      expect(sample.velocity).toBeGreaterThan(0);
      expect(sample.distance).toBeGreaterThan(0);
    }
    const velocities = warm.map((sample) => sample.velocity * 60);
    expect(Math.max(...velocities) - Math.min(...velocities)).toBeLessThan(25);
    expect(Math.min(...velocities)).toBeGreaterThan(125);
    expect(Math.max(...warm.map((sample) => sample.distance))).toBeLessThan(60);
    const meanDistance = (rows: Sample[]) => rows.reduce((sum, row) => sum + row.distance, 0) / rows.length;
    const early = warm.filter((sample) => sample.time < 4000);
    const late = warm.filter((sample) => sample.time >= 8000);
    expect(Math.abs(meanDistance(late) - meanDistance(early))).toBeLessThan(2);
  });

  it.each(rates)('keeps irregular line arrivals smooth and responsive at %iHz', (hz) => {
    for (const intervals of [[100], [200], [250], [275], [90, 160, 110, 240, 130, 270, 100, 190]]) {
      const warm = stream(hz, intervals, [16, 20, 24]).filter((sample) => sample.time >= 2000);
      expect(Math.max(...warm.map((sample) => Math.abs(sample.jerk)))).toBeLessThan(0.16);
      expect(Math.max(...warm.map((sample) => sample.distance))).toBeLessThan(80);
      expect(Math.min(...warm.map((sample) => sample.velocity))).toBeGreaterThan(0);
    }
  });

  it.each([60, 144, 165, 240, 480])('preserves the same streaming curve on native grids at %iHz', (hz) => {
    const reference = stream(hz, [1000 / 7.5], [20]);
    for (const quantum of [1, 0.8, 1 / 2.625]) {
      const actual = stream(hz, [1000 / 7.5], [20], quantum);
      for (let i = 0; i < reference.length; i++) {
        expect(Math.abs(actual[i].position - reference[i].position)).toBeLessThanOrEqual(quantum / 2 + 1e-5);
        expect(actual[i].velocity).toBeCloseTo(reference[i].velocity, 5);
      }
    }
  });
});

it.each(rates)('rounds cruise entry and burst retargets without losing catch-up speed at %iHz', (hz) => {
  const h = makeHarness();
  h.setTarget(1200);
  h.spring.start();
  let velocity = 0;
  let acceleration = 0;
  let extended = false;
  let extendedAt = 0;
  let brakingMs = 0;
  let minHandoffSpeed = Infinity;
  let maxSpeed = 0;
  for (let tick = 1; tick <= hz * 5 && h.getScrollTop() !== h.getTarget(); tick++) {
    if (!extended && brakingMs >= 50) {
      h.setTarget(2400);
      h.spring.markTargetChanged();
      extended = true;
      extendedAt = tick;
      expect(velocity).toBeGreaterThan(15);
    }
    frame(1000 / hz);
    const fraction = tick === 1 ? 1 : 60 / hz;
    const nextVelocity = h.velocity();
    const nextAcceleration = (nextVelocity - velocity) / fraction;
    const jerk = (nextAcceleration - acceleration) / fraction;
    if (tick > 3 && h.getTarget() - h.getScrollTop() > 2) {
      expect(Math.abs(jerk), `jerk at tick ${tick}`).toBeLessThan(0.51);
    }
    if (extended && tick - extendedAt < hz / 3) minHandoffSpeed = Math.min(minHandoffSpeed, nextVelocity);
    brakingMs = nextAcceleration < -0.05 ? brakingMs + 1000 / hz : 0;
    maxSpeed = Math.max(maxSpeed, nextVelocity);
    velocity = nextVelocity;
    acceleration = nextAcceleration;
  }
  expect(extended).toBe(true);
  expect(minHandoffSpeed).toBeGreaterThan(15);
  expect(maxSpeed).toBeCloseTo(27);
  expect(h.getScrollTop()).toBe(2400);
  h.spring.cancel();
});

it('keeps elapsed-time motion continuous across cadence changes during a stream', () => {
  const h = makeHarness();
  h.setTarget(20);
  h.spring.start();
  let time = 0;
  let next = 1000 / 7.5;
  let velocity = 0;
  let acceleration = 0;
  for (const hz of [60, 240, 144, 165, 120, 480, 60]) {
    for (let tick = 0; tick < hz; tick++) {
      const ms = 1000 / hz * [0.9, 1.1, 1.02, 0.98][tick % 4];
      time += ms;
      while (time >= next) {
        h.setTarget(h.getTarget() + 20);
        h.spring.markTargetChanged();
        next += 1000 / 7.5;
      }
      frame(ms);
      const fraction = Math.min(1, ms * 60 / 1000);
      const nextVelocity = h.velocity();
      const nextAcceleration = (nextVelocity - velocity) / fraction;
      if (time > 1500) {
        expect(Math.abs(nextAcceleration - acceleration) / fraction).toBeLessThan(0.105);
        expect(nextVelocity * 60).toBeGreaterThan(125);
        expect(h.getTarget() - h.getScrollTop()).toBeLessThan(60);
      }
      velocity = nextVelocity;
      acceleration = nextAcceleration;
    }
  }
  h.spring.cancel();
});
