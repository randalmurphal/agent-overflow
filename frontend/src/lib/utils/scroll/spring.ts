// Spring-chase animation strategy for the sticky-bottom controller
// (useStickToBottom): velocity-spring kinematics, the sentinel-alive
// retain window that keeps `springActive` true across streaming gaps,
// the one-shot structural-append eligibility window, and the
// oscillation clamp recovery.
//
// Division of labor: the controller decides WHEN the spring runs — the
// pure delivery resolver's decisions and the user-intent handlers call
// start/cancel/requestStop — while this module owns HOW a chase
// advances scrollTop frame to frame. Every write goes through
// `deps.writeScrollTop`, the controller's single scrollTop chokepoint,
// so the one-writer contract survives the extraction. Arrival-readback
// acceptance state stays controller-owned (the
// notifyLiveContentMaybeGrew path shares it) and is reached through
// deps.

import { springGateIsOpen, withinArrivalBand } from './resolver';
import { nowMs } from './time';
import { trace } from './trace';
import { isUiRenderTraceEnabled } from '../uiRenderTrace';
import type { ScrollWriteCaller } from './types';

// ===== Spring chase tuning =====
// Tuned from upstream stackblitz-labs/use-stick-to-bottom defaults
// (damping 0.7, stiffness 0.05, mass 1.25). A 0.05 stiffness takes
// roughly half a second to settle a one-line scroll target change, which
// leaves WebKit spending too long in the low-velocity rounded tail during
// fast streaming. 0.08 keeps the no-visible-overshoot shape but catches
// line-sized target jumps quickly enough that consecutive wraps read as one
// continuous follow.
const DEFAULT_SPRING = { damping: 0.7, stiffness: 0.08, mass: 1.25 } as const;
const SIXTY_FPS_INTERVAL_MS = 1000 / 60;
// Cap on how many fixed 60Hz steps one rAF tick may integrate. A
// stalled rAF (heavy frame, tab back from background) would otherwise
// pay its entire gap at once — a many-step advance the cross-target
// clamp turns into a visible teleport. Four steps ≈ 67ms of motion per
// real frame kept post-stall catch-up brisk but smooth with the original
// spring. The faster streaming-follow spring uses three steps to preserve
// the same bounded-burst behavior; anything
// longer is absorbed by subsequent frames and the arrival snap.
const SPRING_MAX_CATCHUP_STEPS = 3;
// Keep chasing for this long after the last positive grow event. Without
// this, the spring would consider itself "arrived" between streaming
// chunks and stop, then have to spin up again on the next chunk —
// visibly jittery at chunk boundaries. Once this window expires AND
// animationMode is still 'spring', the spring enters sentinel mode
// (re-rAFs without writing, keeping springToken non-zero) so
// `springActive` stays true across gaps > 350ms (async shiki loads,
// parseIncompleteMarkdown rebalances) for the two resolver decisions
// that key on it: the engine-compensation decline tier
// (resolveEngineCompensation) and the negative-delta mid-chase spring
// carve-out (resolveContentDelivery). The sentinel cancels on the next
// tick where animationMode flips to 'instant' (no live content advanced
// within the consumer's hold window — see MessageTimeline's
// content-keyed latch). No ordering between that hold window and this
// constant is required for correctness: a compensation arriving after
// the sentinel died resolves through the pass/redirect tiers, both safe
// (the historical HOLD > RETAIN cross-file invariant died with the
// descriptor gate — see resolveEngineCompensation's provenance notes).
export const RETAIN_ANIMATION_DURATION_MS = 350;
// Spring arrival: within the shared ARRIVAL_DISTANCE_PX band
// (scroll/resolver.ts) AND velocity below 0.5 px-per-60fps-frame means
// we've effectively settled.
const ARRIVAL_VELOCITY_THRESHOLD = 0.5;
// Momentum carry across catch-up. Content height grows in quanta during
// streaming — a line wrap moves the bottom ~20px, a command-output flush
// 60–150px — so the spring repeatedly reaches the bottom and idles in the
// gaps. When it catches up mid-stream (inside the retain window), its
// upward velocity is CLAMPED to this ceiling rather than zeroed, so the
// next quantum continues the existing motion instead of re-accelerating
// from a dead stop — the difference between one continuous glide and a
// string of stop-start pulses (the clamp-not-zero form removes the dead
// stop the original fixed ceiling still produced after every
// above-ceiling burst).
//
// The ceiling keeps carry from reintroducing the big→small snap that the
// historical diff===0 zeroing fixed: a carried velocity crosses a growth
// of size D on the very first frame when
// velocity > D · (mass − stiffness) / damping ≈ 1.67 · D
// for DEFAULT_SPRING, and the cross-target clamp then lands it as an
// instant snap instead of a glide. 4 is safe for even a sub-line ~3px
// growth (1.67 · 3 ≈ 5), and keeps the "stuck spring" snap-guard
// scenarios (frozen remnants ~8/14/28 from instant pins) shedding down
// exactly as the zeroing did, minus the dead stop.
//
// A quantum-EMA adaptive ceiling (floor 4, max 12, learned from growth
// quanta observed while parked) shipped briefly in 2026-07 and was
// removed: a real-session capture showed the sampler never fired —
// streaming quanta land mid-glide or across chase boundaries, never
// while parked within the same chase — so the machinery was dead weight
// and the fixed ceiling is all that ever ran.
const SPRING_CARRY_VELOCITY_CEILING = 4;
// Hard cap on chase speed, in px per 60Hz frame (18 ≈ 1080 px/s). Large
// distances — a command block appended mid-prose, a several-hundred-px
// correction routed through the spring — glide at a bounded constant
// speed instead of a distance-proportional zoom (~3000 px/s for a 400px
// jump), trading a few hundred ms of extra follow latency for motion
// that stays readable. Deliberately above the end-of-stream drain's peak
// content growth rate (~900 px/s) so steady-state follow never
// accumulates lag; genuine bulk layout corrections bypass the spring
// entirely (resolver decline tier / instant pins) and still snap.
const SPRING_MAX_VELOCITY_PX_PER_FRAME = 18;
// Minimum glide speed for the BODY of a chase, in px per 60Hz frame
// (1.6 ≈ 96 px/s). scrollTop lands on whole CSS pixels, so motion below
// ~1px per display frame renders as 1px steps every N frames — a
// 2026-07-04 165Hz session capture showed the exponential ease-out tail
// spending >half of each glide at 0.08–0.9 px/frame, i.e. 1px steps at
// an effective 14–55Hz: the reported "low fps" judder, with rAF cadence
// measured clean. The floor keeps stepping at ≥ ~96Hz effective
// (0.58px per 165Hz frame, 0.67px at 144Hz, 1.6px at 60Hz) — far above
// the observed judder band. Inside the settle-taper zone (the last
// SPRING_MIN / SPRING_SETTLE_TAPER_RATIO ≈ 4px) the effective minimum
// tapers below this value — see SPRING_SETTLE_TAPER_RATIO.
//
// TUNING (2026-07-04, same-day feedback): the floor first shipped at
// 2.5, which sat ABOVE the spring's natural peak velocity (≈0.13 · D)
// for quanta under ~19px — line-sized glides ran start-to-stop at the
// constant floor and read as a flat "glide, not a spring". Raise only
// if 1px stepping at ~96Hz proves visible; every bump trades spring
// character on small quanta for step rate.
//
// Applied only when the spring's own velocity already points at the
// target, so direction reversals still turn around on the spring curve
// instead of snapping. Snap-safety: for realistic quanta (≥ ~3px, cross
// bound ≈1.67 · D ≥ 5) the floor never crosses a fresh growth in one
// frame; a rare sub-1.5px growth is at most LANDED ON in one frame —
// the cross-target clamp bounds that move to the growth itself, which
// is smaller than a regular floor step. Must also stay below
// SPRING_CARRY_VELOCITY_CEILING so carried momentum still dominates
// mid-stream.
const SPRING_MIN_VELOCITY_PX_PER_FRAME = 1.6;
// Deceleration envelope: max chase speed as a fraction of the REMAINING
// distance (per 60Hz frame), floored at SPRING_MIN and capped at
// SPRING_MAX. This is what shapes the perceived ease-out — speed bleeds
// off in proportion to how close the glide is — and it doubles as the
// small-quantum peak limiter: a single-line 26px growth peaks at
// ≈ 0.11·26 ≈ 2.6 px/frame instead of the raw spring's ≈ 3.4, and a
// carried (≤4) start into a line is immediately shaped down to the same
// envelope (2026-07-04 feedback: line-sized glides started too fast;
// catch-up latency explicitly matters less than perceived smoothness).
// 0.11 sits just under the spring's natural quasi-steady follow ratio
// (≈ 0.145·remaining at 60Hz), so it binds gently rather than fighting
// the integrator. Large chases cruise at SPRING_MAX until the envelope
// takes over below ≈ 164px remaining, giving big glides a progressive
// slowdown instead of cruise-until-stop.
const SPRING_DECEL_ENVELOPE_RATIO = 0.11;
// Settle taper: inside the last SPRING_MIN / this ≈ 4px, the minimum
// speed is remaining · this instead of the flat floor, so a glide ends
// with a RITARDANDO — 1px steps whose intervals grow monotonically
// (≈1.6 → 1.2 → 0.9 → 0.7 → 0.5 → 0.4 → land, ~130ms) — rather than
// constant-speed-then-stop. The judder distinction is rhythm, not
// speed: the captured bad tails hovered at a near-CONSTANT slow step
// rate for 300–500ms, which the eye locks onto as a beat; a monotone
// deceleration visits each rate once, reads as "coming to rest", and
// the sub-1px remainder never crosses a pixel boundary at all.
//
// TUNING (2026-07-04): 0.4 (a ~4px / ~70ms settle) read as a hard
// "drop and stick" landing; 0.25 stretches the settle to ~6.5px /
// ~130ms for a cradled stop. Lower = longer, softer settle but more
// frames at low step rates (0.11 = the envelope ratio = the unbounded
// old tail; never go there); higher = crisper stick.
const SPRING_SETTLE_TAPER_RATIO = 0.25;
// How long a structural-append mark (markStructuralAppend, the
// controller's markStructuralContentPending) keeps near-term content
// growth spring-eligible while animationMode is 'instant'.
const STRUCTURAL_APPEND_SPRING_WINDOW_MS = 250;

// Chase-telemetry frame-gap histogram bounds (ms). Chosen to separate
// healthy 165/144/120/60Hz rAF cadences from dropped-frame territory:
// <9 (≥120Hz), 9–13 (~90Hz), 13–18 (~60Hz), 18–26 (missed one 60Hz
// frame), 26–42 (~30Hz), >42 (multi-frame stall / long task). One
// `scroll.spring.chase` summary per chase reports the counts, which is
// what distinguishes "the animation genuinely dropped frames" from "the
// motion profile just looked steppy" in a capture — the sampled
// spring.tick trace records CANNOT answer that (see the telemetry
// footgun note in docs/architecture/settle-flicker-analysis.md).
const CHASE_GAP_BUCKET_BOUNDS_MS = [9, 13, 18, 26, 42] as const;

// Per-chase telemetry accumulator. Scalar counters only — no per-frame
// allocations or serialization; the single trace record is built at
// chase end. Created only when the UI render trace is enabled, so
// production builds never touch it.
interface ChaseTelemetry {
  startedAt: number;
  ticks: number;
  writeTicks: number;
  sentinelTicks: number;
  maxGapMs: number;
  gapBuckets: number[];
  catchupClamps: number;
  targetChanges: number;
  sentinelEntries: number;
  longTasks: number;
  longTaskMs: number;
}

function requestFrame(callback: FrameRequestCallback): number {
  return typeof requestAnimationFrame === 'function'
    ? requestAnimationFrame(callback)
    : window.setTimeout(() => callback(nowMs()), 0);
}

function cancelFrame(handle: number): void {
  if (typeof cancelAnimationFrame === 'function') {
    cancelAnimationFrame(handle);
  } else {
    window.clearTimeout(handle);
  }
}

// Arrival-readback acceptance bookkeeping, reached through deps as one
// unit. The underlying state (one nullable accepted target) is
// controller-owned because notifyLiveContentMaybeGrew shares it; see the
// cluster in scroll/index.svelte.ts.
export interface ArrivalReadback {
  /** Accepted readback matches `target` and scrollTop is still at it. */
  matches(target: number): boolean;
  /** Record acceptance after a write that landed in-band but off-target. */
  record(target: number): void;
  /** An exact write toward `target` is still warranted. */
  shouldWriteExact(target: number): boolean;
  /** Write exactly `target` through the chokepoint, then record. */
  writeExact(caller: ScrollWriteCaller, target: number): void;
  clear(): void;
  /** Drop an accepted readback whose target moved out of the arrival band. */
  invalidateStale(target: number): void;
}

// Everything the spring needs from the controller. Geometry and the
// scrollTop chokepoint come straight through; the arrival-readback
// group reaches the controller-owned acceptance state shared with
// notifyLiveContentMaybeGrew.
export interface SpringChaseDeps {
  /** Current scroll element; undefined between attach cycles. */
  getScrollEl(): HTMLElement | undefined;
  isPaused(): boolean;
  /** Intent flag: "we want to be glued to the bottom" (isAtBottomState). */
  isAtBottom(): boolean;
  isEscaped(): boolean;
  /**
   * True while a text selection crosses the scroll element — the spring
   * pauses (re-rAFs without writing) instead of fighting the user.
   */
  selectionActive(): boolean;
  targetScrollTop(): number;
  /** scrollTop is within the arrival band of `target` (geometry read). */
  scrollTopIsAtTarget(target: number): boolean;
  arrival: ArrivalReadback;
  writeScrollTop(caller: ScrollWriteCaller, value: number): void;
  /** Normalized per-fire animation mode (anything but 'spring' is 'instant'). */
  animationMode(): 'spring' | 'instant';
  prefersReducedMotion(): boolean;
  /**
   * Force the controller's sampled spring-tick trace to record the next
   * write, so the trace shows every chase boundary rather than every
   * ~12th sampled tick.
   */
  forceNextSpringTickTrace(): void;
}

export interface SpringChase {
  /** Start a chase if none is in flight and the gate is open. */
  start(): void;
  /** Stop the chase and reset all kinematic + sentinel state. */
  cancel(): void;
  /**
   * One-shot clamp recovery: settle the spring at a target it has
   * already reached (see the FOOTGUN comment on the implementation).
   */
  snapOscillationToBottom(caller: ScrollWriteCaller, top: number): void;
  /** Sampled impure wrapper over the resolver's pure spring-gate predicate. */
  gateOpen(): boolean;
  /** True while a chase or its sentinel is alive (`springActive` in resolver terms). */
  isActive(): boolean;
  /** Current spring run token for trace payloads; 0 = no spring in flight. */
  token(): number;
  /** User broke from auto-follow — the next tick bails and cancels. */
  requestStop(): void;
  clearStopRequest(): void;
  stopRequested(): boolean;
  /**
   * Record that the chase target moved. Keeps the retain window open
   * across streaming chunk boundaries so the spring doesn't
   * arrive-and-stop between chunks.
   */
  markTargetChanged(): void;
  /** Open the one-shot structural-append spring-eligibility window. */
  markStructuralAppend(): void;
  structuralAppendPending(): boolean;
  clearStructuralAppend(): void;
  /** Sentinel-entry target for the resolver snapshot; -1 = not in sentinel. */
  sentinelTarget(): number;
}

export function createSpringChase(deps: SpringChaseDeps): SpringChase {
  let velocity = 0;
  let accumulated = 0;
  let lastTickAt: number | null = null;
  // Monotonic counter (cheaper than `Symbol('spring')` per start). 0 means
  // no spring in flight; positive values identify the current spring run.
  let springToken = 0;
  let springGen = 0;
  let springFrameHandle: number | null = null;
  // Bumped on any target change while a spring may be chasing — positive
  // contentRO deltas, notifyLiveContentMaybeGrew nudges, and (when the
  // sync-pin write is suppressed by the spring carve-out) negative
  // contentRO deltas too. The retain check in the spring tick uses this
  // to keep chasing across chunk boundaries instead of arriving-then-
  // restarting (visibly jittery). "TargetChanged" rather than "Grew"
  // because the symmetric spring now follows shrinks as well as growths.
  let lastTargetChangedAt = 0;
  let springStopRequested = false;
  let structuralAppendSpringUntil = 0;
  let springStartedFromStructuralAppend = false;
  // The target when the sentinel first entered after a chase. When the
  // sentinel tick sees diff > 0 but target === sentinelEntryTarget, the
  // content oscillated and returned to the same height — snap instantly.
  // -1 means not in sentinel. Only set on the FIRST sentinel entry
  // after a chase (not on re-entry), so the value reflects the target
  // at the moment the spring settled. Cleared on cancel() and
  // when the spring exits sentinel with a different target.
  let sentinelEntryTarget = -1;
  // Target seen by the previous tick; -1 = none yet this chase.
  // Telemetry-only (drives the target-change counter); untouched when
  // tracing is disabled.
  let lastChaseTarget = -1;
  // Per-chase telemetry (null when tracing is disabled or no chase is in
  // flight) and the long-task observer that attributes main-thread
  // blockage to the chase window. Both live start()→cancel().
  let chaseTelemetry: ChaseTelemetry | null = null;
  let longTaskObserver: PerformanceObserver | null = null;

  function beginChaseTelemetry(): void {
    if (!isUiRenderTraceEnabled()) return;
    chaseTelemetry = {
      startedAt: nowMs(),
      ticks: 0,
      writeTicks: 0,
      sentinelTicks: 0,
      maxGapMs: 0,
      gapBuckets: new Array<number>(CHASE_GAP_BUCKET_BOUNDS_MS.length + 1).fill(0),
      catchupClamps: 0,
      targetChanges: 0,
      sentinelEntries: 0,
      longTasks: 0,
      longTaskMs: 0,
    };
    if (
      typeof PerformanceObserver !== 'undefined'
      && (PerformanceObserver.supportedEntryTypes ?? []).includes('longtask')
    ) {
      try {
        const observer = new PerformanceObserver((list) => {
          const stats = chaseTelemetry;
          if (!stats) return;
          for (const entry of list.getEntries()) {
            stats.longTasks += 1;
            stats.longTaskMs += entry.duration;
          }
        });
        observer.observe({ type: 'longtask' });
        longTaskObserver = observer;
      } catch {
        // Graceful degradation for engines that advertise 'longtask'
        // support but throw on observe (dev-only telemetry; the chase
        // summary still emits without long-task attribution).
        longTaskObserver = null;
      }
    }
  }

  function recordChaseFrame(now: number, previousTickAt: number | null, dtFrames: number): void {
    const stats = chaseTelemetry;
    if (!stats) return;
    stats.ticks += 1;
    if (dtFrames > SPRING_MAX_CATCHUP_STEPS) stats.catchupClamps += 1;
    if (previousTickAt === null) return;
    const gapMs = now - previousTickAt;
    if (gapMs > stats.maxGapMs) stats.maxGapMs = gapMs;
    let bucket: number = CHASE_GAP_BUCKET_BOUNDS_MS.length;
    for (let i = 0; i < CHASE_GAP_BUCKET_BOUNDS_MS.length; i++) {
      if (gapMs < CHASE_GAP_BUCKET_BOUNDS_MS[i]) {
        bucket = i;
        break;
      }
    }
    stats.gapBuckets[bucket] += 1;
  }

  function endChaseTelemetry(): void {
    const stats = chaseTelemetry;
    chaseTelemetry = null;
    if (longTaskObserver) {
      longTaskObserver.disconnect();
      longTaskObserver = null;
    }
    // Skip trivial chases (a start that bailed immediately) — they carry
    // no cadence information and would crowd the trace during churn.
    if (!stats || stats.ticks < 3 || !isUiRenderTraceEnabled()) return;
    trace('scroll.spring.chase', () => ({
      durationMs: Math.round(nowMs() - stats.startedAt),
      ticks: stats.ticks,
      writeTicks: stats.writeTicks,
      sentinelTicks: stats.sentinelTicks,
      maxGapMs: Math.round(stats.maxGapMs * 10) / 10,
      // Bucket bounds documented at CHASE_GAP_BUCKET_BOUNDS_MS:
      // [<9, 9–13, 13–18, 18–26, 26–42, >42] ms.
      gapBuckets: stats.gapBuckets,
      catchupClamps: stats.catchupClamps,
      targetChanges: stats.targetChanges,
      sentinelEntries: stats.sentinelEntries,
      longTasks: stats.longTasks,
      longTaskMs: Math.round(stats.longTaskMs),
    }));
  }

  function cancel(): void {
    if (springFrameHandle !== null) {
      cancelFrame(springFrameHandle);
      springFrameHandle = null;
    }
    springToken = 0;
    velocity = 0;
    accumulated = 0;
    lastTickAt = null;
    deps.arrival.clear();
    springStartedFromStructuralAppend = false;
    // Reset the target-change timestamp so a stale value can't trick a
    // fresh chase into thinking it's within the retain window right out
    // of the gate (matches the historical 80LoC-spring cleanup semantics).
    lastTargetChangedAt = 0;
    sentinelEntryTarget = -1;
    lastChaseTarget = -1;
    endChaseTelemetry();
  }

  // Settle the spring at a target it has already reached, shared by both
  // oscillation-recovery sites: the spring-tick path (the spring caught up
  // to a target that had returned to the sentinel-entry value) and the
  // controller's synchronous contentRO path (an above-viewport remeasure
  // regrow). The body is load-bearing and must stay identical across both
  // — zero velocity/accumulated so the arrival check stays settled, and
  // consume `sentinelEntryTarget` so the OTHER site's snap no-ops for this
  // same oscillation. `springToken` is intentionally left untouched: the
  // spring stays sentinel-alive so `springActive` keeps engaging the
  // resolver's decline tier and negative-delta carve-out. Shared so the
  // two sites can't drift — the same reason `gateOpen()` is shared.
  //
  // FOOTGUN — this is a one-shot CLAMP RECOVERY, not an oscillation source. If
  // you ever catch it firing every frame in a sustained ±N px limit cycle (text
  // visibly "vibrating"/flickering, idle or while streaming), the bug is NOT
  // here: some other code is driving a per-frame content-height oscillation that
  // keeps re-arming the snap. The classic cause is a forced synchronous layout
  // read (getBoundingClientRect / offsetHeight) in a ResizeObserver or Svelte
  // `use:` action hot path — the deleted timelineRowGeometry.ts `applyParams`
  // did exactly this once (git history, incident commit a5a5d032). Do NOT
  // "fix" the vibration by adding a
  // stop-after-N break here: this snap exists to rescue scrollTop from a browser
  // max-scroll clamp, and a break would instead STRAND it there — the
  // post-width-reflow floating-message bug it recovers from. Fix the driver.
  function snapOscillationToBottom(caller: ScrollWriteCaller, top: number): void {
    deps.writeScrollTop(caller, top);
    velocity = 0;
    accumulated = 0;
    sentinelEntryTarget = -1;
  }

  // Impure sampling wrapper over the shared pure predicate
  // (scroll/resolver.ts springGateIsOpen). Used by `start()`, the
  // delivery resolver (via its sampled observation), and the
  // controller's notifyLiveContentMaybeGrew so the sites can't drift on
  // which conditions allow the spring. The `warm` check is intentionally
  // omitted from the predicate — start() is called from inside
  // already-warm branches; warm-checking inside it would double-gate and
  // confuse the read.
  function gateOpen(): boolean {
    return springGateIsOpen({
      springStopRequested,
      paused: deps.isPaused(),
      isAtBottom: deps.isAtBottom(),
      escaped: deps.isEscaped(),
      prefersReducedMotion: deps.prefersReducedMotion(),
      animationMode: deps.animationMode(),
      structuralAppendPending: structuralAppendSpringUntil > nowMs(),
    });
  }

  function start(): void {
    if (springToken !== 0) return;
    if (!gateOpen()) return;
    springStartedFromStructuralAppend =
      deps.animationMode() !== 'spring'
      && structuralAppendSpringUntil > nowMs();
    const myToken = ++springGen;
    springToken = myToken;
    lastTickAt = null;
    deps.forceNextSpringTickTrace();
    beginChaseTelemetry();

    const tick = (now: number): void => {
      springFrameHandle = null;
      const el = deps.getScrollEl();
      if (springToken !== myToken || !el) return;
      // Bail conditions: lease acquired, escape set, or stop requested.
      // All three are handled by `cancel()` cleanup at exit.
      if (springStopRequested || deps.isPaused() || !deps.isAtBottom() || deps.isEscaped()) {
        cancel();
        return;
      }
      if (deps.selectionActive()) {
        // Selection drag should never fight the user — re-rAF without
        // advancing scrollTop so the spring effectively pauses.
        springFrameHandle = requestFrame(tick);
        return;
      }

      // Frame-rate independent spring integration. One full step matches
      // the tuned 60Hz recurrence; higher-refresh frames integrate a
      // fractional step and still write every rAF, so 120Hz displays do not
      // see every other frame held. Long gaps are capped to a bounded burst
      // so a blocked frame cannot pay the entire stall in one write.
      const previousTickAt = lastTickAt;
      const dtFrames = previousTickAt === null ? 1 : (now - previousTickAt) / SIXTY_FPS_INTERVAL_MS;
      lastTickAt = now;
      if (chaseTelemetry) recordChaseFrame(now, previousTickAt, dtFrames);
      const integrationFrames = Math.min(Math.max(dtFrames, 0), SPRING_MAX_CATCHUP_STEPS);

      // Cache per-tick. `targetScrollTop()` reads `scrollHeight` /
      // `clientHeight` — both force layout. Compute once per frame.
      const target = deps.targetScrollTop();
      const current = el.scrollTop;
      deps.arrival.invalidateStale(target);

      if (chaseTelemetry) {
        if (lastChaseTarget >= 0 && target !== lastChaseTarget) {
          chaseTelemetry.targetChanges += 1;
        }
        lastChaseTarget = target;
      }

      // Whether the consumer still wants spring follow, and whether a
      // target change landed recently enough that more content is probably
      // still arriving. Hoisted above the diff branch so the caught-up
      // branch can decide whether to KEEP momentum for the next growth or
      // shed it and settle; the arrival check below reuses both.
      const wantsStreamingSpringNow = deps.animationMode() === 'spring';
      const wantsSpringNow = wantsStreamingSpringNow || springStartedFromStructuralAppend;
      const withinTargetChangeRetainWindow =
        wantsSpringNow && now - lastTargetChangedAt < RETAIN_ANIMATION_DURATION_MS;

      if (current !== target && !deps.arrival.matches(target)) {
        // Content oscillation guard: if the sentinel was idle
        // (sentinelEntryTarget set) and the target returned to the
        // sentinel entry value, the content layer oscillated in
        // height (-N then +N from async Streamdown typesetting /
        // a windowing row remount). The browser auto-clamped scrollTop
        // during the low point (a native engine operation — not a
        // scrollTop write the controller could arbitrate), stranding
        // scrollTop below the restored target. Snap back instantly — a spring
        // chase for zero net content change is a visible artifact.
        //
        // This check is DELIBERATELY different from the resolver's
        // isSentinelOscillationStranded (scroll/resolver.ts): it
        // triggers on exact inequality filtered by arrival-readback
        // acceptance (the outer branch condition), not the 1px stranded
        // band — see the predicate's call-site map before unifying.
        if (sentinelEntryTarget >= 0 && withinArrivalBand(target, sentinelEntryTarget)) {
          snapOscillationToBottom('spring.oscillationSnap', target);
        } else {
          deps.arrival.clear();
          sentinelEntryTarget = -1;
          if (integrationFrames > 0) {
            let remainingFrames = integrationFrames;
            while (remainingFrames > 0) {
              const stepFraction = Math.min(1, remainingFrames);
              remainingFrames -= stepFraction;
              // Re-derive the remaining gap per step from the in-frame
              // position (`current + accumulated`) — pure arithmetic, no
              // extra layout reads — so a multi-step catch-up follows the
              // same curve N sequential 60Hz frames would have. Fractional
              // steps use proportional stiffness and exponential damping so
              // high-refresh frames advance smoothly without changing the
              // 60Hz shape.
              const stepDiff = target - (current + accumulated);
              velocity =
                (Math.pow(DEFAULT_SPRING.damping, stepFraction) * velocity
                  + DEFAULT_SPRING.stiffness * stepFraction * stepDiff)
                / DEFAULT_SPRING.mass;
              // Hard speed cap: large chases glide at a bounded constant
              // speed instead of a distance-proportional zoom (see
              // SPRING_MAX_VELOCITY_PX_PER_FRAME).
              if (velocity > SPRING_MAX_VELOCITY_PX_PER_FRAME) {
                velocity = SPRING_MAX_VELOCITY_PX_PER_FRAME;
              } else if (velocity < -SPRING_MAX_VELOCITY_PX_PER_FRAME) {
                velocity = -SPRING_MAX_VELOCITY_PX_PER_FRAME;
              }
              // Perceived-smoothness shaping, applied only to a velocity
              // already pointing at the target (reversals still turn on
              // the spring curve). Upper bound: the deceleration envelope
              // (speed ∝ remaining distance) caps small-quantum peaks and
              // shapes the ease-out. Lower bound: the glide floor keeps
              // stepping above the judder band, tapering off across the
              // last ~4px so the landing decelerates visibly instead of
              // stopping flat. See SPRING_DECEL_ENVELOPE_RATIO /
              // SPRING_MIN_VELOCITY_PX_PER_FRAME / SPRING_SETTLE_TAPER_RATIO.
              const remaining = Math.abs(stepDiff);
              const envelope = Math.min(
                SPRING_MAX_VELOCITY_PX_PER_FRAME,
                Math.max(
                  SPRING_MIN_VELOCITY_PX_PER_FRAME,
                  remaining * SPRING_DECEL_ENVELOPE_RATIO,
                ),
              );
              const minGlide = Math.min(
                SPRING_MIN_VELOCITY_PX_PER_FRAME,
                remaining * SPRING_SETTLE_TAPER_RATIO,
              );
              if (stepDiff > 0 && velocity > 0) {
                if (velocity > envelope) velocity = envelope;
                else if (velocity < minGlide) velocity = minGlide;
              } else if (stepDiff < 0 && velocity < 0) {
                if (velocity < -envelope) velocity = -envelope;
                else if (velocity > -minGlide) velocity = -minGlide;
              }
              accumulated += velocity * stepFraction;
            }
            const next = current + accumulated;
            // Pre-clamp in JS so we know the post-state without a second
            // layout read just to check whether the browser clamped. Cross-
            // target clamps in EITHER direction count as kinematic
            // overshoot: a positive-diff chase overshoots when
            // `next > target`, a negative-diff chase (the symmetric branch
            // that lets the spring follow shrinks) overshoots when
            // `next < target`. Both clamp to `target` and zero `accumulated`
            // below.
            const crossedTarget =
              (current < target && next > target)
              || (current > target && next < target);
            const clamped = crossedTarget ? target : next;
            deps.writeScrollTop(crossedTarget ? 'spring.overshoot' : 'spring.tick', clamped);
            if (chaseTelemetry) chaseTelemetry.writeTicks += 1;
            if (clamped === target) {
              deps.arrival.record(target);
            }
            if (el.scrollTop !== current) accumulated = 0;
          }
        }
      } else {
        // Caught up to the bottom. Exact equality is the normal path; the
        // accepted-readback path covers engines that already rejected an exact
        // target write but read back within the one-pixel arrival band.
        //
        // `accumulated` is always dropped — there is no useful sub-pixel
        // position carry to keep. Nothing is written in this branch, so a
        // retained velocity can't move the viewport on its own; it only seeds
        // the next diff > 0 tick, where the cross-target clamp still bounds
        // overshoot.
        //
        // KEEP upward follow velocity across the catch-up — clamped to
        // the carry ceiling — instead of zeroing it: that is what turns
        // a quantum-by-quantum stream into one glide rather than a
        // stop-start pulse per quantum. Clamping (not zeroing) an
        // above-ceiling remnant is equally snap-safe: the kept value is
        // by construction one the ceiling rule already licenses (see
        // SPRING_CARRY_VELOCITY_CEILING for the derivation).
        // Shed velocity entirely when:
        //   - outside the retain window → streaming paused; the arrival
        //     check below needs |velocity| < 0.5 to settle the spring (or
        //     hand it to the sentinel), else it ticks at 60fps forever;
        //   - downward (velocity <= 0) → carry is scoped to growth-follow;
        //     a shrink-follow remnant carried into a resumed growth would
        //     nudge the viewport the wrong way for a frame.
        accumulated = 0;
        if (withinTargetChangeRetainWindow && velocity > 0) {
          if (velocity > SPRING_CARRY_VELOCITY_CEILING) {
            velocity = SPRING_CARRY_VELOCITY_CEILING;
          }
        } else {
          velocity = 0;
        }
      }

      // Arrival check uses the cached `target` for the position
      // comparison; the time delta uses rAF's `now` (matches
      // `nowMs()` in test environments because `performance.now` is
      // mocked to read the same source rAF passes the callback).
      // Mode flip mid-flight (turn ended) or RETAIN_ANIMATION_DURATION_MS
      // elapsing without another target-change event makes
      // `withinTargetChangeRetainWindow` (computed above) false, so the
      // spring lands on its next arrival check rather than chasing forever.
      // Bidirectional — applies to downward chases (shrinks) as well as
      // upward (growth).
      const arrived =
        deps.scrollTopIsAtTarget(target)
        && Math.abs(velocity) < ARRIVAL_VELOCITY_THRESHOLD;
      if (arrived && !withinTargetChangeRetainWindow) {
        if (wantsStreamingSpringNow) {
          // Streaming active but no target change within the retain
          // window (async shiki load, inter-chunk gap, parseIncomplete
          // Markdown rebalance). Keep the spring sentinel-alive so
          // `springActive` stays true for the resolver decisions that
          // key on it: resolveEngineCompensation's decline tier and
          // resolveContentDelivery's negative-delta carve-out. Without
          // this, cancel() sets springToken=0 and the dead window
          // lets a routed engine compensation or a negative contentRO
          // sync-pin land instantly — visible as 1-2 lines of instant
          // jump mid-stream. The next positive contentRO delta bumps
          // lastTargetChangedAt and the chase resumes on the following
          // tick.
          //
          // Snap pixel-perfect on sentinel entry only when the browser readback
          // is outside the accepted arrival band. Some engines reject the exact
          // max scrollTop by one CSS pixel; repeatedly writing that rejected
          // target is pure jank and creates needless ResizeObserver pressure.
          // Zeroing velocity/accumulated keeps the arrival check stable across
          // sentinel ticks.
          if (deps.arrival.shouldWriteExact(target)) {
            deps.arrival.writeExact('spring.arrive', target);
          }
          velocity = 0;
          accumulated = 0;
          if (chaseTelemetry) chaseTelemetry.sentinelTicks += 1;
          if (sentinelEntryTarget < 0) {
            sentinelEntryTarget = target;
            if (chaseTelemetry) chaseTelemetry.sentinelEntries += 1;
          }
          springFrameHandle = requestFrame(tick);
          return;
        }
        // Snap to the exact target on arrival so the final paint lands
        // pixel-perfect rather than 0.5px above the bottom, unless the browser
        // already accepted a value inside the arrival band.
        if (deps.arrival.shouldWriteExact(target)) {
          deps.arrival.writeExact('spring.arrive', target);
        }
        cancel();
        return;
      }
      springFrameHandle = requestFrame(tick);
    };
    springFrameHandle = requestFrame(tick);
  }

  return {
    start,
    cancel,
    snapOscillationToBottom,
    gateOpen,
    isActive: () => springToken !== 0,
    token: () => springToken,
    requestStop: () => {
      springStopRequested = true;
    },
    clearStopRequest: () => {
      springStopRequested = false;
    },
    stopRequested: () => springStopRequested,
    markTargetChanged: () => {
      lastTargetChangedAt = nowMs();
    },
    markStructuralAppend: () => {
      structuralAppendSpringUntil = nowMs() + STRUCTURAL_APPEND_SPRING_WINDOW_MS;
    },
    structuralAppendPending: () => structuralAppendSpringUntil > nowMs(),
    clearStructuralAppend: () => {
      structuralAppendSpringUntil = 0;
      springStartedFromStructuralAppend = false;
    },
    sentinelTarget: () => sentinelEntryTarget,
  };
}
