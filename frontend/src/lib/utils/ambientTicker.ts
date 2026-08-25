// Ambient-indicator waveforms, and the one indicator that still needs a
// JS timer.
//
// Everything else is CSS keyframes on a compositable property,
// phase-locked to wall clock by utils/ambientPhase.ts (app.css:
// `ambient-pulse`, `ambient-led`, `ambient-spin`, `working-sprite-run`).
// Blink promotes those elements and ticks them off the main thread, so
// they cost no style recalc and no paint at all.
//
// The sidebar status glow is one exception, and the reason is a
// PROPERTY, not a place. Its `--ambient-glow-t` drives three things at
// once in app.css — shadow spread 0→2px, shadow alpha 0→0.22, and the
// ::before's opacity 0.7→1.0, which lifts the 1px border with it.
// box-shadow is not compositable, and opacity alone cannot reproduce
// spread GROWTH: the ring would sit at full width and fade in rather
// than expand. So this timer writes that one custom property inline on
// the rows carrying a glow class.
//
// `.animate-pulse` is the other exception, and its reason is LAYER
// COUNT, not a property. A running opacity animation — steps() or not —
// promotes its element to its own composited layer
// (CompositingReason::kActiveOpacityAnimation), and the pulse class sits
// on one working dot per busy thread/subagent row: a fleet of agents put
// 18 of the app's 26 layers on 6px dots (measured 2026-08-25, user app
// under multi-agent load). This timer instead writes each dot's opacity
// INLINE, exactly like the glow: per-element writes, mark-and-sweep
// clearing, CSS rest state (opacity 1) when a write stops.
//
// NOT root custom properties, and that was measured, not guessed: the
// first de-promotion attempt wrote three `--ambient-pulse-*` vars on
// the document root per slot, and a root-level custom-property write
// invalidates style for the WHOLE document, not just the consumers —
// 381 recalc passes of ~3,500 elements at 22-30ms each over a 10s
// window on the user's app (2026-08-25, `probe frames`), ~195ms/s of
// recalc that dropped frames under every scroll spring. A per-element
// inline write invalidates one 6px span; at fleet scale (tens of dots,
// 8 writes/s each) the recalc cost is single-digit ms/s. The ~28
// whole-document repaints/sec that killed the ORIGINAL ticker pulse
// came from the sprite's background-position write, not from opacity
// on leaf spans. History of all three flips lives at app.css
// `--animate-pulse`.
//
// Measured 2026-08-23, per 5s: the sprite's old inline write cost
// 163.0ms of main-thread work; as a CSS animation it costs 0.0ms.
//
// A write is what makes a tick expensive, and the timer stops entirely
// when there is nothing to write — see `armWake` below. A thread
// pending user action is the only thing that arms it, so in an ordinary
// session it is suspended.
//
// The waveform functions are the authoritative specification for the
// CSS, not dead code: ambientCss.browser.test.ts samples the rendered
// animations against them slot by slot, and this module's suite pins the
// glow write to glowTAt, so drift in either direction fails.
import { prefersReducedMotion, reducedMotionQuery } from './reducedMotion';
import { startAmbientPhase } from './ambientPhase';

/** One step slot: the grid every ambient waveform lands its value
 * changes on (pulse 125ms, LEDs/glow 250ms, spin 125ms). The CSS
 * animations step on it, and the timer arms on it, so a glow write
 * catches its 250ms boundaries exactly. */
export const AMBIENT_SLOT_MS = 125;

const mod = (x: number, m: number): number => ((x % m) + m) % m;

/**
 * `animate-pulse` opacity, mirroring app.css `@keyframes ambient-pulse`:
 * base → 0.5 @50% → base over 2s, steps(8, jump-none) per keyframe
 * segment — 8 evenly spaced values across each 1s half, endpoints
 * included, 125ms per slot.
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

/** Stagger lead for a pulse dot: Indicator.svelte's backgrounded
 * variant marks its second and third dots one and two 250ms slots
 * AHEAD of the base waveform (matching the deleted CSS
 * animation-delays — a negative delay shows the waveform ahead). */
function pulseLeadFor(el: Element): number {
  const classes = el.classList;
  if (classes.contains('ambient-pulse-s4')) return 500;
  if (classes.contains('ambient-pulse-s2')) return 250;
  return 0;
}

/** Consecutive empty ticks before the timer suspends. One second, so a
 * surface that briefly has no indicator does not thrash the timer off
 * and on. */
const IDLE_TICKS_BEFORE_SUSPEND = 8;

let refs = 0;
let timer: ReturnType<typeof setTimeout> | null = null;
let wake: MutationObserver | null = null;
let idleTicks = 0;
let stopPhase: (() => void) | null = null;
/** Elements carrying a ticker-written inline style (`--ambient-glow-t`
 * on glow rows, `opacity` on pulse dots), for mark-and-sweep clearing
 * when they lose their marker class or leave the document. */
const styled = new Set<Element>();

function clearStyles(el: Element): void {
  const style = (el as HTMLElement | SVGElement).style;
  style.removeProperty('--ambient-glow-t');
  style.removeProperty('opacity');
}

function clearAllStyles(): void {
  for (const el of styled) clearStyles(el);
  styled.clear();
}

/** Returns whether any consumer is currently on screen. */
function writeStyles(tMs: number): boolean {
  const seen = new Set<Element>();
  const glowT = formatValue(glowTAt(tMs));

  for (const el of document.getElementsByClassName('animate-pulse')) {
    const style = (el as HTMLElement | SVGElement).style;
    const value = formatValue(pulseOpacityAt(tMs + pulseLeadFor(el)));
    if (style.getPropertyValue('opacity') !== value) {
      style.setProperty('opacity', value);
    }
    seen.add(el);
    styled.add(el);
  }

  for (const name of GLOW_CLASSES) {
    for (const el of document.getElementsByClassName(name)) {
      const style = (el as HTMLElement | SVGElement).style;
      if (style.getPropertyValue('--ambient-glow-t') !== glowT) {
        style.setProperty('--ambient-glow-t', glowT);
      }
      seen.add(el);
      styled.add(el);
    }
  }

  // Sweep: anything styled on a previous tick that no longer carries a
  // marker class (toggled off, or detached) falls back to its CSS rest
  // state — glow t=0, pulse opacity 1. Clearing a detached element is
  // harmless.
  for (const el of styled) {
    if (!seen.has(el)) {
      clearStyles(el);
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
