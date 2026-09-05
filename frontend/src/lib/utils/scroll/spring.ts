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

import { ARRIVAL_DISTANCE_PX, springGateIsOpen, withinArrivalBand } from './resolver';
import { installDocumentResumeTracking, msSinceDocumentResume } from './documentResume';
import { nowMs } from './time';
import { trace } from './trace';
import { createRetargetAccelerationBridge } from './retarget';
import { isUiRenderTraceEnabled } from '../uiRenderTrace';
import {
  __resetAnimationFrameCoordinatorForTest,
  createAnimationFrameBatcher,
} from '../animationFrameBatcher';
import type { ScrollWriteCaller } from './types';
import type { ScrollGrid } from './grid';
import { createFrameCadence, quantizedFloorStep } from './cadence';

// ===== Spring chase tuning =====
// Tuned from upstream stackblitz-labs/use-stick-to-bottom defaults
// (damping 0.7, stiffness 0.05, mass 1.25). A 0.05 stiffness takes
// roughly half a second to settle a one-line scroll target change, which
// leaves WebKit spending too long in the low-velocity rounded tail during
// fast streaming. 0.08 keeps the no-visible-overshoot shape but catches
// line-sized target jumps quickly enough that consecutive wraps read as one
// continuous follow.
const DEFAULT_SPRING = { damping: 0.7, stiffness: 0.08, mass: 1.25 } as const;
// The derived pair the integrator actually uses (see the
// composability note at the integration site). Tune DEFAULT_SPRING,
// not these. RETENTION is the per-frame velocity carry-over
// (damping/mass = 0.56); the FOLLOW RATIO is the recurrence's fixed
// point per px of remaining distance — velocity converges toward
// 0.145·remaining, the quasi-steady follow speed the deceleration
// envelope is tuned just under.
const SPRING_VELOCITY_RETENTION = DEFAULT_SPRING.damping / DEFAULT_SPRING.mass;
const SPRING_QUASI_STEADY_FOLLOW_RATIO =
  DEFAULT_SPRING.stiffness / (DEFAULT_SPRING.mass - DEFAULT_SPRING.damping);
const SIXTY_FPS_INTERVAL_MS = 1000 / 60;
// Cap on how many fixed 60Hz steps one rAF tick may integrate. A
// stalled rAF would otherwise pay its entire gap at once — a many-step
// advance the cross-target clamp turns into a visible teleport.
//
// History: four steps (≈67ms of motion per real frame) with the
// original spring, then three for the faster streaming-follow spring to
// preserve the same bounded burst (04d393fb). Both were chosen to keep
// post-stall catch-up BRISK, tuned when a stall meant something rare —
// a tab returning from background, one pathological frame.
//
// That premise was wrong for WebKit. The 2026-08-05 boundary measurement
// measured rAF gaps of 25–129ms and 6–7 stall ticks per chase on Linux
// WebKit, against zero on Chromium: there, stalls are ROUTINE. At three
// steps the resume tick writes up to ~81px in one go (3 × the 27px
// velocity cap), and a single write that large IS the mechanism behind
// the residual Mac "fast jump" — several times per chase.
//
// One step. Multi-frame catch-up was never needed for TRACKING: the
// stall grows the distance to the target, and the spring's distance
// term accelerates into it on the following frames (still velocity-
// capped), so the lost integration time is recovered smoothly instead
// of instantly. The extremes stay bounded by the two mechanisms that
// always handled them — the arrival snap, and the >1-viewport
// catchupSnap for gaps a spring should not chase at all.
//
// Exported for the interleaving invariant suite
// (scrollInterleavings.test.ts), which holds every non-snap frame to
// the bounded step this and the velocity cap imply.
export const SPRING_MAX_CATCHUP_STEPS = 1;
// The stall-tick threshold for CHASE TELEMETRY, deliberately decoupled
// from the integration cap above. `catchupClamps` has meant "ticks that
// arrived 3+ frames late" across every trace we have, including the
// incident bookmarks; leaving it pinned to SPRING_MAX_CATCHUP_STEPS
// would have silently redefined the counter when that dropped to 1 and
// made post-change traces incomparable with the ones that motivated the
// change. The integration cap is now STRICTER than this threshold —
// that asymmetry is the point, and spring.test.ts pins both.
const SPRING_STALL_TICK_FRAMES = 3;
// Keep chasing for this long after the last positive grow event. Without
// this, the spring would consider itself "arrived" between streaming
// chunks and stop, then have to spin up again on the next chunk —
// visibly jittery at chunk boundaries. Once this window expires AND
// live content is still arriving, the spring enters sentinel mode
// (re-rAFs without writing, keeping springToken non-zero) so
// `springActive` stays true across gaps > 350ms (async shiki loads,
// parseIncompleteMarkdown rebalances) for the springActive-keyed
// resolver behavior — chiefly the negative-delta mid-chase carve-out,
// plus resolveContentDelivery's overshoot and idle-deadband clauses.
// The sentinel cancels on the next tick where liveContentActive() goes
// false (nothing advanced within the consumer's hold window — see
// utils/liveContentActivity.ts). Ending it early is cheap: the next
// growth simply starts a fresh chase, which is why a time window is an
// acceptable answer here and not for the physics gate.
// No ordering between that hold window and this constant is required
// for correctness: a compensation arriving after the sentinel died
// resolves through the pass/redirect tiers, both safe (the historical
// HOLD > RETAIN cross-file invariant died with the descriptor gate —
// see resolveEngineCompensation's provenance notes).
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
// The kept remnant additionally decays toward the slew ramp base per
// parked frame (see SPRING_ACCEL_SLEW_FACTOR_PER_FRAME): carry
// bridges brief inter-quantum gaps at speed without laundering a
// long visible stop into a fast restart.
//
// A quantum-EMA adaptive ceiling (floor 4, max 12, learned from growth
// quanta observed while parked) shipped briefly in 2026-07 and was
// removed: a real-session capture showed the sampler never fired —
// streaming quanta land mid-glide or across chase boundaries, never
// while parked within the same chase — so the machinery was dead weight
// and the fixed ceiling is all that ever ran.
const SPRING_CARRY_VELOCITY_CEILING = 4;
// Hard cap on chase speed, in px per 60Hz frame (27 ≈ 1620 px/s). Large
// distances — a command block appended mid-prose, a several-hundred-px
// correction routed through the spring — glide at a bounded constant
// speed instead of a distance-proportional zoom (~3000 px/s for a 400px
// jump), trading a few hundred ms of extra follow latency for motion
// that stays readable.
//
// INVARIANT: this cap must stay comfortably ABOVE the peak
// reveal-driven content growth rate (~345 px/s — catch-up under
// MAX_ADAPTIVE_CHARS_PER_SEC, markdown/smoothing/PerItemSmoother.ts,
// which is now the smoother's ONLY ceiling, with no carve-out: the
// successor-waiting fast-drain that used to burst past this cap was
// removed 2026-08-05, and the bounded-backlog skip that briefly replaced
// it was rejected in the same week — every character animates, in order,
// at no more than that rate. The smoother's `snap()` paths (low power,
// streaming-off, visibility resume) still deliver a backlog in one
// frame; those are one-off discontinuities, absorbed by the
// resumed-chase viewport clamp below, not a sustained growth rate.
// The follower being faster than the growth is what keeps steady-state
// follow lag-free at ANY wire speed — reveal rate-limits rendered
// height, not the wire. Raising reveal rates requires raising this in
// step. The acceleration ramp (SPRING_ACCEL_SLEW_FACTOR_PER_FRAME)
// makes a cold follower temporarily slower than peak growth; that
// transient is bounded and reclaimed precisely because this cap far
// exceeds growth and the envelope licenses 0.09× of any accrued lag.
export const SPRING_MAX_VELOCITY_PX_PER_FRAME = 27;
// Backlog threshold (in viewports) past which a RESUMED chase snaps to
// the target instead of animating at all. When a tick finds the target
// more than one viewport away AND one of the discontinuity signals below
// says rendering genuinely paused, the backlog accrued while nothing was
// being watched is placed instantly — the reader returns to a window
// that is simply already in the right place. This used to leave one
// viewport of glide as a "catch-up in flight" cue; that residual motion
// was rejected 2026-08-22 ("when it becomes visible it should already be
// in the right place, instantly") — the cue was read as the app being
// behind, not as intentional. Growth that arrives AFTER the snap is
// ordinary live follow and glides as always.
//
// Distance alone is deliberately NOT proof of a stall: a >viewport
// structural mount (a huge diff card, a fat command-output flush) is
// spring-routed by design during live follow (resolver.ts
// positiveWillPin) and must keep its full bounded glide — snapping it
// would be an unintentional mid-stream cut. So the snap additionally
// requires an OBSERVED discontinuity, either signal sufficing:
//   - this tick's real rAF gap ≥ STALL_RESUME_GAP_MS (occlusion or
//     minimize froze rAF mid-chase / mid-sentinel; the first resumed
//     tick carries the whole gap), or
//   - the document returned to visibility within
//     RESUME_CLAMP_WINDOW_MS (a chase starting FRESH right after
//     resume has no prior tick to carry the gap; visibilitychange →
//     visible also snaps the text smoothers to the wire in one frame,
//     which is exactly what creates the backlog).
// Failure is self-limiting in both directions: a missed signal
// degrades to the full-distance bounded glide; a spurious signal still
// needs a >viewport backlog to do anything, and the reader it snaps was
// not watching the span it skipped.
const SPRING_MAX_CHASE_DISTANCE_VIEWPORTS = 1;
const STALL_RESUME_GAP_MS = 1000;
const RESUME_CLAMP_WINDOW_MS = 2000;
// Deceleration envelope: max chase speed as a fraction of the REMAINING
// distance (per 60Hz frame), never squeezed below the _MIN below and
// capped at SPRING_MAX. This shapes the perceived ease-out — speed
// bleeds off in proportion to how close the glide is — and it caps the
// entry speed of a CARRIED start: a remnant (≤4) landing on a
// line-sized growth is shaped to 0.09·remaining on its first frame
// (2026-07-04 feedback: line-sized glides started too fast; catch-up
// latency explicitly matters less than perceived smoothness). A cold
// onset peaks lower still — at the crossover where this falling
// envelope meets the rising acceleration ramp (see
// SPRING_ACCEL_SLEW_FACTOR_PER_FRAME).
// Shipped at 0.11 — just under the spring's natural quasi-steady
// follow ratio (SPRING_QUASI_STEADY_FOLLOW_RATIO ≈ 0.145), so it
// bound gently rather than fighting the integrator — and softened to
// 0.09 on 2026-08-04 feel-tuning feedback: line quanta complete
// before the next line arrives, so slower cruise means glides overlap
// the next arrival instead of parking between lines. Large chases
// cruise at SPRING_MAX until the envelope takes over below ≈ 300px
// remaining, giving big glides a progressive slowdown instead of
// cruise-until-stop.
//
// The tail below the envelope is the spring's own decay, bounded below
// by the quantized-motion floor (see SPRING_QUANTIZED_MOTION_FLOOR_PX_PER_FRAME):
// every displayed frame of the tail moves the same whole number of
// device pixels, all the way to the landing.
const SPRING_DECEL_ENVELOPE_RATIO = 0.09;
// Lower cap on the envelope itself (an upper bound never squeezed below
// this), NOT a forced minimum speed: without it the envelope would
// strangle a tiny growth's natural motion (a 3px quantum would be
// capped at 0.33 px/frame and take ~300ms). The spring's own velocity
// below this value is untouched.
const SPRING_DECEL_ENVELOPE_MIN_PX_PER_FRAME = 1.6;
// Acceleration slew: ceiling on how fast target-pointing speed may
// BUILD, as a growth factor per 60Hz frame. The decel envelope above
// is a PERMIT that is largest exactly when a glide begins (remaining
// peaks at onset), so without a build limit every quantum starts at
// its peak speed and only decelerates — a hard onset per line wrap,
// and a glide that finishes early then parks dead until the next
// quantum (the stop-start sawtooth of line-after-line streaming;
// 2026-08-04 feedback). The slew turns onsets into a ramp: each step,
// speed toward the target may grow by at most this factor over
// max(ramp base, previous speed toward the target) — based at the
// ramp base (see SPRING_ACCEL_SLEW_BASE_PX_PER_FRAME below) so a
// standstill start never ramps through the sub-floor breathing band,
// and at current speed so a growth landing mid-motion continues from
// where it is. Speed each step is capped by min(slew, envelope, hard
// cap), all recomputed from live geometry (below every ceiling the
// spring's own decay governs), so accelerate→decelerate needs no mode
// state: the falling envelope undercuts the rising ramp wherever they
// cross — a few frames in for a line quantum, near the midpoint for a
// paragraph (a natural ease-in-out), after ~0.6s of spool-up to the
// hard cap for a multi-viewport backlog. That describes a FIXED target.
// If streaming extends the target during its falling side, the retarget
// jerk bridge below preserves acceleration continuity across the new
// fixed-target candidate.
//
// Geometric rather than additive because speed-change perception is
// relative (Weber's law): ×1.10/frame reads as the same "push" at
// 1.5 px/frame as at 20, which lets ONE constant give line quanta a
// soft ~100ms onset AND big backlogs a brisk floor→cap spool
// (ln 27 / ln 1.10 ≈ 35 frames). An additive rate tuned soft enough
// for lines made large catch-ups crawl (~1.7s to cap). Shipped at
// 1.12; softened to 1.10 on 2026-08-04 feel-tuning feedback.
//
// Deliberately NOT applied to deceleration or to a reversal's shed —
// the envelope must dump speed as fast as landing requires or glides
// would overshoot into the cross-target clamp, and the base counts
// only speed already pointing at the target, so a direction flip
// re-ramps from the base. The stall-resume catch-up jump is exempt
// by construction: its cap-speed velocity seed becomes the next
// step's base. While the spring idles caught-up between quanta, the
// carried remnant decays by this same factor per REAL elapsed frame
// toward the ramp base (see the caught-up branch): a couple-frame
// catch-up resumes essentially where it left off, a longer park
// converges to the standstill onset — licensed speed reflects how
// long the pane has actually been still.
const SPRING_ACCEL_SLEW_FACTOR_PER_FRAME = 1.1;
// Where a standstill ramp starts, in px per 60Hz frame. The ramp base
// is max(this, the motion floor's rate), so a standstill never ramps
// through the sub-floor band the floor exists to skip.
const SPRING_ACCEL_SLEW_BASE_PX_PER_FRAME = 1;

// ===== Whole-pixel motion =====
// Every engine snaps a scrollTop write to its pixel grid, and which
// grid is the engine's to choose: desktop Chromium rounds to whole CSS
// pixels at every DPR (scrollTopQuantization.browser.test.ts pins 1,
// 1.25, 1.5 and 2), the Android WebView to whole DEVICE pixels (the
// 2026-09-04 Pixel 9a measurement: 1/2.625 CSS px steps, readbacks at 1/32
// px precision). A model that integrates fractional displacement and
// lets the engine round it therefore paints an ALTERNATING step
// pattern whenever its per-frame rate is not a whole number of grid
// pixels — 1,1,1,2 at DPR 1.25, 1,2,1,2 at 2.625 × 120Hz — and the
// slow tail is exactly where that is visible. Owner ruling 2026-09-04:
// no jitter, and if constant motion cannot avoid it the glide stops
// rather than jitters. So the spring authors whole grid pixels. The
// lattice is measured by grid.ts on a private scroller, including browser
// zoom and floor-based writes. Each tick's displacement is snapped to a
// ladder of even cadences:
//
//   - `n` device pixels a tick from one up to SPRING_STEP_DIFFUSION_MIN_
//     DEVICE_PX, and below one, one device pixel every `k` ticks — the
//     nearest rung either way, held through a small hysteresis
//     (SPRING_STEP_HYSTERESIS_DEVICE_PX) around the previous rung, so a
//     monotone deceleration steps down 3,2,1 once and never wobbles
//     2,1,2,1 at a rounding boundary, and a rate under a pixel a tick
//     shows as a steady cadence rather than the irregular 1,0,1,1,0 mix
//     error diffusion makes of it. The sub-pixel residue is dropped;
//     the spring's own feedback absorbs the rate it quantizes away.
//   - At cruise, where a ±1 on a step of eight or more is invisible,
//     the residue is CARRIED (error diffusion) so the average rate is
//     exact at every refresh rate.
//   - The motion floor is a rung of that ladder (quantizedFloorStep):
//     `n` device pixels every displayed frame, or one every `k`, the
//     regular cadence closest in ratio to this reference rate. In the
//     floor regime the tick advances exactly that, by frame count, so
//     rAF timestamp jitter can never slip an extra pixel in.
//   - The floor holds until the cross-target clamp lands the glide,
//     through a landing cradle that is itself even: the last
//     SPRING_LANDING_CRADLE_EVENTS pixel events each take one floor
//     interval longer than the one before (k, 2k, 3k ticks). That is
//     the ritardando the 2026-07-04 feedback asked for (a flat stop
//     read as too firm), on the grid, where the sub-pixel decay that
//     used to make it could only paint an irregular one.
//
// This is the reference rate the rung is derived from: one CSS pixel per
// 60Hz-equivalent frame, 60px/s. The rung lands within a factor of
// ~1.35 of it: exactly 60px/s at DPR 1 and 2 on 60Hz and at DPR 1 on
// 120/240Hz; 55px/s (one per three frames) on a 165Hz DPR-1 panel;
// ~46px/s (one device pixel per frame) on a 2.625 × 120Hz phone;
// 72px/s (one per two frames) on a 144Hz DPR-1 panel.
// How many pixel events the landing cradle spans (see the floor bullet
// above); 0 is a flat stop.
const SPRING_LANDING_CRADLE_EVENTS = 3;
// Hysteresis around the previous ladder rung, in device pixels a tick
// above one and in ticks a pixel below. Timestamp jitter perturbs a
// tick's displacement by ~2%, so 0.15 holds every rung below ~7 device
// pixels per frame steady; above that a ±1 wobble is under 15% of the
// step and invisible.
const SPRING_STEP_HYSTERESIS_DEVICE_PX = 0.15;
// From this many device pixels per tick the write goes back to error
// diffusion: a ±1 alternation on a step of eight is under an eighth of
// the step, below what motion at that speed resolves, and the exact
// average rate it buys is what keeps cruise speed the same at every
// refresh rate (rounding 13.5 per 120Hz tick to 13 is a 4% cruise).
const SPRING_STEP_DIFFUSION_MIN_DEVICE_PX = 8;
// The floor hold releases once the velocity exceeds the rung's rate by
// this factor: above the ~0.2% the recurrence hovers at near the floor,
// below one 240Hz tick of the slew ramp (1.1^0.25 ≈ 1.024), so a real
// extension releases within one or two ticks at any refresh rate.
const SPRING_FLOOR_RELEASE_FACTOR = 1.02;

// ===== Write-refusal guard =====
// bug-report-20260818T003129Z: an ActivityRun clip spent 227 seconds
// refusing every scrollTop write — real scrollHeight/clientHeight, all
// writes read back 0 — while its content streamed 410→1228px. The only
// browser state that reproduces that pattern is the element not being
// a scroll container (overflow computed to clip/visible); nothing in
// this codebase sets that, so the trigger sits below our floor
// (renderer state, likely WebView2-specific). Two defects were ours
// regardless, and this guard closes both:
//   - The spring's simulated position (`accumulated`) coasted to the
//     target while the element stayed put, so every subsequent tick
//     wrote the FULL target ('spring.overshoot') — 37,565 failed writes
//     at display rate, and the first accepted write after the element
//     healed was a 940px instant teleport.
//   - All of it was silent: no diagnostic, and the 165Hz trace burst
//     evicted its own onset evidence from the bookmark ring.
// The guard uses synchronous write/readback evidence: motion heals a latch;
// a refused whole measured grid step outside the arrival band counts toward
// backoff. A sub-grid exact landing is inconclusive. An absolute 1.5px
// threshold missed every refused one-pixel tail write at high refresh rates.
//
// After SPRING_WRITE_REFUSAL_LATCH_TICKS consecutive refusals the
// guard latches. A latched chase keeps its rAF loop but gates the
// WHOLE tick body — geometry reads included — behind the retry
// interval: skipped ticks pay only the bail checks and a parked-style
// velocity decay toward the slew ramp base (the incident's real cost
// was 227s × 165Hz of forced layout, not just the writes; and the
// decay means a heal after a long wedge ramps up like every other
// onset instead of launching at cruise — the slew doctrine). One tick
// per SPRING_WRITE_REFUSAL_RETRY_INTERVAL_MS runs in full — the retry
// — and consumes the retry slot at the gate regardless of how its
// write classifies, so even an inconclusive retry backs off. Arrival /
// caught-up transitions and flag flips are honored within one retry
// interval while latched; nothing visible can move on a wedged
// element in that window, so the deferral costs nothing.
// `springActive` deliberately stays true for the whole latch: the
// resolver keeps yielding deliveries to the chase, which is exactly
// what lets the heal arrive as the chase's own bounded glide.
//
// Transitions are reported through deps.reportWriteRefusal — 'latched'
// (the controller attaches DOM diagnostics: computed overflow and
// scroll-behavior, connectedness; if the wedge ever recurs, that
// record IS the root-cause capture), 'healed' (the bookend), and
// 'abandoned' (cancel() ended the chase while still latched — a wedge
// that never heals must not end silently). The latch survives a
// caught-up excursion (target collapsing onto the wedged position)
// uncleared: a stale latch costs nothing — the first chase tick after
// it retries immediately (the retry stamp is long stale) and a MOVED
// retry unlatches on the spot — while clearing it would re-earn five
// refusals per excursion. cancel() resets the guard with the rest of
// the kinematic state: a fresh chase starts with fresh evidence.
//
// Scope: the guard covers SPRING writes only — deliberately. One-shot
// placement writers (requestBottom, engine compensation, forceStick,
// the virtualizer target) share the chokepoint and fail silently
// during a wedge, but each fails ONCE and bounded, and any sustained
// wedge during bottom-follow reaches the spring — the only writer
// that can busy-loop or teleport.
export const SPRING_WRITE_REFUSAL_LATCH_TICKS = 5;
export const SPRING_WRITE_REFUSAL_RETRY_INTERVAL_MS = 250;

// How long a structural-append mark (markStructuralAppend, the
// controller's markStructuralContentPending) counts as evidence that
// more content is imminent — liveness only, alongside
// liveContentActive(); the physics gate does not consult either.
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
  /** Integrating ticks that wrote nothing: displacement under a device
   * pixel, or a cradle tick between stretched events. */
  zeroStepTicks: number;
  sentinelTicks: number;
  maxGapMs: number;
  gapBuckets: number[];
  /**
   * Ticks that arrived more than SPRING_STALL_TICK_FRAMES (3) frames
   * late. A stall counter, NOT a count of integration clamps — every
   * tick past one frame is clamped since SPRING_MAX_CATCHUP_STEPS
   * dropped to 1. The threshold is held at 3 so the series stays
   * comparable with the traces that predate that change.
   */
  catchupClamps: number;
  distanceJumps: number;
  targetChanges: number;
  sentinelEntries: number;
  longTasks: number;
  longTaskMs: number;
  /**
   * Writes the element refused outright (see the write-refusal guard).
   * A subset of writeTicks: every refused write was still an attempt.
   */
  refusedWrites: number;
  /**
   * Frames the chase re-armed without moving because a selection drag
   * crossed the element. Not part of `ticks` (nothing was integrated),
   * so a chase that is all pause still shows WHY it never wrote.
   */
  selectionPausedTicks: number;
}

// Data for deps.reportWriteRefusal — the write-refusal guard's
// episodic transitions. Kinematic facts only; the controller owns the
// DOM diagnostics (computed overflow, connectedness, surface id) because
// they are element concerns, not spring concerns.
export interface SpringWriteRefusalEvent {
  /**
   * 'latched' — refusals crossed the latch threshold; 'healed' — a
   * latched element accepted (MOVED on) a write, the latch's bookend;
   * 'abandoned' — cancel() ended the chase while still latched, so no
   * heal was ever observed (a wedge that outlives its chase must not
   * end silently).
   */
  phase: 'latched' | 'healed' | 'abandoned';
  /** Consecutive refused writes at this transition (the wedge total on 'healed'/'abandoned'). */
  consecutiveRefusals: number;
  /** The write that triggered the transition; -1 on 'abandoned' (no write involved). */
  requested: number;
  /** The element's actual post-write scrollTop (-1 on 'abandoned' with no element). */
  scrollTop: number;
  /** The chase target at the transition (-1 on 'abandoned' with no element). */
  target: number;
  /** 0 on 'latched'; ms since the latch on 'healed'/'abandoned'. */
  wedgeMs: number;
}

let springFrameBatcher = createAnimationFrameBatcher(
  'scroll-spring',
  'before-dom-update',
);

/** Test seam: spring tests replace the global rAF queue between cases. */
export function __resetSpringFrameBatcherForTest(): void {
  __resetAnimationFrameCoordinatorForTest();
  springFrameBatcher = createAnimationFrameBatcher(
    'scroll-spring',
    'before-dom-update',
  );
}

function requestFrame(callback: FrameRequestCallback): number {
  return typeof requestAnimationFrame === 'function' && typeof cancelAnimationFrame === 'function'
    ? springFrameBatcher.request(callback)
    : window.setTimeout(() => callback(nowMs()), 0);
}

function cancelFrame(handle: number): void {
  if (typeof requestAnimationFrame === 'function' && typeof cancelAnimationFrame === 'function') {
    springFrameBatcher.cancel(handle);
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
  /** Current bottom target. `forceLayoutRead` flushes layout for clamp evidence. */
  targetScrollTop(forceLayoutRead?: boolean): number;
  /** Current position. External-geometry surfaces may serve their synchronized readback. */
  currentScrollTop(forceLayoutRead?: boolean): number;
  /** scrollTop is within the arrival band of `target` (geometry read). */
  scrollTopIsAtTarget(target: number): boolean;
  arrival: ArrivalReadback;
  writeScrollTop(caller: ScrollWriteCaller, value: number, bottomTarget: number): number | undefined;
  /**
   * Is live timeline content still arriving? LIVENESS ONLY — it decides
   * whether an arrived chase stays sentinel-alive across inter-chunk
   * gaps, never whether a movement animates (see resolver.ts
   * springGateIsOpen and utils/liveContentActivity.ts).
   */
  liveContentActive(): boolean;
  /** OS prefers-reduced-motion OR the app's low-power setting (the
   * controller's combined motionReduced() gate). */
  prefersReducedMotion(): boolean;
  /** Measured engine write lattice, refreshed when the display scale changes. */
  scrollGrid(): ScrollGrid;
  /**
   * Force the controller's sampled spring-tick trace to record the next
   * write, so the trace shows every chase boundary rather than every
   * ~12th sampled tick.
   */
  forceNextSpringTickTrace(): void;
  /**
   * The live scrollTop is NOT explained by the controller's provenance
   * ledger — it differs (beyond the arrival band) from the last
   * authored write's browser-rounded readback / last classified user
   * scroll. Every authored write goes through the chokepoint and every
   * user gesture is classified by the intent machine, so during a
   * sentinel idle the only unexplained mover left is the browser's
   * max-scroll clamp. This is the clamp EVIDENCE the oscillation
   * guards require: a baseline match alone is just a numeric shape,
   * and an authored displacement (a head-splice compensation) produces
   * the same one (bug-report-20260801T213259Z).
   */
  scrollTopUnexplained(): boolean;
  /**
   * Write-refusal guard promotion (see the guard's constant block).
   * Called exactly once per latch and once per heal — episodic by
   * construction, never per-tick. The controller attaches DOM
   * diagnostics and files the frontend-errors diagnostic; a silent
   * implementation would recreate the incident this guard exists to
   * make loud, so the dep is required, not optional.
   */
  reportWriteRefusal(event: SpringWriteRefusalEvent): void;
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
  /** Current spring run token for trace data; 0 = no spring in flight. */
  token(): number;
  /**
   * The model's velocity after the last tick, in CSS px per 60Hz frame.
   * TEST ONLY: the kinematic suites pin the ramp, the envelope and the
   * retarget bridge on the model, since the written motion is whole
   * device pixels and cannot show a 10% per-frame ratio.
   */
  velocityForTest(): number;
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
  /**
   * Sentinel-entry target for the resolver snapshot, rebased by any
   * clientHeight drift since entry so the stranded-oscillation check
   * compares content heights; -1 = not in sentinel.
   */
  sentinelTarget(): number;
  /**
   * Clamp evidence for the resolver snapshot: unexplained scrollTop
   * movement has been WITNESSED since the current sentinel entry (see
   * deps.scrollTopUnexplained). Latched per sentinel session — every
   * sentinel tick checks, and this accessor also checks lazily so a
   * delivery arriving before the next tick still sees a clamp that
   * just happened. False whenever no sentinel is armed. Both
   * snap-recovery sites require it: a baseline match without witnessed
   * movement is an authored displacement (a head-splice compensation's
   * anchor hold, whose hidden growth is owed a glide), not a browser
   * clamp (bug-report-20260801T213259Z: think → bash inside a run
   * clip snapped the new row in).
   */
  sentinelClampWitnessed(): boolean;
  /**
   * The write-refusal latch is set (see the guard's constant block).
   * Observability only — the dev hook and tests read it; nothing
   * outside the spring makes decisions on it.
   */
  refusalLatched(): boolean;
}

export function createSpringChase(deps: SpringChaseDeps): SpringChase {
  // Idempotent: the chase-distance clamp's visibility-resume signal
  // needs one document-level listener per page, not per controller.
  installDocumentResumeTracking();
  let velocity = 0;
  const retarget = createRetargetAccelerationBridge();
  let accumulated = 0;
  let lastTickAt: number | null = null;
  // One `scroll.spring.selectionPause` record per pause session: set on
  // the first paused tick, cleared by the next tick that integrates or
  // by cancel().
  let selectionPauseTraced = false;
  // True once the current quantum's glide has exceeded the quantized
  // motion floor. Reset at every catch-up and on
  // cancel, so each growth's entry ramp stays natural.
  let slewRampBase = SPRING_ACCEL_SLEW_BASE_PX_PER_FRAME;
  let gridQuantum = 0;
  let gridOffset = 0;
  let quantizedFloorEngaged = false;
  // The floor hold, latched from the first sub-floor step of an engaged
  // glide until the landing, a reversal, or a target extension that
  // drives the velocity above the release band (see the tick).
  let floorHolding = false;
  // The write ladder's state (see the whole-device-pixel block): the
  // rung the previous tick wrote — `n` device pixels per tick as +n,
  // one pixel every `k` ticks as −k, 0 for none — which is the
  // hysteresis anchor; the direction it was written in (a reversal
  // starts the ladder afresh); and the ticks counted toward a cadence
  // rung's next pixel.
  let lastRung = 0;
  let rungDirection = 0;
  let cadenceTicks = 0;
  // Ticks since the last pixel event while the landing cradle is
  // stretching the floor's cadence (see the floor regime in the tick).
  let cradleTicks = 0;
  // Measured rAF cadence for the motion-floor derivation. Deliberately
  // NOT reset in cancel() — a new chase still uses the same display.
  const sampleFrameCadence = createFrameCadence();
  let frameIntervalEmaMs: number | null = null;
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
  //
  // The oscillation being guarded is a CONTENT-height dip-and-restore,
  // but `target` is scrollHeight − clientHeight — so a raw comparison
  // conflates the two components. If clientHeight moves while the
  // sentinel idles (the composer/ActivityRail resizing at a tool
  // boundary), the browser clamps scrollTop with the dropped target,
  // and the next APPENDED row of about the rail-row's height brings the
  // target back to the entry value: a composition change misread as a
  // restore, snapped instead of glided (the 2026-07-29 tool-boundary
  // jump; regression: appendAfterQuiet.browser.test.ts test C). Both
  // compare sites therefore rebase the entry by the clientHeight drift
  // (`rebasedSentinelEntryTarget`), making the comparison purely
  // content-height: entry scrollHeight == current scrollHeight ⇔
  // current target == entryTarget + (entryClientHeight − clientHeight).
  let sentinelEntryTarget = -1;
  let sentinelEntryClientHeight = 0;
  // Clamp evidence, latched per sentinel session (cleared on entry,
  // cancel, exit, and snap consumption): unexplained scrollTop movement
  // — beyond what the controller's provenance ledger can account for —
  // was witnessed while this sentinel idled. The only unexplained mover
  // during a sentinel is the browser's max-scroll clamp, so this is
  // what turns the baseline heuristic ("target returned to the entry
  // value") into evidence ("...AND something no write explains moved
  // scrollTop"). Latched rather than point-in-time so an authored
  // write landing between the clamp and the restore (a remeasure-above
  // compensation ratifying the clamped position) cannot launder a
  // strand the guard still needs to rescue.
  let sentinelClampWitnessed = false;
  // ===== Write-refusal guard state (see the guard's constant block) =====
  // Consecutive refused writes; a MOVED write resets it (inconclusive
  // sub-threshold writes touch nothing). Latch + retry timestamps are
  // on the rAF clock (`now`).
  let consecutiveRefusedWrites = 0;
  let writeRefusalLatched = false;
  let refusalLatchedAt = 0;
  let lastRefusalRetryAt = 0;

  function witnessClampIfUnexplained(): boolean {
    if (sentinelEntryTarget < 0) return false;
    if (!sentinelClampWitnessed && deps.scrollTopUnexplained()) {
      sentinelClampWitnessed = true;
    }
    return sentinelClampWitnessed;
  }

  function rebasedSentinelEntryTarget(clientHeightNow: number): number {
    if (sentinelEntryTarget < 0) return -1;
    return sentinelEntryTarget + (sentinelEntryClientHeight - clientHeightNow);
  }
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
      zeroStepTicks: 0,
      sentinelTicks: 0,
      maxGapMs: 0,
      gapBuckets: new Array<number>(CHASE_GAP_BUCKET_BOUNDS_MS.length + 1).fill(0),
      catchupClamps: 0,
      distanceJumps: 0,
      targetChanges: 0,
      sentinelEntries: 0,
      longTasks: 0,
      longTaskMs: 0,
      refusedWrites: 0,
      selectionPausedTicks: 0,
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
    if (dtFrames > SPRING_STALL_TICK_FRAMES) stats.catchupClamps += 1;
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
    if (!stats || !isUiRenderTraceEnabled()) return;
    if (stats.ticks < 3 && stats.selectionPausedTicks === 0) return;
    trace('scroll.spring.chase', () => ({
      durationMs: Math.round(nowMs() - stats.startedAt),
      ticks: stats.ticks,
      writeTicks: stats.writeTicks,
      zeroStepTicks: stats.zeroStepTicks,
      sentinelTicks: stats.sentinelTicks,
      maxGapMs: Math.round(stats.maxGapMs * 10) / 10,
      // Bucket bounds documented at CHASE_GAP_BUCKET_BOUNDS_MS:
      // [<9, 9–13, 13–18, 18–26, 26–42, >42] ms.
      gapBuckets: stats.gapBuckets,
      // The rAF cadence the spring is actually being driven at (the
      // fusion-floor EMA). On mixed-refresh multi-monitor setups
      // Chromium can pace rAF to the WRONG monitor's clock — compare
      // this against the refresh rate of the monitor the window is on:
      // a 6.06ms EMA while sitting on a 144Hz (6.94ms) panel means
      // ~21 frames/s are being dropped at presentation, visible as
      // pixel-skips at any glide speed.
      cadenceEmaMs:
        frameIntervalEmaMs === null ? null : Math.round(frameIntervalEmaMs * 100) / 100,
      catchupClamps: stats.catchupClamps,
      distanceJumps: stats.distanceJumps,
      targetChanges: stats.targetChanges,
      sentinelEntries: stats.sentinelEntries,
      longTasks: stats.longTasks,
      longTaskMs: Math.round(stats.longTaskMs),
      refusedWrites: stats.refusedWrites,
      selectionPausedTicks: stats.selectionPausedTicks,
    }));
  }

  function cancel(): void {
    if (springFrameHandle !== null) {
      cancelFrame(springFrameHandle);
      springFrameHandle = null;
    }
    springToken = 0;
    velocity = 0;
    retarget.reset();
    accumulated = 0;
    quantizedFloorEngaged = false;
    floorHolding = false;
    lastRung = 0;
    rungDirection = 0;
    cadenceTicks = 0;
    cradleTicks = 0;
    lastTickAt = null;
    selectionPauseTraced = false;
    deps.arrival.clear();
    springStartedFromStructuralAppend = false;
    // Reset the target-change timestamp so a stale value can't trick a
    // fresh chase into thinking it's within the retain window right out
    // of the gate (matches the historical 80LoC-spring cleanup semantics).
    lastTargetChangedAt = 0;
    sentinelEntryTarget = -1;
    sentinelClampWitnessed = false;
    // A chase ending while still latched never observed the element
    // accept anything — report 'abandoned' (not 'healed': there was no
    // heal) so a wedge that outlives its chase doesn't end silently.
    // Emitted before the guard reset below so the event carries the
    // wedge's true duration and count.
    if (writeRefusalLatched) {
      const el = deps.getScrollEl();
      deps.reportWriteRefusal({
        phase: 'abandoned',
        consecutiveRefusals: consecutiveRefusedWrites,
        requested: -1,
        scrollTop: el ? el.scrollTop : -1,
        target: el ? deps.targetScrollTop() : -1,
        wedgeMs: Math.round(nowMs() - refusalLatchedAt),
      });
    }
    // Guard state resets with the rest of the kinematics — a fresh
    // chase starts with fresh refusal evidence.
    consecutiveRefusedWrites = 0;
    writeRefusalLatched = false;
    refusalLatchedAt = 0;
    lastRefusalRetryAt = 0;
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
  // resolver's negative-delta carve-out. Shared so the
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
    deps.writeScrollTop(caller, top, top);
    velocity = 0;
    retarget.reset();
    accumulated = 0;
    sentinelEntryTarget = -1;
    sentinelClampWitnessed = false;
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
    });
  }

  function start(): void {
    if (springToken !== 0) return;
    if (!gateOpen()) return;
    springStartedFromStructuralAppend =
      !deps.liveContentActive()
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
        // Frames still arrive during selection. Keep the clock current so
        // release cannot integrate the pause or misclassify it as a stall.
        lastTickAt = now;
        // Selection drag should never fight the user. Re-rAF without
        // advancing scrollTop; `accumulated` stays intact so the resumed
        // chase remains continuous. Counted and traced on entry: this is
        // the one branch that keeps `springActive` true while writing
        // nothing, and a pause that outlives the gesture (the 2026-09-03
        // stuck-latch incident) is otherwise invisible in the trace.
        if (chaseTelemetry) chaseTelemetry.selectionPausedTicks += 1;
        if (!selectionPauseTraced) {
          selectionPauseTraced = true;
          if (isUiRenderTraceEnabled()) trace('scroll.spring.selectionPause', () => ({
            scrollTop: Math.round(el.scrollTop),
            target: Math.round(deps.targetScrollTop()),
          }));
        }
        springFrameHandle = requestFrame(tick);
        return;
      }
      selectionPauseTraced = false;

      // Frame-rate independent spring integration. One full step matches
      // the tuned 60Hz recurrence; higher-refresh frames integrate a
      // fractional step and still write every rAF, so 120Hz displays do not
      // see every other frame held. Long gaps are capped to a bounded burst
      // so a blocked frame cannot pay the entire stall in one write.
      const previousTickAt = lastTickAt;
      const rawDtFrames =
        previousTickAt === null ? 1 : (now - previousTickAt) / SIXTY_FPS_INTERVAL_MS;
      // Clamped elapsed time in 60Hz frames: a negative gap (clock
      // skew) degrades to zero motion, a non-finite timestamp to one
      // frame — a NaN here would otherwise corrupt velocity through the
      // parked decay and ride through every sign-conditioned clamp
      // into writeScrollTop.
      const dtFrames = Number.isFinite(rawDtFrames) ? Math.max(rawDtFrames, 0) : 1;
      lastTickAt = now;
      if (previousTickAt !== null) frameIntervalEmaMs = sampleFrameCadence(now - previousTickAt);
      if (chaseTelemetry) recordChaseFrame(now, previousTickAt, dtFrames);
      const integrationFrames = Math.min(dtFrames, SPRING_MAX_CATCHUP_STEPS);

      // Wedged-element backoff: while the write-refusal latch is set,
      // the WHOLE tick body below — including the geometry reads, each
      // a forced layout — runs only on retry-cadence ticks. The
      // incident's real cost was 227s of 165Hz forced layout, not just
      // the writes, so a skipped tick pays only the bail checks above
      // plus a parked-style velocity decay toward the slew ramp base
      // (mirrors the caught-up branch's parked decay; a heal after a
      // long wedge then ramps up like any other onset instead of
      // launching at carried cruise). Arrival / caught-up transitions
      // are deferred at most one retry interval — nothing visible can
      // move on a wedged element in that window. The retry slot is
      // consumed HERE, unconditionally: even a retry whose write
      // classifies inconclusive (sub-threshold) backs off, so the
      // latch can never re-enter a display-rate loop.
      if (writeRefusalLatched) {
        if (now - lastRefusalRetryAt < SPRING_WRITE_REFUSAL_RETRY_INTERVAL_MS) {
          const speed = Math.abs(velocity);
          if (speed > slewRampBase) {
            velocity =
              Math.sign(velocity)
              * Math.max(
                slewRampBase,
                speed / Math.pow(SPRING_ACCEL_SLEW_FACTOR_PER_FRAME, dtFrames),
              );
          }
          retarget.breakMotion();
          springFrameHandle = requestFrame(tick);
          return;
        }
        lastRefusalRetryAt = now;
      }

      const grid = deps.scrollGrid();
      if (grid.quantum !== gridQuantum || grid.writeOffset !== gridOffset) {
        if (!(grid.quantum > 0) || !Number.isFinite(grid.quantum) || !Number.isFinite(grid.writeOffset)) {
          throw new Error('Spring requires a finite positive measured scroll grid');
        }
        gridQuantum = grid.quantum;
        gridOffset = grid.writeOffset;
        accumulated = 0;
        lastRung = 0;
        cadenceTicks = 0;
        cradleTicks = 0;
        quantizedFloorEngaged = false;
        floorHolding = false;
      }
      const dpr = 1 / gridQuantum;
      const frameFraction =
        (frameIntervalEmaMs ?? SIXTY_FPS_INTERVAL_MS) / SIXTY_FPS_INTERVAL_MS;
      const floorStep = quantizedFloorStep(dpr, frameFraction);
      const floorEventPixels = Math.max(1, floorStep);
      const floorTickCss = floorStep / dpr;
      const floorRate = floorTickCss / frameFraction;
      slewRampBase = Math.max(SPRING_ACCEL_SLEW_BASE_PX_PER_FRAME, floorRate);
      // This tick's integrated displacement in CSS px, kept apart from
      // the carried residue (`accumulated`) so the write can band on it.
      let tickDisplacement = 0;
      let cradleTickCounted = false;

      // External-geometry surfaces publish their exact bottom target with
      // every virtualizer sample, so ordinary chase frames read that cache.
      // A sentinel needs a real layout flush before comparing the provenance
      // ledger for browser-clamp evidence. A write-refusal retry also takes
      // the real path because it is diagnosing an element outside its normal
      // scrolling state. (`current` is re-read after a catch-up jump below.)
      const forceGeometryRead = sentinelEntryTarget >= 0 || writeRefusalLatched;
      const target = deps.targetScrollTop(forceGeometryRead);
      let current = deps.currentScrollTop(forceGeometryRead);
      deps.arrival.invalidateStale(target);
      // Eager clamp-evidence check while the sentinel baseline is armed
      // — every sentinel tick, both branches, so a clamp during a dip
      // (where scrollTop lands exactly on the dipped target and the
      // caught-up branch runs) is witnessed before the restore brings
      // the target back and the guard below asks for the evidence. The
      // targetScrollTop() read above flushed layout, so a clamp applied
      // by this frame's layout is visible to the ledger comparison.
      witnessClampIfUnexplained();

      // Reduced motion (OS preference or the app's low-power setting)
      // flipped on mid-chase: land exactly and stop. The gate is
      // otherwise only consulted when (re)starting a chase (start(),
      // the delivery resolver, notifyLiveContentMaybeGrew), never
      // inside the running tick loop — so without this an in-flight
      // chase, kept alive by the retain window across streaming
      // quanta, would keep gliding indefinitely after the toggle.
      // Subsequent placements take the resolver's instant paths.
      if (deps.prefersReducedMotion()) {
        if (deps.arrival.shouldWriteExact(target)) {
          deps.arrival.writeExact('spring.arrive', target);
        }
        cancel();
        return;
      }

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
      const wantsStreamingSpringNow = deps.liveContentActive();
      const wantsSpringNow = wantsStreamingSpringNow || springStartedFromStructuralAppend;
      const withinTargetChangeRetainWindow =
        wantsSpringNow && now - lastTargetChangedAt < RETAIN_ANIMATION_DURATION_MS;

      if (current !== target && !deps.arrival.matches(target)) {
        // Content oscillation guard: if the sentinel was idle
        // (sentinelEntryTarget set), the target returned to the
        // sentinel entry value, AND unexplained scrollTop movement was
        // witnessed since entry, the content layer oscillated in
        // height (-N then +N from async Streamdown typesetting /
        // a windowing row remount). The browser auto-clamped scrollTop
        // during the low point (a native engine operation — not a
        // scrollTop write the controller could arbitrate), stranding
        // scrollTop below the restored target. Snap back instantly — a spring
        // chase for zero net content change is a visible artifact.
        // The witness requirement is what keeps an AUTHORED
        // displacement with the same numeric shape — a head-splice
        // compensation's anchor hold, whose hidden growth is owed a
        // glide — out of the snap: authored writes update the
        // provenance ledger, so they can never read as a clamp
        // (bug-report-20260801T213259Z). Without the witness the
        // else branch below runs instead: the sentinel exits and the
        // remainder glides in, which is the displacement's contract.
        //
        // This check is DELIBERATELY different from the resolver's
        // isSentinelOscillationStranded (scroll/resolver.ts): it
        // triggers on exact inequality filtered by arrival-readback
        // acceptance (the outer branch condition), not the 1px stranded
        // band — see the predicate's call-site map before unifying.
        if (
          sentinelEntryTarget >= 0
          && withinArrivalBand(target, rebasedSentinelEntryTarget(el.clientHeight))
          && witnessClampIfUnexplained()
        ) {
          snapOscillationToBottom('spring.oscillationSnap', target);
        } else {
          deps.arrival.clear();
          sentinelEntryTarget = -1;
          sentinelClampWitnessed = false;
          // Resume snap (see SPRING_MAX_CHASE_DISTANCE_VIEWPORTS for the
          // full gating rationale): only on a >viewport backlog paired
          // with an OBSERVED discontinuity — this tick's real rAF gap,
          // or a just-resumed document. The backlog accrued while
          // nothing was being watched, so it is placed in one write
          // rather than paid as motion; velocity resets to standstill so
          // growth arriving after the snap ramps up like any other cold
          // onset. Layout is clean here (targetScrollTop just read it),
          // so clientHeight doesn't reflow. Skip when unmeasured
          // (clientHeight 0): a zero threshold would degrade every chase
          // into a snap.
          const tickGapMs = previousTickAt === null ? 0 : now - previousTickAt;
          const resumedFromDiscontinuity =
            tickGapMs >= STALL_RESUME_GAP_MS
            || msSinceDocumentResume() <= RESUME_CLAMP_WINDOW_MS;
          if (resumedFromDiscontinuity) {
            const chaseLimitPx = el.clientHeight * SPRING_MAX_CHASE_DISTANCE_VIEWPORTS;
            if (chaseLimitPx > 0 && Math.abs(target - current) > chaseLimitPx) {
              const catchupReadback = deps.writeScrollTop('spring.catchupSnap', target, target);
              // Re-read: the engine may round or refuse the written value.
              // Reset kinematics only if the element actually MOVED — a
              // refused snap (write-refusal wedge) falls through to the
              // main write, whose classification counts it.
              if (catchupReadback !== undefined && catchupReadback !== current) {
                current = catchupReadback;
                accumulated = 0;
                velocity = 0;
                retarget.breakMotion();
                if (chaseTelemetry) chaseTelemetry.distanceJumps += 1;
              }
            }
          }
          if (integrationFrames > 0) {
            let remainingFrames = integrationFrames;
            while (remainingFrames > 0) {
              const stepFraction = Math.min(1, remainingFrames);
              remainingFrames -= stepFraction;
              // Re-derive the remaining gap per step from the in-frame
              // position (`current + accumulated`) — pure arithmetic, no
              // extra layout reads — so a multi-step catch-up follows the
              // same curve N sequential 60Hz frames would have.
              const stepDiff = target - (current + accumulated + tickDisplacement);
              const stepStartVelocity = velocity;
              // Slew base: the speed already pointing at the target when
              // this step began. A reversal contributes zero, so the new
              // direction ramps from the base; the catch-up jump's
              // cap-speed seed lands here and passes untouched. See
              // SPRING_ACCEL_SLEW_FACTOR_PER_FRAME.
              const towardTargetSpeed =
                stepDiff > 0
                  ? Math.max(0, velocity)
                  : stepDiff < 0
                    ? Math.max(0, -velocity)
                    : 0;
              const slewCeiling =
                Math.max(slewRampBase, towardTargetSpeed)
                * Math.pow(SPRING_ACCEL_SLEW_FACTOR_PER_FRAME, stepFraction);
              // Composable fractional-step discretization. The full-step
              // recurrence v' = (damping·v + stiffness·diff)/mass is an
              // exponential approach toward the quasi-steady follow
              // speed (RATIO·diff): v' = R·v + (1−R)·RATIO·diff with
              // R = damping/mass. A fraction f of it is exactly
              // R^f·v + (1−R^f)·RATIO·diff — identical at f=1, and BOTH
              // terms compose (two half-steps reproduce one full step
              // for a held diff), keeping high-refresh displays on the
              // same 60Hz-tuned shape. The historical form
              // ((damping^f·v + stiffness·f·diff)/mass) failed this
              // twice: the /mass didn't scale with f, so every extra
              // step bled 20% of the velocity regardless of fraction —
              // even the ~0.0002-frame micro-step that rAF timestamp
              // jitter (16.67ms vs the exact 60Hz 16.6667ms) appends to
              // nearly every tick (the slew ramp's velocity history
              // exposed that as a ramp resetting to the base every
              // tick) — and a linear f·gain input term under-drives
              // split steps by ~12% at 120Hz.
              const retention = Math.pow(SPRING_VELOCITY_RETENTION, stepFraction);
              velocity =
                retention * velocity
                + (1 - retention) * SPRING_QUASI_STEADY_FOLLOW_RATIO * stepDiff;
              // Hard speed cap: large chases glide at a bounded constant
              // speed instead of a distance-proportional zoom (see
              // SPRING_MAX_VELOCITY_PX_PER_FRAME).
              if (velocity > SPRING_MAX_VELOCITY_PX_PER_FRAME) {
                velocity = SPRING_MAX_VELOCITY_PX_PER_FRAME;
              } else if (velocity < -SPRING_MAX_VELOCITY_PX_PER_FRAME) {
                velocity = -SPRING_MAX_VELOCITY_PX_PER_FRAME;
              }
              // Deceleration envelope (speed ∝ remaining distance),
              // applied only to a velocity already pointing at the
              // target — reversals still turn on the spring curve. Caps
              // small-quantum peaks and shapes the ease-out. The tail
              // below it is the spring's own decay. See
              // SPRING_DECEL_ENVELOPE_RATIO.
              const remaining = Math.abs(stepDiff);
              const envelope = Math.min(
                SPRING_MAX_VELOCITY_PX_PER_FRAME,
                Math.max(
                  SPRING_DECEL_ENVELOPE_MIN_PX_PER_FRAME,
                  remaining * SPRING_DECEL_ENVELOPE_RATIO,
                ),
              );
              if (stepDiff > 0 && velocity > envelope) {
                velocity = envelope;
              } else if (stepDiff < 0 && velocity < -envelope) {
                velocity = -envelope;
              }
              // Acceleration slew: an onset builds speed at the ramp,
              // never jumping straight to the envelope's permit (see
              // SPRING_ACCEL_SLEW_FACTOR_PER_FRAME). Ordering with the
              // envelope is immaterial (both are ceilings); the floor
              // push-up below can never exceed the slew ceiling (its
              // base is ≥ the floor), so the three compose without
              // fighting.
              if (stepDiff > 0 && velocity > slewCeiling) {
                velocity = slewCeiling;
              } else if (stepDiff < 0 && velocity < -slewCeiling) {
                velocity = -slewCeiling;
              }
              // Once this quantum has run faster than the motion floor,
              // deceleration never sinks below it: the hold LATCHES the
              // first time the spring's own velocity falls under the
              // rung's rate and releases only when a target extension
              // drives it a twentieth above (through the slew ramp or the
              // retarget bridge). Near the floor the natural velocity
              // sits within a fraction of a percent of the rate for
              // frames at a time, and a hold keyed on the instantaneous
              // comparison interleaved two cadences (the 2026-09-04
              // 240Hz measurement). Reversals still decelerate through zero.
              if (velocity * stepDiff <= 0) {
                floorHolding = false;
              } else if (Math.abs(velocity) >= floorRate) {
                quantizedFloorEngaged = true;
              } else if (quantizedFloorEngaged) {
                velocity = Math.sign(stepDiff) * floorRate;
                floorHolding = true;
              }
              // The three ceilings above produce the fixed-target
              // candidate. A target extension during braking (or a held
              // speed) passes that candidate through the acceleration-
              // continuity bridge. Every other step returns it unchanged.
              const candidate = velocity;
              velocity = retarget.step(
                target,
                stepDiff,
                stepStartVelocity,
                velocity,
                stepFraction,
                SPRING_MAX_VELOCITY_PX_PER_FRAME,
              );
              if (
                floorHolding
                && velocity === candidate
                && velocity * stepDiff > 0
                && Math.abs(velocity) < floorRate * SPRING_FLOOR_RELEASE_FACTOR
              ) {
                // Floor regime: the tick advances the rung's average by
                // FRAME COUNT (one rAF tick is one displayed frame; a
                // dropped frame is not made up as a double step), spread
                // over the tick's sub-steps so the sum is exact whatever
                // the split. On the ladder below that displacement IS
                // the rung: `pxPerEvent` a tick, or one every
                // `framesPerEvent`.
                velocity = Math.sign(stepDiff) * floorRate;
                // The landing cradle (see the constant block): with
                // `n` pixel events left, the current one takes
                // (CRADLE + 1 − n) floor intervals — the rung's tick
                // displacement is emitted only on every `stretch`-th
                // tick since the last event, and the ladder below sees
                // the same rung at a longer cadence: the last three
                // events at k, 2k, 3k ticks, whatever the rung's size.
                const eventsLeft = Math.max(
                  1,
                  Math.ceil((Math.abs(stepDiff) - Math.max(
                    grid.readbackError,
                    // DOM positions may be returned through float32. Its
                    // relative precision must not add a phantom cradle event.
                    Math.max(Math.abs(current), Math.abs(target)) * 2 ** -23,
                  )) * dpr / floorEventPixels),
                );
                const stretch =
                  eventsLeft <= SPRING_LANDING_CRADLE_EVENTS
                    ? SPRING_LANDING_CRADLE_EVENTS + 1 - eventsLeft
                    : 1;
                if (stretch > 1 && !cradleTickCounted) {
                  cradleTickCounted = true;
                  cradleTicks += 1;
                }
                if (stretch === 1 || cradleTicks % stretch === 0) {
                  tickDisplacement +=
                    Math.sign(stepDiff) * floorTickCss * (stepFraction / integrationFrames);
                }
              } else {
                // Released: a reversal, the bridge ramping out of the
                // hold (its output differs from the candidate), or the
                // slew ramp past the release band.
                floorHolding = false;
                tickDisplacement += velocity * stepFraction;
              }
            }
            // ---- Whole-device-pixel step (see the constant block) ----
            // This tick's integrated displacement becomes a signed whole
            // number of device pixels off the ladder. The band is chosen
            // by the TICK's own displacement, never a carried total, so a
            // residue that accrued below one device pixel cannot surface
            // as a double step the moment the rate crosses one (the
            // `0 2 0 2` onset of the 2026-09-04 120Hz measurement).
            const tickDevicePx = tickDisplacement * dpr;
            const magnitude = Math.abs(tickDevicePx);
            const direction = Math.sign(tickDevicePx);
            let stepDevicePx = 0;
            // A reversal starts the ladder afresh; a tick with nothing to
            // write (a gated cradle tick) is not one, and keeps the
            // cadence count it is part of.
            if (direction !== 0 && direction !== rungDirection) {
              lastRung = 0;
              cadenceTicks = 0;
              cradleTicks = 0;
              rungDirection = direction;
            }
            if (magnitude >= SPRING_STEP_DIFFUSION_MIN_DEVICE_PX) {
              // Cruise: error diffusion, the nearest whole pixel out with
              // the residue carried, so the average rate is exact.
              const wantDevicePx = (accumulated + tickDisplacement) * dpr;
              stepDevicePx = Math.round(wantDevicePx);
              accumulated = (wantDevicePx - stepDevicePx) / dpr;
              lastRung = 0;
              cadenceTicks = 0;
              cradleTicks = 0;
            } else if (magnitude >= 1) {
              // Whole pixels a tick, held through hysteresis around the
              // previous rung; the residue is discarded so timestamp
              // jitter never accrues into an extra pixel.
              let rung = Math.round(magnitude);
              if (
                lastRung > 0
                && Math.abs(magnitude - lastRung) < 0.5 + SPRING_STEP_HYSTERESIS_DEVICE_PX
              ) {
                rung = lastRung;
              }
              stepDevicePx = direction * rung;
              lastRung = rung;
              cadenceTicks = 0;
              cradleTicks = 0;
              accumulated = 0;
            } else if (magnitude > 0) {
              // One pixel every k ticks, k the nearest cadence to the
              // displacement's reciprocal under the same hysteresis: a
              // rate under a pixel a tick is shown as an even cadence,
              // never as the irregular mix error diffusion would make
              // of it. The motion floor's rung lands here on its own.
              const ticksPerPx = 1 / magnitude;
              let cadence = Math.round(ticksPerPx);
              if (
                lastRung < 0
                && Math.abs(ticksPerPx + lastRung) < 0.5 + SPRING_STEP_HYSTERESIS_DEVICE_PX
              ) {
                cadence = -lastRung;
              }
              lastRung = -cadence;
              cadenceTicks += 1;
              if (cadenceTicks >= cadence) {
                stepDevicePx = direction;
                cadenceTicks = 0;
                cradleTicks = 0;
              }
              accumulated = 0;
            }
            if (stepDevicePx === 0) {
              // Nothing whole to write: touch neither the element nor the
              // refusal guard (no request was made, so nothing was
              // refused).
              if (chaseTelemetry) chaseTelemetry.zeroStepTicks += 1;
            } else {
              const next = current + stepDevicePx / dpr;
              // Pre-clamp in JS so we know the post-state without a second
              // layout read just to check whether the browser clamped. Cross-
              // target clamps in EITHER direction count as kinematic
              // overshoot: a positive-diff chase overshoots when
              // `next > target`, a negative-diff chase (the symmetric branch
              // that lets the spring follow shrinks) overshoots when
              // `next < target`. Both clamp to `target` (the one write that
              // may be fractional: the exact landing) and zero
              // `accumulated` below.
              const crossedTarget =
                (current < target && next > target)
                || (current > target && next < target);
              // Bias only interior requests into the accepted interval.
              // A floor-based engine must not lose a pixel to float error;
              // an exact target remains exact, including at a native clamp.
              const clamped = crossedTarget || next === target
                ? target : next + gridOffset;
              const postWriteTop =
                deps.writeScrollTop(
                  crossedTarget ? 'spring.overshoot' : 'spring.tick',
                  clamped,
                  target,
                ) ?? current;
              if (chaseTelemetry) chaseTelemetry.writeTicks += 1;
              if (clamped === target) {
                deps.arrival.record(target);
                // A landing from the floor is the glide's end: the
                // carried velocity would only decay to the ramp base the
                // next quantum starts from anyway, and the arrival check
                // below needs it under its threshold to settle the
                // spring on this frame, as the sub-pixel tail's decay
                // used to. A faster landing (a clamp across a moved
                // target) keeps its momentum for the carry rule.
                if (Math.abs(velocity) <= floorRate) velocity = 0;
                floorHolding = false;
                lastRung = 0;
                cadenceTicks = 0;
                cradleTicks = 0;
                accumulated = 0;
              }
              if (crossedTarget) {
                retarget.breakMotion();
              }
              // Three-way write classification (see the write-refusal
              // guard's constant block): MOVED heals, REFUSED counts,
              // INCONCLUSIVE (no motion, sub-threshold request) is
              // evidence of nothing and touches nothing.
              if (postWriteTop !== current) {
                // MOVED: the element accepted the write. Healing requires
                // observed MOTION — not merely "not refused" — because a
                // sub-threshold no-motion write (ramp onset, near-target
                // sliver) proves nothing about a wedge.
                if (writeRefusalLatched) {
                  deps.reportWriteRefusal({
                    phase: 'healed',
                    consecutiveRefusals: consecutiveRefusedWrites,
                    requested: clamped,
                    scrollTop: postWriteTop,
                    target,
                    wedgeMs: Math.round(now - refusalLatchedAt),
                  });
                  writeRefusalLatched = false;
                  refusalLatchedAt = 0;
                  lastRefusalRetryAt = 0;
                }
                consecutiveRefusedWrites = 0;
                // The model's residue was settled above, per band. The
                // engine's own readback residue (its grid snap, or a
                // max-scroll clamp) is never carried: `current` resyncs
                // to the readback below.
              } else if (Math.abs(next - current) >= gridQuantum * (1 - 1e-6) && Math.abs(target - current) > ARRIVAL_DISTANCE_PX) {
                // REFUSED WRITE: a real motion request moved the element
                // by nothing. Re-anchor the model to reality — dropping
                // `accumulated` pins the simulated position back to the
                // element's true one, so the next write stays within one
                // velocity-capped step of it and a heal can only ever be
                // a bounded glide, never a teleport. Velocity is kept:
                // it carries the ramped retry speed and seeds the heal
                // glide's cruise. Retry pacing is NOT stamped here — the
                // gate above the geometry reads owns the retry slot.
                accumulated = 0;
                retarget.breakMotion();
                consecutiveRefusedWrites += 1;
                if (chaseTelemetry) chaseTelemetry.refusedWrites += 1;
                if (
                  !writeRefusalLatched
                  && consecutiveRefusedWrites >= SPRING_WRITE_REFUSAL_LATCH_TICKS
                ) {
                  writeRefusalLatched = true;
                  refusalLatchedAt = now;
                  lastRefusalRetryAt = now;
                  deps.reportWriteRefusal({
                    phase: 'latched',
                    consecutiveRefusals: consecutiveRefusedWrites,
                    requested: clamped,
                    scrollTop: postWriteTop,
                    target,
                    wedgeMs: 0,
                  });
                }
              }
              current = postWriteTop;
            }
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
        // SPRING_CARRY_VELOCITY_CEILING for the derivation). The kept
        // remnant DECAYS toward the slew ramp base per REAL elapsed
        // frame (below, rationale at SPRING_ACCEL_SLEW_FACTOR_PER_FRAME).
        // Shed velocity entirely when:
        //   - outside the retain window → streaming paused; the arrival
        //     check below needs |velocity| < 0.5 to settle the spring (or
        //     hand it to the sentinel), else it ticks at 60fps forever;
        //   - downward (velocity <= 0) → carry is scoped to growth-follow;
        //     a shrink-follow remnant carried into a resumed growth would
        //     nudge the viewport the wrong way for a frame.
        accumulated = 0;
        // Each quantum re-earns the quantized floor: a caught-up spring's
        // next growth starts its ramp naturally (a carried remnant ≥
        // the floor re-engages it on the first step anyway).
        quantizedFloorEngaged = false;
        floorHolding = false;
        lastRung = 0;
        rungDirection = 0;
        cadenceTicks = 0;
        cradleTicks = 0;
        if (withinTargetChangeRetainWindow && velocity > 0) {
          if (velocity > SPRING_CARRY_VELOCITY_CEILING) {
            velocity = SPRING_CARRY_VELOCITY_CEILING;
          }
          // Parked decay, over the REAL elapsed frames — deliberately
          // NOT the catch-up-capped integrationFrames: that cap bounds
          // motion per tick, and decay is not motion. A long-task gap
          // while parked must count in full, or a pane visually still
          // for 300ms re-enters at carried speed. Guarded so a remnant
          // already at or below the base is left alone — the decay
          // converges on the base, never raises toward it.
          if (velocity > slewRampBase) {
            velocity = Math.max(
              slewRampBase,
              velocity / Math.pow(SPRING_ACCEL_SLEW_FACTOR_PER_FRAME, dtFrames),
            );
          }
        } else {
          velocity = 0;
        }
        // No visible motion occurred in this tick. A later target change
        // is a carried or cold start, not a mid-motion retarget, so it must
        // not inherit the bridge.
        retarget.breakMotion();
      }

      // Arrival check uses the cached `target` for the position
      // comparison; the time delta uses rAF's `now` (matches
      // `nowMs()` in test environments because `performance.now` is
      // mocked to read the same source rAF passes the callback).
      // Liveness lapsing mid-flight (content stopped arriving) or
      // RETAIN_ANIMATION_DURATION_MS elapsing without another
      // target-change event makes
      // `withinTargetChangeRetainWindow` (computed above) false, so the
      // spring lands on its next arrival check rather than chasing forever.
      // Bidirectional — applies to downward chases (shrinks) as well as
      // upward (growth).
      const arrived =
        withinArrivalBand(current, target)
        && Math.abs(velocity) < ARRIVAL_VELOCITY_THRESHOLD;
      if (arrived && !withinTargetChangeRetainWindow) {
        if (wantsStreamingSpringNow) {
          // Streaming active but no target change within the retain
          // window (async shiki load, inter-chunk gap, parseIncomplete
          // Markdown rebalance). Keep the spring sentinel-alive so
          // `springActive` stays true for the springActive-keyed
          // resolver behavior — chiefly resolveContentDelivery's
          // negative-delta carve-out (plus its overshoot and
          // idle-deadband clauses). Without this, cancel() sets
          // springToken=0 and the dead window lets a negative
          // contentRO sync-pin land instantly — visible as 1-2 lines
          // of instant jump mid-stream. The next positive contentRO
          // delta bumps
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
            sentinelEntryClientHeight = el.clientHeight;
            // A fresh sentinel session starts with a clean evidence
            // slate — the arrival that entered it just explained the
            // current position.
            sentinelClampWitnessed = false;
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
    velocityForTest: () => velocity,
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
    sentinelTarget: () => {
      // No sentinel armed (the overwhelmingly common case): answer
      // without touching the DOM. The clientHeight read below is a
      // layout-dependent read, and snapshot sampling now runs on the
      // controller's read-free delta path where layout may be dirty —
      // an unconditional read here would be the forced pass that path
      // exists to avoid.
      if (sentinelEntryTarget < 0) return -1;
      // Rebased at sample time so the resolver's stranded-oscillation
      // comparison stays content-height-based even when the viewport
      // resized mid-sentinel (see the field's declaration).
      const el = deps.getScrollEl();
      if (!el) return sentinelEntryTarget;
      return rebasedSentinelEntryTarget(el.clientHeight);
    },
    // Lazy latch on read: a delivery sampling the snapshot between the
    // clamp and the next sentinel tick still sees the evidence.
    sentinelClampWitnessed: () => witnessClampIfUnexplained(),
    refusalLatched: () => writeRefusalLatched,
  };
}
