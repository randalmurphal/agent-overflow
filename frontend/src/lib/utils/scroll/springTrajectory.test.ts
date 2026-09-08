import { describe, expect, it } from 'vitest';
import { frame, makeHarness } from './springTestHarness';

const rates = [30, 60, 90, 120, 144, 165, 220, 240, 360, 480];
const grids = [1, 0.8, 1 / 1.5, 0.5, 1 / 2.625, 1 / 3];

describe('continuous trajectory on a native grid', () => {
  it.each(rates)('bounds spatial error across grids, targets, and rounding rules at %iHz', (hz) => {
    for (const quantum of grids) for (const floorGrid of [false, true]) {
      for (const target of [1, 3, 20, 64, 600]) {
        const h = makeHarness({ quantum, floorGrid });
        const reference = makeHarness();
        for (const chase of [h, reference]) {
          chase.setTarget(target);
          chase.spring.markTargetChanged();
          chase.spring.start();
        }
        let maxError = 0;
        let previous = 0;
        let minimumStep = 0;
        for (let i = 0; i < hz * 6; i++) {
          frame(1000 / hz);
          const current = h.getScrollTop();
          maxError = Math.max(maxError, Math.abs(current - reference.getScrollTop()));
          minimumStep = Math.min(minimumStep, current - previous);
          previous = current;
          if (h.velocity() === 0 && reference.getScrollTop() === target && i >= hz / 2) break;
        }
        const label = `${quantum} grid, floor=${floorGrid}, target=${target}`;
        // The terminal exact write can floor a fractional target to the lower
        // accepted position. Interior motion has the tighter half-grid bound.
        expect(minimumStep, label).toBeGreaterThanOrEqual(-1e-6);
        expect(maxError, label).toBeLessThanOrEqual(quantum + 1e-4);
        expect(Math.abs(h.getScrollTop() - target), label).toBeLessThanOrEqual(quantum);
        expect(h.refusalEvents, label).toHaveLength(0);
        h.spring.cancel();
        reference.spring.cancel();
      }
    }
  });

  it.each([144, 165, 240])('has no velocity ladder at %iHz, including streamed retargets and reversals', (hz) => {
    const h = makeHarness({ quantize: true });
    const reference = makeHarness();
    for (const chase of [h, reference]) {
      chase.setTarget(600);
      chase.spring.start();
    }
    for (let i = 0; i < hz * 3; i++) {
      if (i === Math.floor(hz / 2) || i === hz) {
        for (const chase of [h, reference]) {
          chase.setTarget(i === hz ? 0 : 750);
          chase.spring.markTargetChanged();
        }
      }
      frame(1000 / hz);
      // Exclude terminal rounding: both models must still be in motion.
      if (Math.abs(reference.getTarget() - reference.getScrollTop()) > 1) {
        expect(Math.abs(h.getScrollTop() - reference.getScrollTop()), `frame ${i}`).toBeLessThanOrEqual(0.5001);
        expect(h.velocity(), `velocity at frame ${i}`).toBeCloseTo(reference.velocity(), 4);
      }
    }
  });

  it.each([120, 144, 165, 240, 480])('lands a paragraph without a final-pixel crawl at %iHz', (hz) => {
    const h = makeHarness({ quantize: true });
    h.setTarget(64);
    h.spring.start();
    let lastMove = 0;
    let largestGap = 0;
    for (let i = 1; i <= hz * 2 && h.getScrollTop() !== 64; i++) {
      const before = h.getScrollTop();
      frame(1000 / hz);
      if (h.getScrollTop() !== before) {
        if (lastMove !== 0) largestGap = Math.max(largestGap, (i - lastMove) * 1000 / hz);
        lastMove = i;
      }
    }
    expect(h.getScrollTop()).toBe(64);
    // Continuous braking bounds the slowest pixel crossing; no k,2k,3k
    // frame schedule and no exponential last-pixel wait. One refresh of
    // sampling error is allowed on top of the 30ms continuous crossing.
    expect(largestGap).toBeLessThanOrEqual(30 + 1000 / hz);
  });

  it('uses elapsed time immediately across monitor switches and jitter without a cadence warmup', () => {
    const h = makeHarness({ quantum: 1 / 2.625, readbackPrecision: 1 / 32, float32: true });
    const reference = makeHarness();
    for (const chase of [h, reference]) {
      chase.setTarget(4000);
      chase.spring.start();
    }
    let tick = 0;
    for (const hz of [60, 240, 144, 165, 120, 60]) {
      for (let i = 0; i < 30; i++) {
        const jitter = [0.97, 1.02, 1.01, 1][tick++ % 4];
        frame(1000 / hz * jitter);
        expect(Math.abs(h.getScrollTop() - reference.getScrollTop())).toBeLessThanOrEqual(1 / 2.625 / 2 + 1 / 64 + 1e-3);
        expect(h.velocity()).toBeCloseTo(reference.velocity(), 4);
      }
    }
    expect(h.refusalEvents).toHaveLength(0);
  });
});
