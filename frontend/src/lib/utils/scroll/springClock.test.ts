import { expect, it } from 'vitest';
import { frame, makeHarness, now } from './springTestHarness';

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
