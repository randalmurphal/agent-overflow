// Wall-clock phase for the ambient indicator animations, plus the device
// pixel ratio their sprite frame width snaps to.
//
// The animations themselves are plain CSS (app.css: `ambient-led`,
// `ambient-spin`, `working-sprite-run`). CSS can express
// the waveform but not the PHASE: an animation starts when its element
// does, so dots that appear as threads start running would each blink on
// their own beat. Measured 2026-08-23: 40 dots mounted 37ms apart land in
// 13 different 125ms slots without this, and in exactly 1 with it.
//
// `.animate-pulse` is deliberately NOT in this set and is not a CSS
// animation at all — it mounts inside the timeline scroller, where an
// animation object flips the present policy. utils/ambientTicker.ts
// drives it with inline writes instead.
//
// One delegated `animationstart` listener catches every ambient animation
// as it begins and pins its `startTime` so the animation's local time IS
// wall-clock time modulo its own period. CSS `animation-delay` is left
// alone and keeps working as a per-element stagger: the delay lives
// inside the effect, and `startTime` shifts the whole thing.
//
// This is NOT a ticker. Nothing polls and nothing writes per frame; each
// animation is aligned once, in the microtask before its first paint, so
// there is no visible phase jump. Aligning does not cost the compositing
// that makes these animations free — measured 0 style recalcs and 0.0ms
// of main-thread work with 40 aligned dots running.

/** The CSS animations this module owns. Anything else on the page —
 * `animate-spin`, view transitions, auto-animate's WAAPI effects — is
 * left strictly alone. */
const AMBIENT_ANIMATIONS: ReadonlySet<string> = new Set([
  'ambient-led',
  'ambient-spin',
  'working-sprite-run',
]);

/** Aligned effects, so a re-fired `animationstart` (a class toggled off
 * and on again reuses nothing, but an element can carry several) never
 * re-pins one and jolts it mid-cycle. */
const aligned = new WeakSet<Animation>();

/** Times come back as a number, as a typed CSSUnitValue, or — for
 * `duration` — as the string 'auto'. Only a real number is usable. */
function toMs(value: unknown): number | null {
  if (typeof value === 'number') return Number.isFinite(value) ? value : null;
  if (typeof value === 'object' && value !== null) {
    const unit = (value as { value?: unknown }).value;
    if (typeof unit === 'number' && Number.isFinite(unit)) return unit;
  }
  return null;
}

function alignOne(animation: Animation): void {
  if (aligned.has(animation)) return;
  // Strict allowlist. Only a CSS animation running one of OUR keyframes is
  // ours to rewind. Anything else reaching this — a script-driven
  // `element.animate()` (auto-animate runs one per sidebar row), a
  // CSSTransition, a foreign keyframe — carries no `animationName` we
  // recognise, and rewinding it would jump a transition mid-flight.
  const name = (animation as Partial<CSSAnimation>).animationName;
  if (typeof name !== 'string' || !AMBIENT_ANIMATIONS.has(name)) return;
  const duration = toMs(animation.effect?.getComputedTiming().duration);
  if (duration === null || duration <= 0) return;
  const timelineNow = toMs(animation.timeline?.currentTime);
  if (timelineNow === null) return;
  aligned.add(animation);
  // localTime = timelineNow - startTime, so this makes localTime equal
  // Date.now() % duration right now, and every animation of the same
  // period agrees regardless of when its element mounted.
  animation.startTime = timelineNow - (Date.now() % duration);
}

function onAnimationStart(event: AnimationEvent): void {
  if (!AMBIENT_ANIMATIONS.has(event.animationName)) return;
  const target = event.target;
  if (!(target instanceof Element)) return;
  if (typeof target.getAnimations !== 'function') return;
  for (const animation of target.getAnimations()) alignOne(animation);
}

// --- device pixel ratio -----------------------------------------------
//
// `.working-sprite` rounds one frame to a whole number of device pixels
// (see the note in app.css). CSS cannot read the ratio, so it reads
// `--dpr` instead. Written once at install and again only when the ratio
// actually changes — a moved window, a display-scaling change — which is
// the one time invalidating the whole document for a root custom
// property is already unavoidable.

let dprQuery: MediaQueryList | null = null;
let lastDpr = 0;

function writeDpr(): void {
  const dpr = window.devicePixelRatio || 1;
  if (dpr === lastDpr) return;
  lastDpr = dpr;
  document.documentElement.style.setProperty('--dpr', String(dpr));
}

/** Re-arms on every change: a `(resolution: Xdppx)` query only reports
 * leaving the ratio it was built for. */
function rearmDpr(): void {
  writeDpr();
  if (typeof window.matchMedia !== 'function') return;
  dprQuery?.removeEventListener('change', rearmDpr);
  dprQuery = window.matchMedia(`(resolution: ${window.devicePixelRatio || 1}dppx)`);
  dprQuery.addEventListener('change', rearmDpr);
}

/**
 * Install the phase aligner and the `--dpr` watcher. Idempotent per
 * returned stop; the ambient ticker owns the single call site.
 */
export function startAmbientPhase(): () => void {
  // Capture phase: `animationstart` bubbles, but capture also sees
  // targets inside shadow roots and cannot be stopped by a handler in
  // between.
  document.addEventListener('animationstart', onAnimationStart, true);
  // Anything already running when this installs (a fast first paint)
  // still needs pinning.
  if (typeof document.getAnimations === 'function') {
    for (const animation of document.getAnimations()) alignOne(animation);
  }
  rearmDpr();
  let stopped = false;
  return () => {
    if (stopped) return;
    stopped = true;
    document.removeEventListener('animationstart', onAnimationStart, true);
    dprQuery?.removeEventListener('change', rearmDpr);
    dprQuery = null;
    lastDpr = 0;
    document.documentElement.style.removeProperty('--dpr');
  };
}
