import { describe, expect, it } from 'vitest';
import { AMBIENT_SLOT_MS } from '../../lib/utils/ambientTicker';
import { ledOpacitiesAt, pulseOpacityAt, spinAngleAt } from './ambientWaveforms';

// Waveform tables assert exact fidelity against the retired CSS
// keyframes: pulse 2s steps(8, jump-none) per segment (base→0.5→base),
// LED chase 750ms one-hot step-end, spin one 30° spoke per 125ms slot.

describe('pulseOpacityAt', () => {
  it('descends base→0.5 across the first 1s segment in 8 jump-none values', () => {
    expect(pulseOpacityAt(0)).toBe(1);
    expect(pulseOpacityAt(124)).toBe(1); // holds within a slot
    expect(pulseOpacityAt(125)).toBeCloseTo(1 - 1 / 14, 10);
    expect(pulseOpacityAt(875)).toBeCloseTo(0.5, 10); // slot 7: segment end value
  });

  it('ascends 0.5→base across the second segment, holding endpoints across joins', () => {
    // jump-none doubles the endpoint values across the segment join:
    // slot 7 (875-999ms) and slot 8 (1000-1124ms) are both 0.5, and the
    // cycle wrap holds base for 250ms (slot 15 + slot 0).
    expect(pulseOpacityAt(1000)).toBeCloseTo(0.5, 10);
    expect(pulseOpacityAt(1125)).toBeCloseTo(0.5 + 1 / 14, 10);
    expect(pulseOpacityAt(1875)).toBe(1); // slot 15
    expect(pulseOpacityAt(2000)).toBe(1); // wrap = slot 0
  });

  it('is 2s-periodic and mirror-symmetric around the trough', () => {
    for (const t of [0, 125, 300, 875, 1250, 1999]) {
      expect(pulseOpacityAt(t + 2000)).toBe(pulseOpacityAt(t));
    }
    expect(pulseOpacityAt(875 - 125)).toBeCloseTo(pulseOpacityAt(1125), 10);
  });

  it('handles negative time (phase shifts sample the past)', () => {
    expect(pulseOpacityAt(-250)).toBe(pulseOpacityAt(1750));
    expect(pulseOpacityAt(-2000)).toBe(pulseOpacityAt(0));
  });
});

describe('ledOpacitiesAt', () => {
  it('chases left-to-right, one lit LED per 250ms phase', () => {
    expect(ledOpacitiesAt(0)).toEqual([1, 0.2, 0.2]);
    expect(ledOpacitiesAt(249)).toEqual([1, 0.2, 0.2]);
    expect(ledOpacitiesAt(250)).toEqual([0.2, 1, 0.2]);
    expect(ledOpacitiesAt(500)).toEqual([0.2, 0.2, 1]);
    expect(ledOpacitiesAt(750)).toEqual([1, 0.2, 0.2]); // wraps
  });
});

describe('spinAngleAt', () => {
  it('advances one 30° spoke per slot, 12 spokes per 1.5s revolution', () => {
    expect(spinAngleAt(0)).toBe(0);
    expect(spinAngleAt(AMBIENT_SLOT_MS)).toBe(30);
    expect(spinAngleAt(11 * AMBIENT_SLOT_MS)).toBe(330);
    expect(spinAngleAt(1500)).toBe(0);
  });
});
