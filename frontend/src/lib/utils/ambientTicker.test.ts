import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  AMBIENT_SLOT_MS,
  glowTAt,
  startAmbientTicker,
} from './ambientTicker';

// The glow table asserts exact fidelity against the retired CSS keyframes:
// glow 2.5s steps(5, jump-none) (0→1→0).

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

  it('writes no inline style onto the CSS-animated indicators or the root', () => {
    // The glow's carrier rows are the ticker's ONLY write target. The
    // pulse dots, LED chase, stepped spinner and sprite are all CSS
    // animations (app.css) phase-locked by ambientPhase.ts and ticked
    // off the main thread — an inline write onto any of them fights the
    // animation for the same property AND puts the tick's cost back on
    // the main thread, one whole-document lifecycle per tick. The
    // DOCUMENT ROOT is off-limits on top of that: a root custom-property
    // write invalidates style for the whole document (~22ms/pass at 3.5k
    // elements, measured 2026-08-25 — the 2fps-springs regression). Both
    // of the 2026-08-25 pulse flips are pinned dead here.
    const chase = addFixture(
      '<span class="working-leds"><i class="working-led"></i><i class="working-led"></i><i class="working-led"></i></span>',
    );
    const spinner = addFixture('<svg class="stepped-spin"></svg>');
    const strip = addFixture('<span class="working-sprite-strip"></span>');
    const dots = [
      addFixture('<span class="animate-pulse"></span>'),
      addFixture('<span class="animate-pulse ambient-pulse-s2"></span>'),
      addFixture('<span class="animate-pulse ambient-pulse-s4"></span>'),
    ];

    start();
    vi.setSystemTime(500);
    vi.advanceTimersByTime(AMBIENT_SLOT_MS);

    for (const el of [spinner, strip, ...dots, ...chase.children]) {
      expect(el.getAttribute('style'), el.getAttribute('class') ?? el.tagName).toBe(null);
    }
    // Other machinery legitimately writes root vars (--dpr); the ticker
    // must contribute none.
    expect(document.documentElement.getAttribute('style') ?? '').not.toContain('--ambient-pulse');
  });

  it('suspends with only pulse dots on screen — they need no tick', () => {
    // The dots animate in CSS, so they are not consumers. A ticker that
    // stayed awake for them would pay 8 wakeups a second to write
    // nothing.
    addFixture('<span class="animate-pulse"></span>');
    start();
    vi.advanceTimersByTime(AMBIENT_SLOT_MS * 12);
    expect(vi.getTimerCount(), 'CSS-animated dots must not hold the timer open').toBe(0);
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

  it('stays suspended through unrelated streaming mutations and wakes for nested or newly marked glows', async () => {
    const host = addFixture('<div></div>');
    start();
    vi.advanceTimersByTime(AMBIENT_SLOT_MS * 12);
    const scans = vi.spyOn(document, 'getElementsByClassName');
    try {
      for (let batch = 0; batch < 20; batch++) {
        host.append(document.createTextNode('stream'), document.createElement('p'));
        host.className = `stream-${batch}`;
        await Promise.resolve();
        vi.advanceTimersByTime(AMBIENT_SLOT_MS);
      }
      expect(scans).not.toHaveBeenCalled();
      expect(vi.getTimerCount()).toBe(0);

      const nested = document.createElement('section');
      nested.innerHTML = '<div><span class="status-glow-info"></span></div>';
      host.append(nested);
      await Promise.resolve();
      const glow = nested.querySelector('span')!;
      expect(glowVar(glow)).toBe(String(glowTAt(Date.now())));
      nested.remove();
      vi.advanceTimersByTime(AMBIENT_SLOT_MS * 12);
      expect(glowVar(glow)).toBe('');
      expect(vi.getTimerCount()).toBe(0);

      host.className = 'status-glow-warning';
      await Promise.resolve();
      expect(glowVar(host)).toBe(String(glowTAt(Date.now())));
      expect(vi.getTimerCount()).toBe(1);
    } finally {
      scans.mockRestore();
    }
  });

  it('does not wake for a glow removed before mutation delivery', async () => {
    start();
    vi.advanceTimersByTime(AMBIENT_SLOT_MS * 12);
    const transient = addFixture('<div class="status-glow-info"></div>');
    transient.remove();
    await Promise.resolve();
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
