import { expect, it } from 'vitest';
import { frame, makeHarness, now, rafQueue } from './springTestHarness';
import { __resetSpringFrameBatcherForTest } from './spring';

it.each([[144, 165], [165, 144]])(
  'keeps cruise steps even at %iHz when animation timestamps follow a %iHz clock',
  (displayHz, timestampHz) => {
    const h = makeHarness({ quantize: true });
    h.setTarget(100_000);
    h.spring.start();
    const interval = 1000 / displayHz;
    const timestampInterval = 1000 / timestampHz;
    const steps: number[] = [];
    for (let tick = 0; tick < displayHz * 3; tick++) {
      const before = h.getScrollTop();
      frame(interval, Math.floor((now + interval) / timestampInterval) * timestampInterval);
      if (tick >= displayHz * 2) steps.push(h.getScrollTop() - before);
    }
    // The steady 1620 px/s trajectory needs only the two adjacent grid steps.
    // A clock from the other output introduces short, long or repeated steps.
    const distancePerRefresh = 1620 / displayHz;
    expect(h.velocity()).toBeCloseTo(27);
    expect(Math.min(...steps)).toBe(Math.floor(distancePerRefresh));
    expect(Math.max(...steps)).toBe(Math.ceil(distancePerRefresh));
    expect(steps.reduce((sum, step) => sum + step, 0)).toBeCloseTo(1620, 0);
    h.spring.cancel();
  },
);

it.each([60, 120, 165])(
  'steps identically at %iHz whether or not callback time jitters around vsync-aligned timestamps',
  (displayHz) => {
    const interval = 1000 / displayHz;
    const run = (jitter: boolean): number[] => {
      const h = makeHarness({ quantize: true, dpr: 2.625 });
      h.setTarget(1000);
      h.spring.markTargetChanged();
      h.spring.start();
      let seed = 7;
      let timestamp = now;
      let target = 1000;
      const steps: number[] = [];
      for (let tick = 0; tick < displayHz * 3; tick++) {
        if (tick % Math.round(displayHz / 8) === 0) {
          target += 20;
          h.setTarget(target);
          h.spring.markTargetChanged();
        }
        seed = (seed * 1103515245 + 12345) % 2147483648;
        // Callback time lands up to 30% of a frame late or early; the
        // timestamp stays on the display clock.
        const skew = jitter ? (seed / 2147483648 - 0.5) * 0.6 * interval : 0;
        timestamp += interval;
        const before = h.getScrollTop();
        frame(timestamp + skew - now, timestamp);
        steps.push(Math.round((h.getScrollTop() - before) * 2.625));
      }
      h.spring.cancel();
      // The stubbed cancelAnimationFrame leaves the batcher's frame queued.
      rafQueue.length = 0;
      __resetSpringFrameBatcherForTest();
      return steps;
    };
    const steady = run(false);
    const jittered = run(true);
    expect(jittered).toEqual(steady);
  },
);
