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

  const glowVar = (el: HTMLElement): string => el.style.getPropertyValue('--ambient-glow-t');

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

  it('writes the glow variable immediately and clears it on stop', () => {
    const warning = addFixture('<div class="status-glow-warning"></div>');
    const info = addFixture('<div class="status-glow-info"></div>');

    const stop = startAmbientTicker();
    expect(glowVar(warning)).toBe('0'); // glow at t=0
    expect(glowVar(info)).toBe('0');

    vi.advanceTimersByTime(250);
    expect(glowVar(warning)).toBe(String(glowTAt(250)));

    stop();
    expect(glowVar(warning)).toBe('');
    expect(glowVar(info)).toBe('');
  });

  it('advances the glow on the slot grid', () => {
    const glow = addFixture('<div class="status-glow-warning"></div>');
    const stop = startAmbientTicker();
    for (const t of [250, 500, 1000, 1250, 1500, 2250, 2500]) {
      vi.setSystemTime(t);
      vi.advanceTimersByTime(AMBIENT_SLOT_MS);
      expect(glowVar(glow), `t=${t}`).toBe(String(glowTAt(t)));
    }
    stop();
  });

  it('leaves pulse, LED and spinner elements entirely alone', () => {
    // These are CSS animations now (app.css), phase-locked by
    // ambientPhase.ts. A tick that writes to them is the regression this
    // whole change exists to prevent: an inline write is not composited,
    // so each one repaints the whole document.
    const dot = addFixture('<span class="animate-pulse"></span>');
    const shifted = addFixture('<span class="animate-pulse ambient-pulse-s2"></span>');
    const chase = addFixture(
      '<span class="working-leds"><i class="working-led"></i><i class="working-led"></i><i class="working-led"></i></span>',
    );
    const spinner = addFixture('<svg class="stepped-spin"></svg>');

    const stop = startAmbientTicker();
    vi.advanceTimersByTime(1000);

    expect(dot.getAttribute('style')).toBe(null);
    expect(shifted.getAttribute('style')).toBe(null);
    for (const led of chase.children) expect(led.getAttribute('style')).toBe(null);
    expect(spinner.getAttribute('style')).toBe(null);
    stop();
  });

  it('sweeps the glow variable from elements that lose their marker class', () => {
    const glow = addFixture('<div class="status-glow-warning"></div>');
    const stop = startAmbientTicker();
    vi.setSystemTime(250);
    vi.advanceTimersByTime(AMBIENT_SLOT_MS);
    expect(glowVar(glow)).not.toBe('');
    glow.classList.remove('status-glow-warning');
    vi.advanceTimersByTime(AMBIENT_SLOT_MS);
    expect(glowVar(glow)).toBe('');
    // Re-adding the class picks it back up on the next tick.
    glow.classList.add('status-glow-warning');
    vi.advanceTimersByTime(AMBIENT_SLOT_MS);
    expect(glowVar(glow)).not.toBe('');
    stop();
  });

  it('is refcounted: the ticker survives until the last stop, stops are idempotent', () => {
    const glow = addFixture('<div class="status-glow-warning"></div>');
    const stopA = startAmbientTicker();
    const stopB = startAmbientTicker();
    stopA();
    stopA(); // double-stop must not release B's hold
    vi.advanceTimersByTime(500); // advances the mocked clock with it
    expect(glowVar(glow)).toBe(String(glowTAt(Date.now())));
    stopB();
    expect(glowVar(glow)).toBe('');
    // A fresh start after full teardown works again.
    const stopC = startAmbientTicker();
    expect(glowVar(glow)).toBe(String(glowTAt(Date.now())));
    stopC();
  });

  it('withholds writes under prefers-reduced-motion so CSS rest states apply', () => {
    matchMediaMock.matches = true;
    const glow = addFixture('<div class="status-glow-warning"></div>');
    const stop = startAmbientTicker();
    vi.advanceTimersByTime(1000);
    expect(glowVar(glow)).toBe('');
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
      const glow = addFixture('<div class="status-glow-warning"></div>');
      const stop = startAmbientTicker();
      vi.setSystemTime(250);
      vi.advanceTimersByTime(1000);
      expect(glowVar(glow)).toBe('');
      hidden = false;
      document.dispatchEvent(new Event('visibilitychange'));
      expect(glowVar(glow)).not.toBe('');
      stop();
    } finally {
      delete (document as { hidden?: boolean }).hidden;
    }
  });
});
