// Ambient-indicator waveforms, and the one indicator that still needs a
// JS timer.
//
// Everything the app pulses, chases, or spins is a CSS animation on a
// compositable property (see app.css: `ambient-pulse`, `ambient-led`,
// `ambient-spin`, `working-sprite-run`), phase-locked to wall clock by
// utils/ambientPhase.ts. Those cost no main-thread work at all: Blink
// promotes an element for a known animation and ticks opacity/transform
// off the main thread.
//
// The status glow is the exception, and deliberately so. Its
// `--ambient-glow-t` drives three things at once in app.css — shadow
// spread 0→2px, shadow alpha 0→0.22, and the ::before's opacity
// 0.7→1.0, which lifts the 1px border with it. box-shadow is not
// compositable, and opacity alone cannot reproduce spread GROWTH: the
// ring would sit at full width and fade in rather than expand. Measured
// 2026-08-23, per 5s with four indicators running:
//
//   glow as a CSS box-shadow animation   324 style recalcs   85.1ms
//   glow on this timer                    40 style recalcs  127.0ms
//   pulse/spin/sprite as CSS animations    0 style recalcs    0.0ms
//
// So the timer stays for the glow only. It costs nothing until a glow is
// actually on screen: with no `.status-glow-*` element the loops below
// write nothing, and a write is what makes a tick expensive — each one
// forces a whole-document lifecycle, which is why driving the other
// indicators this way cost more total main-thread time than the CSS
// keyframes it replaced.
//
// The waveform functions are the authoritative specification for the CSS
// keyframes, not dead code: ambientCss.browser.test.ts samples the
// rendered animations against them slot by slot, so drift in either
// direction fails.
import { prefersReducedMotion, reducedMotionQuery } from './reducedMotion';
import { startAmbientPhase } from './ambientPhase';

/** One step slot. Every waveform's value changes only on multiples of
 * this (pulse 125ms, LEDs/glow 250ms, spin 125ms), so one grid-aligned
 * timer catches every boundary exactly. */
export const AMBIENT_SLOT_MS = 125;

const mod = (x: number, m: number): number => ((x % m) + m) % m;

/**
 * `animate-pulse` opacity. Keyframes: base → 0.5 @50% → base over 2s,
 * steps(8, jump-none) per keyframe segment — 8 evenly spaced values
 * across each 1s half, endpoints included, 125ms per slot.
 */
export function pulseOpacityAt(tMs: number): number {
  const slot = Math.floor(mod(tMs, 2000) / AMBIENT_SLOT_MS);
  return slot < 8 ? 1 - slot / 14 : 0.5 + (slot - 8) / 14;
}

/**
 * Working-LED chase opacities. Keyframes: a left-to-right one-hot chase,
 * one LED lit (1) and two resting (0.2), advancing every 250ms over a
 * 750ms cycle.
 */
export function ledOpacitiesAt(tMs: number): [number, number, number] {
  const phase = Math.floor(mod(tMs, 750) / 250);
  return [phase === 0 ? 1 : 0.2, phase === 1 ? 1 : 0.2, phase === 2 ? 1 : 0.2];
}

/**
 * Status-glow interpolation factor t ∈ [0,1]. Keyframes: rest → peak
 * @50% → rest over 2.5s, steps(5, jump-none) per segment — 5 values per
 * 1.25s half, 250ms per slot. app.css derives box-shadow spread/alpha
 * and opacity from this single factor.
 */
export function glowTAt(tMs: number): number {
  const slot = Math.floor(mod(tMs, 2500) / 250);
  return slot < 5 ? slot / 4 : (9 - slot) / 4;
}

/**
 * SteppedSpinner rotation in degrees. One 30° spoke advance per slot,
 * 12 spokes = 1.5s per revolution.
 */
export function spinAngleAt(tMs: number): number {
  return Math.floor(mod(tMs, 1500) / AMBIENT_SLOT_MS) * 30;
}

function formatValue(v: number): string {
  return String(Math.round(v * 10000) / 10000);
}

const GLOW_CLASSES = ['status-glow-warning', 'status-glow-info'] as const;

let refs = 0;
let timer: ReturnType<typeof setTimeout> | null = null;
let stopPhase: (() => void) | null = null;
/** Elements carrying the ticker-written glow variable, for mark-and-sweep
 * clearing when they lose their marker class or leave the document. */
const styled = new Set<Element>();

function clearAllStyles(): void {
  for (const el of styled) (el as HTMLElement).style.removeProperty('--ambient-glow-t');
  styled.clear();
}

function writeStyles(tMs: number): void {
  const glowT = formatValue(glowTAt(tMs));
  const seen = new Set<Element>();
  for (const name of GLOW_CLASSES) {
    for (const el of document.getElementsByClassName(name)) {
      seen.add(el);
      const style = (el as HTMLElement).style;
      if (style.getPropertyValue('--ambient-glow-t') !== glowT) {
        style.setProperty('--ambient-glow-t', glowT);
      }
      styled.add(el);
    }
  }
  // Sweep: anything styled on a previous tick that no longer carries a
  // glow class (toggled off, or detached) falls back to the var()
  // fallback, which is the t=0 rest state. Clearing a detached element
  // is harmless.
  for (const el of styled) {
    if (!seen.has(el)) {
      (el as HTMLElement).style.removeProperty('--ambient-glow-t');
      styled.delete(el);
    }
  }
}

function tick(): void {
  if (prefersReducedMotion()) {
    // Static indicators: fall back to the CSS rest state.
    clearAllStyles();
  } else if (!document.hidden) {
    writeStyles(Date.now());
  }
  // Hidden: skip writes (nothing renders; Chromium throttles this timer
  // to 1Hz anyway) and leave stale values — the visibilitychange
  // listener re-syncs the instant the page is visible again.
  const now = Date.now();
  timer = setTimeout(tick, AMBIENT_SLOT_MS - (now % AMBIENT_SLOT_MS) || AMBIENT_SLOT_MS);
}

function syncNow(): void {
  if (timer !== null) clearTimeout(timer);
  tick();
}

function install(): void {
  stopPhase = startAmbientPhase();
  reducedMotionQuery()?.addEventListener('change', syncNow);
  document.addEventListener('visibilitychange', syncNow);
  tick();
}

function uninstall(): void {
  if (timer !== null) clearTimeout(timer);
  timer = null;
  reducedMotionQuery()?.removeEventListener('change', syncNow);
  document.removeEventListener('visibilitychange', syncNow);
  stopPhase?.();
  stopPhase = null;
  clearAllStyles();
}

/**
 * Start the ambient indicator machinery (refcounted — concurrent starts
 * share one timer and one phase listener). Returns an idempotent stop;
 * the last stop halts the timer, removes the listener and clears every
 * inline style it wrote, so a stopped ticker leaves no residue.
 */
export function startAmbientTicker(): () => void {
  refs += 1;
  if (refs === 1) install();
  let stopped = false;
  return () => {
    if (stopped) return;
    stopped = true;
    refs -= 1;
    if (refs === 0) uninstall();
  };
}
