// The waveforms of the ambient CSS animations in app.css (`ambient-pulse`,
// `ambient-led`, `ambient-spin`). They are the authoritative specification
// for the CSS: ambientCss.browser.test.ts samples the rendered animations
// against them slot by slot, so drift in either direction fails. The app
// runs the CSS, not these; the glow, the one indicator a JS timer still
// drives, keeps its waveform (glowTAt) in utils/ambientTicker.ts.
import { AMBIENT_SLOT_MS } from '../../lib/utils/ambientTicker';

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
 * SteppedSpinner rotation in degrees. One 30° spoke advance per slot,
 * 12 spokes = 1.5s per revolution.
 */
export function spinAngleAt(tMs: number): number {
  return Math.floor(mod(tMs, 1500) / AMBIENT_SLOT_MS) * 30;
}
