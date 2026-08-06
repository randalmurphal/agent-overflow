// Ambient-indicator ticker: drives the app's standing indicator visuals
// (pulse dots, the composer working-LED chase, sidebar status glows,
// stepped spinners) from ONE wall-clock-aligned JS timer that writes
// inline styles directly on the indicator elements, instead of
// per-element CSS animations.
//
// Why not CSS animations: a running CSS animation ticks main-thread
// style recalc every vsync for as long as it is active — even with a
// steps() timing function, which discretizes the OUTPUT but not the
// timeline. The stepped indicator animations cost ~60 style recalcs/sec
// (measured 2026-07-20: 65.7 recalcs/s with indicators running vs 7.0/s
// paused), and every recalc allocates ComputedStyle objects in Blink's
// Oilpan heap — a continuous churn feeder for the renderer's
// committed-page ratchet. steps() fixed the GPU present rate (see
// app.css); this fixes the style-recalc rate.
//
// Why inline styles on the elements, not CSS variables on <html>: a
// custom-property write on the root — even one no rule references —
// invalidates style for the ENTIRE document, because custom properties
// are inherited style data (measured: ~13ms per write at a 5k-node DOM,
// ~90ms at 30k, vs 0.007ms for an inline opacity write on the leaf).
// The first cut of this ticker used root vars and turned 8 cheap
// write-batches/sec into 12% of a core; leaf writes make the recalc
// scope exactly the elements that changed.
//
// Discovery is by the same marker classes the retired animations used —
// `animate-pulse` (+ `ambient-pulse-s2`/`-s4` phase shifts),
// `working-leds` (wrapper; children are the LEDs), `status-glow-warning`
// / `status-glow-info` (host var read by its ::before — pseudo-elements
// can't take inline styles, and a host-scoped var invalidates only that
// row's subtree), and `stepped-spin`. Components keep toggling classes;
// no registration API to thread through call sites. Elements that lose
// their marker class (or leave the DOM) get their inline styles cleared
// on the next tick, so conditional indicators fall back to their static
// CSS rest state.
//
// Waveforms replicate the retired keyframes exactly — see the pure
// *At() functions. All values derive from wall-clock time, so every
// indicator in the app shares one phase regardless of mount time
// (previously approximated with grid-aligned animation-delays).
//
// Reduced motion: writes are withheld and anything written is cleared,
// leaving the static CSS rest states (pulse at base opacity, LEDs at
// their resting 0.45, glow at t=0 border-only, spinner unrotated).
// Deliberately the OS preference only: the app's low-power setting is
// enforced upstream — surfaces pass `animate={false}` / drop the marker
// classes, so low power leaves the ticker nothing to drive.
import { prefersReducedMotion, reducedMotionQuery } from './reducedMotion';

/** One step slot. Every waveform's value changes only on multiples of
 * this (pulse 125ms, LEDs/glow 250ms, spin 125ms), so one grid-aligned
 * timer catches every boundary exactly. */
export const AMBIENT_SLOT_MS = 125;

const mod = (x: number, m: number): number => ((x % m) + m) % m;

/**
 * `animate-pulse` opacity. Retired keyframes: base → 0.5 @50% → base
 * over 2s, steps(8, jump-none) per keyframe segment — 8 evenly spaced
 * values across each 1s half, endpoints included, 125ms per slot.
 */
export function pulseOpacityAt(tMs: number): number {
  const slot = Math.floor(mod(tMs, 2000) / AMBIENT_SLOT_MS);
  return slot < 8 ? 1 - slot / 14 : 0.5 + (slot - 8) / 14;
}

/**
 * Working-LED chase opacities. Retired keyframes: a left-to-right
 * one-hot chase, one LED lit (1) and two resting (0.2), advancing every
 * 250ms over a 750ms cycle.
 */
export function ledOpacitiesAt(tMs: number): [number, number, number] {
  const phase = Math.floor(mod(tMs, 750) / 250);
  return [phase === 0 ? 1 : 0.2, phase === 1 ? 1 : 0.2, phase === 2 ? 1 : 0.2];
}

/**
 * Status-glow interpolation factor t ∈ [0,1]. Retired keyframes: rest →
 * peak @50% → rest over 2.5s, steps(5, jump-none) per segment — 5 values
 * per 1.25s half, 250ms per slot. app.css derives box-shadow spread/
 * alpha and opacity from this single factor.
 */
export function glowTAt(tMs: number): number {
  const slot = Math.floor(mod(tMs, 2500) / 250);
  return slot < 5 ? slot / 4 : (9 - slot) / 4;
}

/**
 * SteppedSpinner rotation in degrees. One 30° spoke advance per slot,
 * 12 spokes ≈ 1.5s per revolution (retired CSS: steps(12) over 1.2s —
 * retimed to the shared slot grid so the spinner's jumps coincide with
 * every other indicator's writes).
 */
export function spinAngleAt(tMs: number): number {
  return Math.floor(mod(tMs, 1500) / AMBIENT_SLOT_MS) * 30;
}

function formatValue(v: number): string {
  return String(Math.round(v * 10000) / 10000);
}

type StyledKind = 'pulse' | 'leds' | 'glow' | 'spin';

let refs = 0;
let timer: ReturnType<typeof setTimeout> | null = null;
/** Elements carrying ticker-written inline styles, for mark-and-sweep
 * clearing when they lose their marker class or leave the collections. */
const styled = new Map<Element, StyledKind>();

function setStyle(el: Element, property: 'opacity' | 'transform', value: string): void {
  const style = (el as HTMLElement | SVGElement).style;
  if (style.getPropertyValue(property) !== value) style.setProperty(property, value);
}

function clearStyles(el: Element, kind: StyledKind): void {
  const style = (el as HTMLElement | SVGElement).style;
  switch (kind) {
    case 'pulse':
      style.removeProperty('opacity');
      break;
    case 'leds':
      for (const led of el.children) {
        (led as HTMLElement).style.removeProperty('opacity');
      }
      break;
    case 'glow':
      style.removeProperty('--ambient-glow-t');
      break;
    case 'spin':
      style.removeProperty('transform');
      break;
  }
}

function clearAllStyles(): void {
  for (const [el, kind] of styled) clearStyles(el, kind);
  styled.clear();
}

function writeStyles(tMs: number): void {
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
    setStyle(el, 'opacity', cls.contains('ambient-pulse-s2') ? pulseS2 : cls.contains('ambient-pulse-s4') ? pulseS4 : pulse);
    mark(el, 'pulse');
  }

  const leds = ledOpacitiesAt(tMs).map(formatValue);
  for (const el of document.getElementsByClassName('working-leds')) {
    for (let i = 0; i < el.children.length && i < 3; i += 1) {
      setStyle(el.children[i]!, 'opacity', leds[i]!);
    }
    mark(el, 'leds');
  }

  const glowT = formatValue(glowTAt(tMs));
  for (const name of ['status-glow-warning', 'status-glow-info']) {
    for (const el of document.getElementsByClassName(name)) {
      const style = (el as HTMLElement).style;
      if (style.getPropertyValue('--ambient-glow-t') !== glowT) {
        style.setProperty('--ambient-glow-t', glowT);
      }
      mark(el, 'glow');
    }
  }

  const spin = `rotate(${spinAngleAt(tMs)}deg)`;
  for (const el of document.getElementsByClassName('stepped-spin')) {
    setStyle(el, 'transform', spin);
    mark(el, 'spin');
  }

  // Sweep: anything styled on a previous tick that no longer matches a
  // marker class (toggled off, or detached) falls back to its CSS rest
  // state. Clearing a detached element is harmless.
  for (const [el, kind] of styled) {
    if (!seen.has(el)) {
      clearStyles(el, kind);
      styled.delete(el);
    }
  }
}

function tick(): void {
  if (prefersReducedMotion()) {
    // Static indicators: fall back to the CSS rest states.
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
  reducedMotionQuery()?.addEventListener('change', syncNow);
  document.addEventListener('visibilitychange', syncNow);
  tick();
}

function uninstall(): void {
  if (timer !== null) clearTimeout(timer);
  timer = null;
  reducedMotionQuery()?.removeEventListener('change', syncNow);
  document.removeEventListener('visibilitychange', syncNow);
  clearAllStyles();
}

/**
 * Start the singleton ticker (refcounted — concurrent starts share one
 * timer). Returns an idempotent stop; the last stop halts the timer and
 * clears every inline style it wrote so a stopped ticker leaves no
 * residue.
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
