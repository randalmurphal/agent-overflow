// Wall-clock phase for the ambient indicator animations, plus the device
// pixel ratio their sprite frame width snaps to.
//
// The animations themselves are plain CSS (app.css: `ambient-pulse`,
// `ambient-led`, `ambient-spin`, `working-sprite-run`). CSS can express
// the waveform but not the PHASE: an animation starts when its element
// does, so dots that appear as threads start running would each blink on
// their own beat. Measured 2026-08-23: 40 dots mounted 37ms apart land in
// 13 different 125ms slots without this, and in exactly 1 with it.
//
// One delegated `animationstart` listener catches every ambient animation
// as it begins and pins its `startTime` so the animation's local time IS
// wall-clock time modulo its own period. CSS `animation-delay` is left
// alone and keeps working as a per-element stagger: the delay lives
// inside the effect, and `startTime` shifts the whole thing.
//
// This is NOT a ticker. Nothing polls and nothing writes per frame; each
// animation is aligned once, in the animation-frame callbacks of the frame
// its `animationstart` is delivered in, before that frame paints. One pass
// per frame aligns every animation that started in it, from a single
// `document.getAnimations()`, reading all of them before writing any.
// Per-element `getAnimations()` scans every animation in the document and
// flushes style that the previous `startTime` write dirtied, so aligning
// each indicator as its event arrived is quadratic in the number mounting
// together. Aligning does not cost the compositing
// that makes these animations free — measured 0 style recalcs and 0.0ms
// of main-thread work with 40 aligned dots running.

/** The CSS animations this module owns. Anything else on the page —
 * `animate-spin`, view transitions, svelte transition/FLIP effects — is
 * left strictly alone. */
const AMBIENT_ANIMATIONS: ReadonlySet<string> = new Set([
  'ambient-pulse',
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

/** The `startTime` that puts animation on the wall-clock beat, or null when
 * it is not ours to align. Reads only, so a batch of these flushes style
 * once. */
function alignedStart(animation: Animation): number | null {
  if (aligned.has(animation)) return null;
  // Strict allowlist. Only a CSS animation running one of OUR keyframes is
  // ours to rewind. Anything else reaching this — a script-driven
  // `element.animate()` (svelte's FLIP/transitions drive sidebar rows), a
  // CSSTransition, a foreign keyframe — carries no `animationName` we
  // recognise, and rewinding it would jump a transition mid-flight.
  const name = (animation as Partial<CSSAnimation>).animationName;
  if (typeof name !== 'string' || !AMBIENT_ANIMATIONS.has(name)) return null;
  const duration = toMs(animation.effect?.getComputedTiming().duration);
  if (duration === null || duration <= 0) return null;
  const timelineNow = toMs(animation.timeline?.currentTime);
  if (timelineNow === null) return null;
  // localTime = timelineNow - startTime, so this makes localTime equal
  // Date.now() % duration right now, and every animation of the same
  // period agrees regardless of when its element mounted.
  return timelineNow - (Date.now() % duration);
}

/** Align every animation given: all reads first, then all writes. */
function alignAll(animations: Iterable<Animation>): void {
  const starts: [Animation, number][] = [];
  for (const animation of animations) {
    const start = alignedStart(animation);
    if (start !== null) starts.push([animation, start]);
  }
  for (const [animation, start] of starts) {
    aligned.add(animation);
    animation.startTime = start;
  }
}

let pendingFrame: number | null = null;

function alignStarted(): void {
  pendingFrame = null;
  alignAll(document.getAnimations());
}

function onAnimationStart(event: AnimationEvent): void {
  if (!AMBIENT_ANIMATIONS.has(event.animationName)) return;
  if (typeof document.getAnimations !== 'function') return;
  pendingFrame ??= requestAnimationFrame(alignStarted);
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
  // Capture phase: `animationstart` bubbles, but a handler in between
  // cannot stop it reaching a capturing listener.
  document.addEventListener('animationstart', onAnimationStart, true);
  // Anything already running when this installs (a fast first paint)
  // still needs pinning.
  if (typeof document.getAnimations === 'function') alignAll(document.getAnimations());
  rearmDpr();
  let stopped = false;
  return () => {
    if (stopped) return;
    stopped = true;
    document.removeEventListener('animationstart', onAnimationStart, true);
    if (pendingFrame !== null) cancelAnimationFrame(pendingFrame);
    pendingFrame = null;
    dprQuery?.removeEventListener('change', rearmDpr);
    dprQuery = null;
    lastDpr = 0;
    document.documentElement.style.removeProperty('--dpr');
  };
}
