import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  AMBIENT_SLOT_MS,
  pulseOpacityAt,
  ledOpacitiesAt,
  glowTAt,
  spinAngleAt,
  startAmbientTicker,
} from './ambientTicker';

// Waveform tables assert exact fidelity against the retired CSS
// keyframes: pulse 2s steps(8, jump-none) per segment (base→0.5→base),
// LED chase 750ms one-hot step-end, glow 2.5s steps(5, jump-none)
// (0→1→0), spin one 30° spoke per 125ms slot.

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

describe('glowTAt', () => {
  it('steps 0→1 over the first 1.25s segment in 5 jump-none values', () => {
    expect(glowTAt(0)).toBe(0);
    expect(glowTAt(250)).toBe(0.25);
    expect(glowTAt(1000)).toBe(1);
  });

  it('descends 1→0 with endpoints held across joins', () => {
    expect(glowTAt(1250)).toBe(1); // second-segment start value
    expect(glowTAt(1500)).toBe(0.75);
    expect(glowTAt(2250)).toBe(0);
    expect(glowTAt(2500)).toBe(0); // wrap = slot 0
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

describe('startAmbientTicker', () => {
  let matchMediaMock: { matches: boolean };
  const fixtures: HTMLElement[] = [];

  function addFixture(html: string): HTMLElement {
    const host = document.createElement('div');
    host.innerHTML = html;
    const el = host.firstElementChild as HTMLElement;
    document.body.appendChild(el);
    fixtures.push(el);
    return el;
  }

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(0);
    matchMediaMock = { matches: false };
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => ({
        matches: matchMediaMock.matches,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
      })),
    );
  });

  afterEach(() => {
    for (const el of fixtures.splice(0)) el.remove();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('writes inline styles to marked elements immediately and clears them on stop', () => {
    const dot = addFixture('<span class="animate-pulse"></span>');
    const chase = addFixture(
      '<span class="working-leds"><i class="working-led"></i><i class="working-led"></i><i class="working-led"></i></span>',
    );
    const glow = addFixture('<div class="status-glow-warning"></div>');
    const spinner = addFixture('<svg class="stepped-spin"></svg>');

    const stop = startAmbientTicker();
    expect(dot.style.opacity).toBe('1'); // pulse at t=0
    expect((chase.children[0] as HTMLElement).style.opacity).toBe('1');
    expect((chase.children[1] as HTMLElement).style.opacity).toBe('0.2');
    expect(glow.style.getPropertyValue('--ambient-glow-t')).toBe('0');
    expect(spinner.style.transform).toBe('rotate(0deg)');

    stop();
    expect(dot.style.opacity).toBe('');
    expect((chase.children[0] as HTMLElement).style.opacity).toBe('');
    expect(glow.style.getPropertyValue('--ambient-glow-t')).toBe('');
    expect(spinner.style.transform).toBe('');
  });

  it('advances values on the slot grid', () => {
    const chase = addFixture(
      '<span class="working-leds"><i></i><i></i><i></i></span>',
    );
    const stop = startAmbientTicker();
    expect((chase.children[0] as HTMLElement).style.opacity).toBe('1');
    vi.advanceTimersByTime(250);
    expect((chase.children[0] as HTMLElement).style.opacity).toBe('0.2');
    expect((chase.children[1] as HTMLElement).style.opacity).toBe('1');
    stop();
  });

  it('applies the s2/s4 phase shifts to staggered pulse dots', () => {
    const d0 = addFixture('<span class="animate-pulse"></span>');
    const d2 = addFixture('<span class="animate-pulse ambient-pulse-s2"></span>');
    const d4 = addFixture('<span class="animate-pulse ambient-pulse-s4"></span>');
    const stop = startAmbientTicker();
    // The ticker writes values rounded to 4 decimals.
    const fmt = (v: number): string => String(Math.round(v * 10000) / 10000);
    expect(d0.style.opacity).toBe(fmt(pulseOpacityAt(0)));
    expect(d2.style.opacity).toBe(fmt(pulseOpacityAt(-250)));
    expect(d4.style.opacity).toBe(fmt(pulseOpacityAt(-500)));
    stop();
  });

  it('sweeps inline styles from elements that lose their marker class', () => {
    const dot = addFixture('<span class="animate-pulse"></span>');
    const stop = startAmbientTicker();
    expect(dot.style.opacity).not.toBe('');
    dot.classList.remove('animate-pulse');
    vi.advanceTimersByTime(AMBIENT_SLOT_MS);
    expect(dot.style.opacity).toBe('');
    // Re-adding the class picks it back up on the next tick.
    dot.classList.add('animate-pulse');
    vi.advanceTimersByTime(AMBIENT_SLOT_MS);
    expect(dot.style.opacity).not.toBe('');
    stop();
  });

  it('is refcounted: the ticker survives until the last stop, stops are idempotent', () => {
    const dot = addFixture('<span class="animate-pulse"></span>');
    const stopA = startAmbientTicker();
    const stopB = startAmbientTicker();
    stopA();
    stopA(); // double-stop must not release B's hold
    vi.advanceTimersByTime(500);
    expect(dot.style.opacity).not.toBe('');
    stopB();
    expect(dot.style.opacity).toBe('');
    // A fresh start after full teardown works again.
    const stopC = startAmbientTicker();
    expect(dot.style.opacity).not.toBe('');
    stopC();
  });

  it('withholds writes under prefers-reduced-motion so CSS rest states apply', () => {
    matchMediaMock.matches = true;
    const dot = addFixture('<span class="animate-pulse"></span>');
    const stop = startAmbientTicker();
    vi.advanceTimersByTime(1000);
    expect(dot.style.opacity).toBe('');
    stop();
  });

  it('skips writes while the document is hidden and re-syncs on visibility', () => {
    // document.hidden is a prototype accessor in happy-dom; shadow it
    // with an own property and delete to restore.
    let hidden = true;
    Object.defineProperty(document, 'hidden', {
      configurable: true,
      get: () => hidden,
    });
    try {
      const dot = addFixture('<span class="animate-pulse"></span>');
      const stop = startAmbientTicker();
      vi.advanceTimersByTime(1000);
      expect(dot.style.opacity).toBe('');
      hidden = false;
      document.dispatchEvent(new Event('visibilitychange'));
      expect(dot.style.opacity).not.toBe('');
      stop();
    } finally {
      delete (document as { hidden?: boolean }).hidden;
    }
  });
});
