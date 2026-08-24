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

  // The ticker's refcount is module state. A test whose assertion throws
  // before its own stop() would leave refs above zero and every LATER
  // test would silently start a ticker that never installs — one stale
  // test then reads as three failures. Every start goes through this and
  // afterEach releases whatever is left.
  const starts: (() => void)[] = [];
  function start(): () => void {
    const stop = startAmbientTicker();
    starts.push(stop);
    return stop;
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
    for (const stop of starts.splice(0)) stop();
    for (const el of fixtures.splice(0)) el.remove();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('writes the glow variable immediately and clears it on stop', () => {
    const warning = addFixture('<div class="status-glow-warning"></div>');
    const info = addFixture('<div class="status-glow-info"></div>');

    const stop = start();
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
    const stop = start();
    for (const t of [250, 500, 1000, 1250, 1500, 2250, 2500]) {
      vi.setSystemTime(t);
      vi.advanceTimersByTime(AMBIENT_SLOT_MS);
      expect(glowVar(glow), `t=${t}`).toBe(String(glowTAt(t)));
    }
    stop();
  });

  it('writes nothing to the CSS-animated indicators', () => {
    // The glow is the only indicator this timer owns. The pulse dots,
    // the LED chase, the stepped spinner and the sprite are CSS
    // animations (app.css) phase-locked by ambientPhase.ts, running off
    // the main thread — a tick that writes an inline style onto any of
    // them is the regression to catch, because the write both forfeits
    // the layer promotion that makes them free and fights the animation
    // for the same property.
    const dot = addFixture('<span class="animate-pulse"></span>');
    const shifted = addFixture('<span class="animate-pulse ambient-pulse-s2"></span>');
    const chase = addFixture(
      '<span class="working-leds"><i class="working-led"></i><i class="working-led"></i><i class="working-led"></i></span>',
    );
    const spinner = addFixture('<svg class="stepped-spin"></svg>');
    const strip = addFixture('<span class="working-sprite-strip"></span>');

    start();
    vi.setSystemTime(500);
    vi.advanceTimersByTime(AMBIENT_SLOT_MS);

    for (const el of [dot, shifted, spinner, strip, ...chase.children]) {
      expect(el.getAttribute('style'), el.getAttribute('class') ?? el.tagName).toBe(null);
    }
  });

  it('suspends the timer when nothing is on screen, and wakes on a DOM change', async () => {
    start();
    // Nothing carries a marker class, so the idle budget runs out and the
    // timer stops completely rather than waking 8x a second forever.
    vi.advanceTimersByTime(AMBIENT_SLOT_MS * 12);
    expect(vi.getTimerCount(), 'timer should be suspended with no consumers').toBe(0);

    const glow = addFixture('<div class="status-glow-warning"></div>');
    // The wake observer delivers as a microtask, so the first write lands
    // in the same checkpoint as the insertion — no visible delay.
    await Promise.resolve();

    expect(glowVar(glow), 'a new consumer must wake the timer').not.toBe('');
    expect(vi.getTimerCount()).toBe(1);
  });

  it('leaves nothing armed after the last stop, suspended or not', async () => {
    const stop = start();
    vi.advanceTimersByTime(AMBIENT_SLOT_MS * 12); // suspend
    stop();

    const glow = addFixture('<div class="status-glow-warning"></div>');
    await Promise.resolve();

    expect(glowVar(glow), 'a stopped ticker must not still be observing').toBe('');
    expect(vi.getTimerCount()).toBe(0);
  });

  it('sweeps the glow variable from elements that lose their marker class', () => {
    const glow = addFixture('<div class="status-glow-warning"></div>');
    const stop = start();
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
    const stopA = start();
    const stopB = start();
    stopA();
    stopA(); // double-stop must not release B's hold
    vi.advanceTimersByTime(500); // advances the mocked clock with it
    expect(glowVar(glow)).toBe(String(glowTAt(Date.now())));
    stopB();
    expect(glowVar(glow)).toBe('');
    // A fresh start after full teardown works again.
    const stopC = start();
    expect(glowVar(glow)).toBe(String(glowTAt(Date.now())));
    stopC();
  });

  it('withholds writes under prefers-reduced-motion so CSS rest states apply', () => {
    matchMediaMock.matches = true;
    const glow = addFixture('<div class="status-glow-warning"></div>');
    const stop = start();
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
      const stop = start();
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
