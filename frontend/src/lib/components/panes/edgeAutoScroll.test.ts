import { describe, expect, it } from 'vitest';
import {
  AUTO_SCROLL_MAX_STEP_PX,
  edgeAutoScrollVelocity,
  type HorizontalEdges,
} from './edgeAutoScroll';

// A 1000px-wide host: edge zone = min(96, 1000/4) = 96px on each side.
const HOST: HorizontalEdges = { left: 0, right: 1000, width: 1000 };
const MAX = AUTO_SCROLL_MAX_STEP_PX;

describe('edgeAutoScrollVelocity', () => {
  // Each case is a host and the [x, velocity] pairs it is read at.
  it.each<{ name: string; host: HorizontalEdges; at: [number, number][] }>([
    { name: 'is zero for a zero-width host (no edge zone)', host: { left: 0, right: 0, width: 0 }, at: [[0, 0]] },
    // 97 and 903 are just outside the 96px zones.
    { name: 'is zero away from both edges', host: HOST, at: [[500, 0], [97, 0], [903, 0]] },
    { name: 'ramps to the max step at the very edges', host: HOST, at: [[1000, MAX], [0, -MAX]] },
    // Halfway into either zone: 48/96 * 18 = 9.
    { name: 'ramps proportionally with proximity, rounding up', host: HOST, at: [[1000 - 48, 9], [48, -9]] },
    // width 200 → zone = min(96, 50) = 50; 25px in is halfway → 9.
    { name: 'caps the edge zone at a quarter of a narrow host', host: { left: 0, right: 200, width: 200 }, at: [[200, MAX], [175, 9]] },
    { name: 'honors a non-zero host left offset', host: { left: 400, right: 1400, width: 1000 }, at: [[400, -MAX], [1400, MAX], [900, 0]] },
  ])('$name', ({ host, at }) => {
    for (const [x, velocity] of at) expect(edgeAutoScrollVelocity(host, x), `at x=${x}`).toBe(velocity);
  });
});
