// Pure decision core for the sticky-bottom scroll controller
// (scroll/index.svelte.ts). Stage 2 of the scroll
// re-architecture (docs/architecture/scroll-rearchitecture-plan.md):
// the controller gathers observations (DOM reads, sampled options,
// clocks) and applies effects (scrollTop writes, spring lifecycle,
// intent flags); every DECISION about a content-resize delivery lives
// here as a pure function so the full state × observation matrix is
// unit-testable without timer choreography.
//
// Contract with the controller:
// - At most ONE write per delivery. The legacy controller could write
//   twice per ResizeObserver delivery (overshoot snap, then pin) — both
//   writes always carried the same cached bottom target, so collapsing
//   them is value-identical; only the intermediate token/trace pair
//   disappears. The caller label on the collapsed write is the label
//   of the LAST branch that would have written, i.e. the write that
//   actually landed.
// - The reducer never reads the DOM or the clock. Anything time- or
//   layout-dependent is sampled into the observation by the controller.

// Spring arrival band: distance ≤1px from target counts as "at target".
// Shared by the overshoot guard here, the spring's arrival check, and
// the exact-arrival write suppression in the controller.
export const ARRIVAL_DISTANCE_PX = 1;

// Auto-follow re-stick band: a DOWN-direction scroll that lands within
// this many pixels of the bottom flips the user back to sticky, and the
// same tolerance decides "the DOM is already pinned" for the engine
// compensation resolver's anchor-redirect. Matches react-virtuoso's
// `atBottomThreshold` default — tolerates row-height estimation +
// browser scrollTop rounding that routinely lands 1-3px short during
// streaming.
export const AUTO_FOLLOW_BOTTOM_EPSILON_PX = 4;

// Idle re-pin deadband. Once the spring has settled (no spring active) and
// scrollTop is already within this many px of the bottom target, a nonzero
// content-height delta is treated as fractional-DPR wobble — not real growth —
// and the re-pin is skipped, breaking the idle viewport-vibration limit cycle
// at its source. Value: large enough to clear the observed ~2px idle flip with
// margin, small enough to stay well below the ≥~line-height gap of genuine
// catch-up growth. Defined AS AUTO_FOLLOW_BOTTOM_EPSILON_PX — "close enough
// to count as at-bottom" and "close enough not to fight a wobble" are the
// same tolerance, and the alias keeps them from drifting apart. Full
// mechanism (fractional-DPR X.5-boundary height flip → moving target →
// self-sustaining ±2px cycle) + the capture it was root-caused from:
// docs/architecture/settle-flicker-analysis.md.
//
// Carried as an explicit reducer branch per the Stage-2 plan; deletable
// later only with a fresh idle fractional-DPR capture proving the flip
// driver is gone.
export const IDLE_REPIN_DEADBAND_PX = AUTO_FOLLOW_BOTTOM_EPSILON_PX;

// Overshoot magnitude at which the delivery resolver snaps scrollTop
// instantly even while a spring chase is in flight. Second policy
// consumer: the controller's live-content observation path (internal
// notifyLiveContentMaybeGrew) mirrors the same absorb-below/snap-above
// split for its structural nudges — a
// change here applies to both. Small overshoots
// (≤ this) come from transient streamdown re-renders —
// parseIncompleteMarkdown auto-balancing unclosed code fences /
// backticks / lists momentarily shrinks scrollHeight by a handful of
// pixels — and snapping for them produced the user-visible "viewport
// jumps upward then springs back" regression on plain-text streams.
// Large overshoots (virtualizer measurement mis-corrections, content
// collapse) still snap instantly so the user doesn't watch the viewport
// drift down across many frames.
export const SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX = 50;

/** |a - b| within the shared 1px arrival band. */
export function withinArrivalBand(a: number, b: number): boolean {
  return Math.abs(a - b) <= ARRIVAL_DISTANCE_PX;
}

// Controller state the delivery decision depends on, sampled at the top
// of the ResizeObserver callback.
export interface ResolverState {
  /** Intent flag: "we want to be glued to the bottom" (isAtBottomState). */
  isAtBottom: boolean;
  /** Geometric ≤70px near-bottom band (isNearBottomState, post-refresh). */
  isNearBottom: boolean;
  /** User explicitly escaped bottom follow. */
  escaped: boolean;
  /** An auto-scroll pause lease is held (pauseDepth > 0). */
  paused: boolean;
  /** Warm-up (quiescence) gate has cleared. */
  warm: boolean;
  /**
   * A spring chase or sentinel is in flight (the controller's
   * `springToken !== 0`; sampled by `resolverStateSnapshot` in
   * scroll/index.svelte.ts).
   */
  springActive: boolean;
  /** setEscapedFromLock(true) requested the spring to stop. */
  springStopRequested: boolean;
  /**
   * The structural-append one-shot window is open (spring.
   * structuralAppendPending(): a wire append / reveal release armed
   * within the last 250ms). The append's growth is owed a glide, so
   * bottom-seeking instant writes must yield — see the anchor-redirect
   * tier in resolveEngineCompensation.
   */
  structuralAppendPending: boolean;
  /**
   * Bottom target captured when the spring sentinel was first entered,
   * rebased by the sampler for any clientHeight change since entry (so
   * comparing it against the current target compares CONTENT heights —
   * a viewport resize mid-sentinel must not make an appended row look
   * like an oscillation restore); -1 when not sentinel-idle. Drives the
   * stranded-oscillation recovery.
   */
  sentinelEntryTarget: number;
  /**
   * Clamp evidence from the provenance ledger
   * (spring.sentinelClampWitnessed()): unexplained scrollTop movement —
   * beyond the last authored write's readback / last classified user
   * scroll — was witnessed since the sentinel entered. The
   * stranded-oscillation recovery requires it: a baseline match alone
   * is a numeric shape an authored head-splice displacement also
   * produces, and snapping for one stomps the glide its hidden growth
   * is owed (bug-report-20260801T213259Z). Always false when no
   * sentinel is armed.
   */
  sentinelClampWitnessed: boolean;
}

// Per-delivery inputs the controller samples alongside the state. The
// clock-dependent option read (the reduced-motion media query) is
// sampled once per delivery so the decision is reproducible.
export interface ContentDeltaObservation {
  kind: 'delta';
  /** Nonzero contentRO height delta (zero deltas never reach the resolver). */
  delta: number;
  /** scrollTop read at delivery time, before any write this decision makes. */
  scrollTop: number;
  /** Bottom target (scrollHeight - clientHeight), cached once per delivery. */
  target: number;
  /**
   * A content-column width change is active (this delivery or within the
   * reflow settle window): the height delta is layout correction for
   * already-rendered content, so it sync-pins even in spring mode.
   */
  widthReflowActive: boolean;
  /** OS prefers-reduced-motion OR the app's low-power setting — the
   * controller feeds this from its combined motionReduced() gate. */
  prefersReducedMotion: boolean;
}

// The very first contentRO fire after attach has no height baseline:
// snap to bottom if the intent flags say so (matches upstream's
// `initial` behavior when isAtBottom starts true). Note there is
// deliberately NO pause gate on this path — a lease held across attach
// must not leave the first paint mid-thread.
export interface ContentFirstObservation {
  kind: 'first';
  target: number;
}

export type ContentObservation = ContentDeltaObservation | ContentFirstObservation;

export type ContentWriteCaller =
  | 'contentRO.firstFire'
  | 'contentRO.overshoot'
  | 'contentRO.positiveDelta'
  | 'contentRO.negativeDelta'
  | 'contentRO.negativeDeltaReflow'
  | 'contentRO.oscillationSnap';

export interface ContentDecision {
  /** At most one scrollTop write per delivery; value is always the bottom target. */
  write: { caller: ContentWriteCaller; value: number } | null;
  /** Start (or continue) the spring chase toward the new bottom. */
  startSpring: boolean;
  /**
   * Refresh the spring's target-changed timestamp without starting a
   * chase: the spring is mid-chase and the sync write was suppressed,
   * but the target moved (downward) — without the bump the retain
   * window could lapse between chunks.
   */
  bumpTargetChanged: boolean;
  /**
   * The stranded-at-bottom oscillation recovery fired: the controller
   * zeroes the spring's velocity/accumulated and consumes the sentinel
   * entry target so the rAF-side recovery no-ops for this oscillation.
   */
  oscillationRecovery: boolean;
  /** Negative-delta re-stick flips the intent flag back to bottom follow. */
  setIsAtBottom: boolean;
}

// Gate conditions for a spring chase. One predicate shared by the
// delivery resolver, the controller's startSpringIfNeeded, and the
// live-content observation path (internal notifyLiveContentMaybeGrew)
// so the sites cannot drift on which conditions allow the spring. The `warm` check is deliberately NOT part
// of this predicate — warmth gates whether a positive delta may spring
// at all, and the callers that need it check it explicitly.
//
// Every input describes the SCROLLER'S STATE, not a guess about what
// caused the growth. There is deliberately no "is this live content?"
// term: while pinned at the bottom, the destination is the bottom
// whatever moved the target, so the only question is the route, and the
// route should always be the glide. Cases that genuinely must not
// animate carry their own signal and are checked by their own gate —
// mount/restore cascade (`warm`), layout correction for a width change
// (`widthReflowActive`), reduced motion (here), sub-pixel idle wobble
// (the deadband), user escape / lease (here). A recency window over the
// last content stamp is not such a signal: it says when a stamp last
// happened, not what this growth is, and keying physics on it teleported
// every growth that no code path happened to stamp — late row enrichment
// and any drain growth landing in a reveal gap (2026-07-25). Liveness
// still has a job (spring sentinel, live-capable nudges); it just isn't
// this one. See utils/liveContentActivity.ts.
export interface SpringGateInputs {
  springStopRequested: boolean;
  paused: boolean;
  isAtBottom: boolean;
  escaped: boolean;
  /** OS prefers-reduced-motion OR the app's low-power setting. */
  prefersReducedMotion: boolean;
}

export function springGateIsOpen(s: SpringGateInputs): boolean {
  return !s.springStopRequested
    && !s.paused
    && s.isAtBottom
    && !s.escaped
    && !s.prefersReducedMotion;
}

// Stranded-at-bottom oscillation: a row ABOVE the viewport transiently
// shrank and regrew while the spring sat sentinel-idle at the bottom —
// a windowing remount/remeasure of a replaced element, e.g. an image
// user-message row scrolled out of the live window. The virtualizer
// sizes its container explicitly (`contain: size` + `height:
// <totalSize>px`), so the dip is the contentEl height the controller
// observes. While pinned
// at the exact bottom, the browser SYNCHRONOUSLY clamps scrollTop down
// during the dip (a native operation no write gate can see); the regrow
// restores the target to exactly the sentinel-entry value, leaving
// scrollTop stranded below the restored bottom. Recovery is an instant
// snap — a spring chase for zero net content change is a visible
// artifact. Genuine new growth (target beyond the sentinel entry) still
// spring-chases, and active chases are untouched. The arrival-band
// stranded check ignores sub-pixel rounding between the browser-rounded
// scrollTop readback and the computed target.
//
// The clamp is EVIDENCED, not inferred: `sentinelClampWitnessed` comes
// from the provenance ledger (the chokepoint's authored-write readback
// plus classified user scrolls), which witnessed scrollTop at a value no
// write explains. Requiring it keeps authored displacements with the
// same numeric shape — a head-splice compensation holding its anchor
// while the hidden growth is owed a glide — out of the snap
// (bug-report-20260801T213259Z).
//
// Call-site map: ONLY the delivery resolver uses this predicate
// (synchronous, same-frame recovery — rAF callbacks fire BEFORE
// ResizeObserver callbacks within a frame, so RO-side recovery is the
// one that avoids painting the stranded frame;
// bug-report-20260615T182227Z was the ~37px one-frame jolt of the late
// tick-side snap). The spring tick's rAF-side recovery DELIBERATELY
// keeps its own inline check with different semantics: it triggers on
// exact inequality (`current !== target`) filtered by arrival-readback
// acceptance, not this predicate's 1px stranded band — engines that
// reject an exact max-scrollTop write by one CSS pixel must not
// re-snap every sentinel tick. Do not "unify" the two without
// reconciling that difference. The tick side survives Stage 2 as the
// fallback for strand-and-restore cycles that produce no contentRO
// delivery (clientHeight-driven target moves, forced-layout clamps
// with net-zero height delta); whether it can be deleted outright is a
// Stage-2 UNKNOWN awaiting capture evidence (plan §3).
export function isSentinelOscillationStranded(s: {
  springActive: boolean;
  sentinelEntryTarget: number;
  sentinelClampWitnessed: boolean;
  isAtBottom: boolean;
  escaped: boolean;
  paused: boolean;
  scrollTop: number;
  target: number;
}): boolean {
  return s.springActive
    && s.sentinelEntryTarget >= 0
    && s.sentinelClampWitnessed
    && s.isAtBottom
    && !s.escaped
    && !s.paused
    && withinArrivalBand(s.target, s.sentinelEntryTarget)
    && !withinArrivalBand(s.scrollTop, s.sentinelEntryTarget);
}

export function resolveContentDelivery(
  state: ResolverState,
  obs: ContentObservation,
): ContentDecision {
  if (obs.kind === 'first') {
    return {
      write: state.isAtBottom && !state.escaped
        ? { caller: 'contentRO.firstFire', value: obs.target }
        : null,
      startSpring: false,
      bumpTargetChanged: false,
      oscillationRecovery: false,
      setIsAtBottom: false,
    };
  }

  const { delta, scrollTop, target } = obs;
  const overshootMagnitude = Math.max(0, scrollTop - target);
  const overshoot = overshootMagnitude > ARRIVAL_DISTANCE_PX;
  // Idle re-pin deadband gate. Only while no spring is in flight (the
  // spring holds its token across inter-chunk gaps during streaming, so
  // this never trips mid-chase) AND scrollTop is already within the
  // deadband of the bottom: a nonzero height delta here is the
  // fractional-DPR content-box wobble, and re-pinning chases a target
  // that never stops moving → the idle viewport-vibration limit cycle.
  // Suppress the pin; real growth moves the target ≥ a line height away
  // (gap ≫ deadband) and pins normally on the next delivery.
  const distanceFromTarget = Math.abs(scrollTop - target);
  const idlePinWithinDeadband =
    !state.springActive && distanceFromTarget <= IDLE_REPIN_DEADBAND_PX;
  const positiveWillPin = delta > 0
    && state.isAtBottom
    && !state.escaped
    && !state.paused
    && !idlePinWithinDeadband;
  const negativeWillPin = delta < 0
    && (state.isAtBottom || state.isNearBottom)
    && !state.escaped
    && !state.paused
    && !idlePinWithinDeadband;

  let write: ContentDecision['write'] = null;
  let startSpring = false;
  let bumpTargetChanged = false;
  let oscillationRecovery = false;
  let setIsAtBottom = false;

  // Overshoot guard: browser auto-clamping or virtualizer corrections
  // pushed scrollTop past the target — snap back. Two clauses past the
  // escape / pause gates:
  //
  // 1. No spring in flight: any overshoot snaps. There is no other
  //    writer that will absorb it (the original Bug-A defense for the
  //    virtua-era applyJump landing past the bottom mid-cascade — the
  //    warm gate keeps the spring suppressed there, so this clause is
  //    always the one reached during the cascade).
  // 2. Spring in flight AND magnitude exceeds the instant-snap
  //    threshold: a large overshoot absorbed by the spring is fatal to
  //    follow UX (the viewport visibly drifts down 100+px across many
  //    frames).
  //
  // Small overshoots during a chase (≤ threshold) fall through both
  // clauses and are absorbed by the symmetric spring: it sees diff < 0
  // and damps down to target across rAF ticks — the fix for the
  // parseIncompleteMarkdown few-pixel up-down jitter per chunk.
  // prefers-reduced-motion users still get the snap on small overshoots:
  // the spring gate is closed for them, so no spring is ever active and
  // clause 1 fires.
  if (
    overshoot
    && !state.escaped
    && !state.paused
    && (!state.springActive || overshootMagnitude > SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX)
  ) {
    write = { caller: 'contentRO.overshoot', value: target };
  }

  // The stranded check must see the scrollTop AS OF this point in the
  // delivery — i.e. after the overshoot guard's write, if it fired.
  // (The legacy controller read the live DOM here, which the overshoot
  // write had already moved; a pre-write read would call an
  // already-recovered position "stranded" and consume the sentinel.)
  const scrollTopAfterOvershootGuard = write !== null ? write.value : scrollTop;

  if (isSentinelOscillationStranded({
    springActive: state.springActive,
    sentinelEntryTarget: state.sentinelEntryTarget,
    sentinelClampWitnessed: state.sentinelClampWitnessed,
    isAtBottom: state.isAtBottom,
    escaped: state.escaped,
    paused: state.paused,
    scrollTop: scrollTopAfterOvershootGuard,
    target,
  })) {
    write = { caller: 'contentRO.oscillationSnap', value: target };
    oscillationRecovery = true;
  } else if (positiveWillPin) {
    // Positive delta: the content got taller while the viewport is
    // pinned to the bottom, so the bottom moved and we follow it. That
    // ALWAYS glides once the controller has warmed past mount settle and
    // the delta is not width-reflow layout correction — no question
    // about what produced the growth (see springGateIsOpen). Both routes
    // end at the same scrollTop; the glide is the one that stays
    // readable.
    //
    // The width-reflow carve-out exists because Mermaid, KaTeX, Shiki,
    // images, and normal prose all change height when the content
    // column width changes. That resize is layout correction for
    // already-rendered content, not the bottom advancing — sync-pin it
    // so a pane/sidebar/window reflow cannot produce a half-viewport
    // spring chase from a stale bottom. (Width and height can arrive in
    // separate RO deliveries; the controller holds the classification
    // open for a settle window after a width change.)
    //
    // The warm gate is what keeps a thread-switch restore from chasing
    // its mount cascade (e00723f): the cascade fires while !warm, so it
    // sync-pins exactly as before.
    if (
      state.warm
      && springGateIsOpen({
        springStopRequested: state.springStopRequested,
        paused: state.paused,
        isAtBottom: state.isAtBottom,
        escaped: state.escaped,
        prefersReducedMotion: obs.prefersReducedMotion,
      })
      && !obs.widthReflowActive
    ) {
      startSpring = true;
    } else {
      write = { caller: 'contentRO.positiveDelta', value: target };
    }
  } else if (negativeWillPin) {
    // Negative delta: re-stick when EITHER the intent flag or the
    // geometric near-bottom band says "stay at bottom". The geometric
    // branch matches upstream's negative-resize re-stick; the intent
    // branch defends against the virtualizer's compensation write
    // flipping isNearBottom=false purely as a downstream effect of an
    // above-viewport remeasure cascade (the "half-screen jump to
    // bottom" on heavy uncached threads — see frontend-scroll.md).
    setIsAtBottom = true;
    // Spring carve-out: suppress the sync write while a spring is
    // chasing so the engine's +ESTIMATE/-CORRECTION pair on row-append
    // (e.g. +90 then -56 within ~5ms) doesn't race the spring. Without
    // it, the negative write lands scrollTop at the corrected target
    // before the spring's first paint and the spring ticks against
    // current==target with no perceptible motion; with it, the spring
    // reads the target each tick and absorbs the correction naturally.
    // For that estimate-correct pair the spring has barely moved by the
    // time the correction arrives, so the overshoot guard above is
    // false and this gate is the only path that needed suppression.
    // Bug-A defense (sync-pin running during the !warm cascade) is
    // preserved by warm-gate ordering: the cascade fires while !warm,
    // the spring gate requires warm, so no spring is active during the
    // cascade and the sync-pin runs as before. Width reflow overrides
    // the carve-out: the paired growth is layout correction and
    // sync-pins, so the shrink must too.
    if (!state.springActive || obs.widthReflowActive) {
      write = {
        caller: obs.widthReflowActive ? 'contentRO.negativeDeltaReflow' : 'contentRO.negativeDelta',
        value: target,
      };
    } else {
      bumpTargetChanged = true;
    }
  }

  return { write, startSpring, bumpTargetChanged, oscillationRecovery, setIsAtBottom };
}

// ===== engine scroll-jump compensation (routed single writer) =====
//
// The bespoke timeline engine (utils/virtual/) compensates for
// above-viewport row remeasurements and head splices by REPORTING an
// absolute scroll target instead of writing scrollTop itself
// (TimelineVirtualizer `onCompensation` → the controller's
// `applyEngineCompensation`): this resolver decides what to do with each
// observation, keeping the controller the single scrollTop writer during
// follow by construction.
//
// Decision order (each tier's provenance):
// - head-splice verbatim: head mutations (load-older prepend, paged
//   head-drop prune) — the engine's offset math is exact and the anchor
//   must hold.
// - reading / paused / pre-warm: above-viewport visual stability is the
//   whole point of the compensation; it must land (mount-cascade +
//   revert-to-top histories — suppressing these visibly shifts the
//   reading anchor).
// - anchor-redirect: DOM already pinned at bottom && the engine requests
//   a target meaningfully above it — its delta only compensates
//   above-viewport remeasures, not the at/below-fold growth that moved
//   the bottom. Landing it paints one frame a few hundred px short (the
//   cold-thread-switch flicker, 8bf8b97f); redirect to the controller's
//   own bottom target so the two writers cannot disagree on where the
//   bottom is.
// - otherwise: apply verbatim — mid-chase included. The compensation is
//   an exact coordinate shift: layout moved everything under the
//   viewport by delta, and the write moves the viewport by the same
//   delta in the same post-flush moment, before paint, so the visual
//   field holds still. The spring re-reads el.scrollTop every tick, so
//   an applied write mid-chase just relocates the chase — the remaining
//   gap (target − scrollTop) is unchanged and the glide continues
//   seamlessly (same reason writes larger than the viewport were always
//   applied: bug-report-20260622T041049Z's +2276px suppressed write →
//   ~1s visible chase; and no-chase suppression produced the
//   thread-switch flicker, bug-report-20260524T200233Z).
//
// There is deliberately NO mid-chase decline tier. The virtua-era gate
// declined sub-viewport compensations while a spring chase was in
// flight ("the spring is the single writer mid-chase", eb99de2e) — but
// declining an exact compensation is what CAUSED the visible jump: the
// content shifted under the stationary viewport by the full delta
// (e.g. a background completion patching its collapsed tool row above
// the viewport into its settled height), then the spring re-chased the
// same distance. Design symmetry, not an incident, motivated the
// decline; the settle-flicker snap it nominally guarded was root-caused
// to margin divergence and is fixed structurally (flow-root row
// containment + the timeline.margin.diverge oracle). A decline needs no
// model re-sync (the engine's offset follows real scroll events), but
// "safe to decline" was never "right to decline".
//
// Deliberate deviation from the legacy gate: there is no animation-mode
// tier. The gate passed writes whenever the mode latch read 'instant',
// which made arbitration depend on that latch's timing — a mode flip
// while a chase/sentinel was alive opened the gate mid-spring ("snap up,
// spring down" per wire-round gap), pinned in place only by a cross-file
// hold > retain invariant. Keying on springActive — the actual thing the
// arbitration protects — makes that timing irrelevant. The latch itself
// is gone now (growth physics takes no such input, see springGateIsOpen),
// so this tier could not come back even if someone wanted it.

export type EngineWriteCaller = 'engine.compensation' | 'engine.anchorRedirect';

export interface EngineCompensationObservation {
  /** What moved: an above-viewport remeasure or a head splice. */
  kind: 'remeasure-above' | 'head-splice';
  /** Absolute target offset the engine computed (current offset + delta). */
  target: number;
  /** Live DOM scrollTop at delivery. */
  scrollTop: number;
  /** The controller's own bottom pin target (scrollHeight - clientHeight). */
  bottomTarget: number;
}

export interface EngineCompensationDecision {
  /**
   * The one write this delivery makes, through the controller chokepoint
   * (tagged like every controller write). Every delivery writes — the
   * only choice is the value (verbatim target vs anchor-redirect to the
   * controller's bottom target).
   */
  write: { caller: EngineWriteCaller; value: number };
}

export function resolveEngineCompensation(
  state: ResolverState,
  obs: EngineCompensationObservation,
): EngineCompensationDecision {
  if (obs.kind === 'head-splice') {
    return { write: { caller: 'engine.compensation', value: obs.target } };
  }
  if (!state.warm || !state.isAtBottom || state.escaped || state.paused) {
    return { write: { caller: 'engine.compensation', value: obs.target } };
  }
  const domAlreadyPinned =
    obs.bottomTarget - obs.scrollTop <= AUTO_FOLLOW_BOTTOM_EPSILON_PX;
  const movesAwayFromBottom =
    obs.bottomTarget - obs.target > AUTO_FOLLOW_BOTTOM_EPSILON_PX;
  // The redirect keeps a pinned reader at the bottom through pure
  // above-viewport corrections (estimate fixes, settled-height patches,
  // sentinel-gap shrinks — springActive alone does NOT yield). But when
  // the structural-append window is open, "the bottom" already includes
  // a row whose arrival is owed a glide: an off-screen shrink landing in
  // the same delivery as a tail append (auto-collapse release racing a
  // tool-call arrival, bug-report-20260731T141600Z) would teleport the
  // reader onto the new row and leave the armed spring with zero
  // distance. Preserve the pre-append view instead; the arm's follow-up
  // nudge glides the remainder to the bottom.
  if (domAlreadyPinned && movesAwayFromBottom && !state.structuralAppendPending) {
    return { write: { caller: 'engine.anchorRedirect', value: obs.bottomTarget } };
  }
  return { write: { caller: 'engine.compensation', value: obs.target } };
}
