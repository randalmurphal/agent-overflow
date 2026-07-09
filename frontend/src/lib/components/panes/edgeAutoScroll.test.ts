import { describe, expect, it } from 'vitest';
import {
  AUTO_SCROLL_MAX_STEP_PX,
  edgeAutoScrollVelocity,
  type HorizontalEdges,
} from './edgeAutoScroll';

// A 1000px-wide host: edge zone = min(96, 1000/4) = 96px on each side.
const HOST: HorizontalEdges = { left: 0, right: 1000, width: 1000 };

describe('edgeAutoScrollVelocity', () => {
  it('is zero for a zero-width host (no edge zone)', () => {
    expect(edgeAutoScrollVelocity({ left: 0, right: 0, width: 0 }, 0)).toBe(0);
  });

  it('is zero away from both edges', () => {
    expect(edgeAutoScrollVelocity(HOST, 500)).toBe(0);
    // Just outside the 96px zones.
    expect(edgeAutoScrollVelocity(HOST, 97)).toBe(0);
    expect(edgeAutoScrollVelocity(HOST, 903)).toBe(0);
  });

  it('ramps to the max step at the very edges', () => {
    expect(edgeAutoScrollVelocity(HOST, 1000)).toBe(AUTO_SCROLL_MAX_STEP_PX);
    expect(edgeAutoScrollVelocity(HOST, 0)).toBe(-AUTO_SCROLL_MAX_STEP_PX);
  });

  it('ramps proportionally with proximity, rounding up', () => {
    // Halfway into the right zone: 48/96 * 18 = 9.
    expect(edgeAutoScrollVelocity(HOST, 1000 - 48)).toBe(9);
    // Halfway into the left zone: -9.
    expect(edgeAutoScrollVelocity(HOST, 48)).toBe(-9);
  });

  it('caps the edge zone at a quarter of a narrow host', () => {
    // width 200 → zone = min(96, 50) = 50; at the right edge → max step.
    const narrow: HorizontalEdges = { left: 0, right: 200, width: 200 };
    expect(edgeAutoScrollVelocity(narrow, 200)).toBe(AUTO_SCROLL_MAX_STEP_PX);
    // 25px in (halfway of the 50px zone) → 9.
    expect(edgeAutoScrollVelocity(narrow, 175)).toBe(9);
  });

  it('honors a non-zero host left offset', () => {
    const offset: HorizontalEdges = { left: 400, right: 1400, width: 1000 };
    expect(edgeAutoScrollVelocity(offset, 400)).toBe(-AUTO_SCROLL_MAX_STEP_PX);
    expect(edgeAutoScrollVelocity(offset, 1400)).toBe(AUTO_SCROLL_MAX_STEP_PX);
    expect(edgeAutoScrollVelocity(offset, 900)).toBe(0);
  });
});
