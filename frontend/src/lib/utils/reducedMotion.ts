// The app's one motion gate.
//
// CSS transitions/animations are silenced globally by the
// `prefers-reduced-motion` reset in app.css, but imperative motion —
// scrollTop spring glides, rAF-driven style writes, FLIP inversions —
// has to check for itself. Every JS motion site gates here so the OS
// preference and the app-level low-power setting always ride together.
import { getSettings } from '../stores/settings.svelte';

// Cached MediaQueryList — some callers check inside 60Hz ticks, and
// parsing the query plus constructing a fresh `MediaQueryList` per call
// is wasted work. Keyed on the `matchMedia` function's identity, not
// cached once: in production that is one construction ever, and a test
// stubbing `window.matchMedia` is a new identity, so its stub takes
// effect without a test-only reset hook.
let query: MediaQueryList | null = null;
let cachedFrom: typeof window.matchMedia | null = null;

/**
 * The cached `(prefers-reduced-motion: reduce)` list, for callers that
 * need to attach change listeners; null when `matchMedia` is missing.
 */
export function reducedMotionQuery(): MediaQueryList | null {
  const mm =
    typeof window !== 'undefined' && typeof window.matchMedia === 'function'
      ? window.matchMedia
      : null;
  if (mm === null) return null;
  if (query === null || cachedFrom !== mm) {
    query = mm.call(window, '(prefers-reduced-motion: reduce)');
    cachedFrom = mm;
  }
  return query;
}

/** OS accessibility preference only. */
export function prefersReducedMotion(): boolean {
  return reducedMotionQuery()?.matches ?? false;
}

/**
 * The full gate: OS reduced-motion OR the app's low-power setting.
 * Low power means "place instantly, never animate" — spring glides are
 * the app's dominant GPU cost, and the reveal smoother and working-LED
 * chase gate on the same setting at their own sites. Read live (plain
 * non-reactive read; callers sample per event/tick, so a toggle
 * applies to the next decision without any subscription).
 */
export function motionReduced(): boolean {
  return prefersReducedMotion() || getSettings().lowPowerMode;
}
