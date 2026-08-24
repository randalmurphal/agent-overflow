// Ambient-indicator waveforms, and the indicators that need a JS timer.
//
// Two mechanisms, and which one an indicator gets is decided by WHERE it
// mounts, not by taste:
//
//   CSS keyframes on a compositable property, phase-locked to wall clock
//   by utils/ambientPhase.ts (app.css: `ambient-led`, `ambient-spin`,
//   `working-sprite-run`). Blink promotes the element and ticks it off
//   the main thread, so these cost no style recalc and no paint at all.
//
//   This timer, writing inline styles (`.animate-pulse` opacity, the
//   status glow's `--ambient-glow-t`). An inline write creates no
//   Animation object.
//
// That last sentence is the whole reason the timer still exists.
// Anything mounted INSIDE the timeline scroller must not create an
// animation object: an active animation flips Blink's present policy to
// smoothness-priority, which licenses presenting a frame with tiles
// still un-rastered. The timeline's core moves are compensated
// viewport-space moves (head splices, prune shifts, bottom-held
// toggles) — rows that stay screen-stationary while every tile
// invalidates at once — and under smoothness-priority that is a
// checkerboard where text used to be (incident 2026-08-17). See
// docs/architecture/frontend-scroll.md § The Print Doctrine.
//
// `.animate-pulse` is rendered by fourteen chat row components through
// components/chat/Indicator.svelte, so every running tool call would
// hold a live animation in the scroller. It stays on this timer.
// `chat/timelineKeyframeAnimations.test.ts` is the tripwire that keeps
// it that way.
//
// The status glow stays for a second, independent reason: its
// `--ambient-glow-t` drives three things at once in app.css — shadow
// spread 0→2px, shadow alpha 0→0.22, and the ::before's opacity
// 0.7→1.0, which lifts the 1px border with it. box-shadow is not
// compositable, and opacity alone cannot reproduce spread GROWTH: the
// ring would sit at full width and fade in rather than expand.
//
// Measured 2026-08-23, per 5s with four indicators running:
//
//   glow as a CSS box-shadow animation   324 style recalcs   85.1ms
//   glow on this timer                    40 style recalcs  127.0ms
//   LED/spin/sprite as CSS animations      0 style recalcs    0.0ms
//
// A write is what makes a tick expensive, and the timer stops entirely
// when there is nothing to write — see `armWake` below.
//
// The waveform functions are the authoritative specification for both
// mechanisms, not dead code: ambientCss.browser.test.ts samples the
// rendered CSS animations against them slot by slot, and the tests in
// this module's suite pin the inline writes to the same functions, so
// drift in either direction fails.
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

type StyledKind = 'pulse' | 'glow';

const GLOW_CLASSES = ['status-glow-warning', 'status-glow-info'] as const;

/** Consecutive empty ticks before the timer suspends. One second, so a
 * surface that briefly has no indicator does not thrash the timer off
 * and on. */
const IDLE_TICKS_BEFORE_SUSPEND = 8;

let refs = 0;
let timer: ReturnType<typeof setTimeout> | null = null;
let wake: MutationObserver | null = null;
let idleTicks = 0;
let stopPhase: (() => void) | null = null;
/** Elements carrying ticker-written inline styles, for mark-and-sweep
 * clearing when they lose their marker class or leave the document. */
const styled = new Map<Element, StyledKind>();

function clearStyles(el: Element, kind: StyledKind): void {
  const style = (el as HTMLElement | SVGElement).style;
  if (kind === 'pulse') style.removeProperty('opacity');
  else style.removeProperty('--ambient-glow-t');
}

function clearAllStyles(): void {
  for (const [el, kind] of styled) clearStyles(el, kind);
  styled.clear();
}

function setStyle(el: Element, property: string, value: string): void {
  const style = (el as HTMLElement | SVGElement).style;
  if (style.getPropertyValue(property) !== value) style.setProperty(property, value);
}

/** Returns whether any consumer is currently on screen. */
function writeStyles(tMs: number): boolean {
  const seen = new Set<Element>();
  const mark = (el: Element, kind: StyledKind): void => {
    seen.add(el);
    styled.set(el, kind);
  };

  const pulse = formatValue(pulseOpacityAt(tMs));
  const pulseS2 = formatValue(pulseOpacityAt(tMs - 250));
  const pulseS4 = formatValue(pulseOpacityAt(tMs - 500));
  for (const el of document.getElementsByClassName('animate-pulse')) {
    const cls = el.classList;
    setStyle(
      el,
      'opacity',
      cls.contains('ambient-pulse-s2') ? pulseS2 : cls.contains('ambient-pulse-s4') ? pulseS4 : pulse,
    );
    mark(el, 'pulse');
  }

  const glowT = formatValue(glowTAt(tMs));
  for (const name of GLOW_CLASSES) {
    for (const el of document.getElementsByClassName(name)) {
      setStyle(el, '--ambient-glow-t', glowT);
      mark(el, 'glow');
    }
  }

  // Sweep: anything styled on a previous tick that no longer carries a
  // marker class (toggled off, or detached) falls back to its CSS rest
  // state. Clearing a detached element is harmless.
  for (const [el, kind] of styled) {
    if (!seen.has(el)) {
      clearStyles(el, kind);
      styled.delete(el);
    }
  }
  return seen.size > 0;
}

function arm(): void {
  const now = Date.now();
  timer = setTimeout(tick, AMBIENT_SLOT_MS - (now % AMBIENT_SLOT_MS) || AMBIENT_SLOT_MS);
}

/**
 * Suspend the timer until the DOM changes. An indicator cannot appear
 * without either being inserted or having its class list rewritten, so
 * those two mutations are the complete wake set — the timer resumes in
 * the same microtask checkpoint as the change that needs it, with no
 * visible delay before the first write. The observer disconnects on its
 * first callback, so a busy document costs one callback per suspension,
 * never one per mutation.
 */
function armWake(): void {
  if (typeof MutationObserver !== 'function' || document.body === null) {
    arm();
    return;
  }
  wake = new MutationObserver(syncNow);
  wake.observe(document.body, {
    childList: true,
    subtree: true,
    attributes: true,
    attributeFilter: ['class'],
  });
}

function disarmWake(): void {
  wake?.disconnect();
  wake = null;
}

function tick(): void {
  timer = null;
  if (prefersReducedMotion()) {
    // Static indicators. The reduced-motion change listener is the wake
    // signal, so no timer and no observer while the preference holds.
    clearAllStyles();
    return;
  }
  if (document.hidden) {
    // Nothing renders. Leave stale values and stop entirely; the
    // visibilitychange listener re-syncs the instant the page is back.
    return;
  }
  idleTicks = writeStyles(Date.now()) ? 0 : idleTicks + 1;
  if (idleTicks >= IDLE_TICKS_BEFORE_SUSPEND) armWake();
  else arm();
}

function syncNow(): void {
  if (timer !== null) clearTimeout(timer);
  timer = null;
  disarmWake();
  idleTicks = 0;
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
  disarmWake();
  idleTicks = 0;
  reducedMotionQuery()?.removeEventListener('change', syncNow);
  document.removeEventListener('visibilitychange', syncNow);
  stopPhase?.();
  stopPhase = null;
  clearAllStyles();
}

/**
 * Start the ambient indicator machinery (refcounted — concurrent starts
 * share one timer and one phase listener). Returns an idempotent stop;
 * the last stop halts the timer, removes every listener and clears every
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
