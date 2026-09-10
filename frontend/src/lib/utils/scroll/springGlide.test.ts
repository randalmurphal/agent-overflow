import { describe, expect, it } from 'vitest';
import { frame, makeHarness, type Harness } from './springTestHarness';
import { ARRIVAL_DISTANCE_PX } from './resolver';

describe('glide shaping and quantized landing', () => {
  function parkAt(h: Harness, target: number): void {
    h.setTarget(target);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 60; i++) frame();
    expect(Math.abs(h.getScrollTop() - target)).toBeLessThanOrEqual(ARRIVAL_DISTANCE_PX);
  }

  /** Frame the chase until within 1px of `target`, returning per-frame moves. */
  function movesUntilNear(h: Harness, target: number, budget: number): number[] {
    const moves: number[] = [];
    for (let i = 0; i < budget && Math.abs(h.getScrollTop() - target) > 1; i++) {
      const before = h.getScrollTop();
      frame();
      moves.push(h.getScrollTop() - before);
    }
    expect(Math.abs(h.getScrollTop() - target)).toBeLessThanOrEqual(1);
    return moves;
  }

  it('bounds the peak and keeps easing through the tail without a crawl', () => {
    const h = makeHarness();
    parkAt(h, 100);

    h.setTarget(160); // one sparse ~3-line quantum
    h.spring.markTargetChanged();
    const moves = movesUntilNear(h, 160, 60);

    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(7);
    }
    // Exclude the final exact arrival write from the monotone braking check.
    const peakIndex = moves.indexOf(Math.max(...moves));
    for (let i = peakIndex + 1; i < moves.length - 1; i++) {
      expect(moves[i]).toBeLessThanOrEqual(moves[i - 1] + 0.01);
    }
    expect(moves[moves.length - 1]).toBeLessThanOrEqual(1.55);
    const tail = moves.filter((move) => move < 1.2);
    expect(tail.length).toBeGreaterThanOrEqual(2);
    expect(tail.slice(0, -1).every((move) => move > 0.5)).toBe(true);
    expect(moves.some((move) => move > 0 && move < 0.95)).toBe(true);
  });

  it('eases a tiny growth in gently without a snap', () => {
    const h = makeHarness({ quantize: true });
    parkAt(h, 100);

    h.setTarget(103); // sub-line growth
    h.spring.markTargetChanged();
    const before = h.getScrollTop();
    frame();
    // Cold first frame is pure spring physics, (0.08·3)/1.25 ≈ 0.19 —
    // the envelope min (1.6) is an upper bound, never a forced speed —
    // under half a pixel, so the grid shows nothing yet.
    expect(h.velocity()).toBeGreaterThan(0.15);
    expect(h.velocity()).toBeLessThan(0.25);
    expect(h.getScrollTop()).toBe(before);
    // Then whole pixels accrue from that curve: never the 3px in one
    // frame, never a step backwards.
    const moves = movesUntilNear(h, 103, 20);
    expect(moves.length).toBeGreaterThanOrEqual(2);
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(1);
      expect(move).toBeGreaterThanOrEqual(0);
    }
  });

  it('keeps written values continuous under integer scrollTop rounding (remainder carry)', () => {
    // Quantized harness: writes land on whole pixels like the browser.
    // Without the remainder carry, each rounded-DOWN readback dropped
    // the sub-pixel progress and the next written value could regress
    // (write 100.4 → readback 100 → next write 100.2), a ±0.5px
    // sawtooth in requested positions at tail speeds. With carry, the
    // written sequence never moves backwards.
    const h = makeHarness({ quantize: true });
    parkAt(h, 100);

    const writesBefore = h.writes.length;
    h.setTarget(160);
    h.spring.markTargetChanged();
    for (let i = 0; i < 80 && Math.abs(h.getScrollTop() - 160) > 0; i++) frame();
    expect(h.getScrollTop()).toBe(160);

    const glideWrites = h.writes
      .slice(writesBefore)
      .filter((w) => w.caller === 'spring.tick' || w.caller === 'spring.overshoot')
      .map((w) => w.value);
    expect(glideWrites.length).toBeGreaterThan(10);
    for (let i = 1; i < glideWrites.length; i++) {
      expect(glideWrites[i]).toBeGreaterThanOrEqual(glideWrites[i - 1]);
    }
  });
});

describe('engine grid integration', () => {
  it('submits only writes that move a whole device pixel', () => {
    const refreshInterval = 1000 / 165;
    const h = makeHarness({ quantize: true });
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 330; i++) frame(refreshInterval);

    const writesBefore = h.writes.length;
    h.setTarget(160);
    h.spring.markTargetChanged();
    let movingFrames = 0;
    for (let i = 0; i < 495 && h.getScrollTop() < 158; i++) {
      const before = h.getScrollTop();
      frame(refreshInterval);
      if (h.getScrollTop() !== before) movingFrames++;
    }

    expect(h.getScrollTop()).toBeGreaterThanOrEqual(158);
    const submittedWrites = h.writes.length - writesBefore;
    // A tick whose displacement rounds to no device pixel writes
    // nothing, so writes never outnumber moving frames beyond the exact
    // landing write.
    expect(submittedWrites).toBeLessThanOrEqual(movingFrames + 1);
  });

  function glideSteps(
    h: Harness,
    hz: number,
  ): { steps: number[]; writes: number[] } {
    const interval = 1000 / hz;
    h.setTarget(96);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < hz * 3 && h.getScrollTop() !== 96; i++) frame(interval);
    expect(h.getScrollTop(), `park at ${hz}Hz`).toBe(96);
    const writesBefore = h.writes.length;
    h.setTarget(160);
    h.spring.markTargetChanged();
    const steps: number[] = [];
    for (let i = 0; i < hz * 3 && h.getScrollTop() < 160; i++) {
      const before = h.getScrollTop();
      frame(interval);
      steps.push(h.getScrollTop() - before);
    }
    expect(h.getScrollTop(), `land at ${hz}Hz`).toBe(160);
    return { steps, writes: h.writes.slice(writesBefore).map((w) => w.value) };
  }

  it('a measured flooring engine at DPR 2 moves immediately at 165Hz', () => {
    // Flooring must not swallow fractional requests during the onset.
    const h = makeHarness({ floorGrid: true, dpr: 2 });
    const { steps, writes } = glideSteps(h, 165);
    // First motion within a handful of ticks of the onset, no frozen run.
    expect(steps.findIndex((step) => step !== 0)).toBeLessThanOrEqual(5);
    // Interior requests sit halfway into each flooring interval.
    for (const value of writes.slice(0, -1)) expect(Number.isInteger(value - 0.5)).toBe(true);
  });

  it('independent springs use the same measured grid from their first tick', () => {
    const first = makeHarness({ cssGrid: true, dpr: 2 });
    glideSteps(first, 60);
    const second = makeHarness({ cssGrid: true, dpr: 2 });
    const { writes } = glideSteps(second, 60);
    for (const value of writes) expect(Number.isInteger(value)).toBe(true);
  });

  it('a max-scroll clamp cannot corrupt the measured grid', () => {
    // A native max-scroll clamp provides no evidence about the grid.
    const dpr = 2.625;
    const h = makeHarness({ quantize: true, dpr, clampMax: 130 });
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 180 && h.getScrollTop() !== 100; i++) frame();
    h.setTarget(160);
    h.spring.markTargetChanged();
    for (let i = 0; i < 120; i++) frame();
    expect(h.getScrollTop()).toBe(130);
    // After the range recovers, the same grid supports the reverse glide.
    h.setTarget(80); // 210 device pixels exactly
    h.spring.markTargetChanged();
    const writesBefore = h.writes.length;
    for (let i = 0; i < 240 && h.getScrollTop() !== 80; i++) frame();
    expect(h.getScrollTop()).toBe(80);
    const tail = h.writes.slice(writesBefore).slice(-6, -1).map((w) => w.value);
    for (const value of tail) {
      expect(Math.abs(value * dpr - Math.round(value * dpr)), `${value}`).toBeLessThan(1e-6);
    }
    expect(tail.some((value) => !Number.isInteger(value))).toBe(true);
  });

  it.each([60, 120, 165, 240])('recalibrated grid changes preserve forward and reverse glides at %sHz', (hz) => {
    const engine = { quantize: true, dpr: 1, floorGrid: false };
    const h = makeHarness(engine);
    h.setTarget(400);
    h.spring.markTargetChanged();
    h.spring.start();
    for (const [dpr, floor, target] of [[1.25, false, 500], [1.5, false, 100], [2, true, 500], [2.625, false, 80]] as const) {
      for (let i = 0; i < 10; i++) frame(1000 / hz);
      engine.dpr = dpr;
      engine.floorGrid = floor;
      h.setTarget(target);
      h.spring.markTargetChanged();
      for (let i = 0; i < hz * 4 && Math.abs(h.getScrollTop() - target) > 0.01; i++) {
        const before = h.getScrollTop();
        frame(1000 / hz);
        expect(Math.abs(h.getScrollTop() - before)).toBeLessThan(29);
      }
      expect(Math.abs(h.getScrollTop() - target)).toBeLessThanOrEqual(1);
      expect(h.refusalEvents).toHaveLength(0);
    }
  });

});
