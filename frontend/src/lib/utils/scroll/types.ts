// Public types for the stick-to-bottom scroll controller.
//
// The controller implementation lives in ./index.svelte.ts; the
// pure per-delivery decision logic lives in ./resolver.ts. This file owns
// the shapes consumers program against: the controller surface, its
// options, the observation-kind hint, and the closed union of programmatic
// write callers.

import type { ContentGeometrySample, EngineCompensation } from '../virtual/types';
import type { ContentWriteCaller, EngineWriteCaller } from './resolver';

// Every programmatic scrollTop write names its origin AT the single write
// site (`writeScrollTop(caller, value)`). Trace attribution and the
// spring-tick trace sampling both key off it; the closed union keeps a new
// write path from landing without declaring itself here. The contentRO.*
// and engine.* members come from the resolver's decision unions so the
// two cannot drift.
export type ScrollWriteCaller =
  | ContentWriteCaller
  | EngineWriteCaller
  | 'virtualizer.scrollTarget'
  | 'spring.tick'
  | 'spring.overshoot'
  | 'spring.arrive'
  | 'spring.oscillationSnap'
  | 'spring.catchupJump'
  | 'notifyContentMaybeGrew'
  | 'notifyLiveContentMaybeGrew'
  | 'notifyLiveContentMaybeGrew.arrive'
  | 'forceStick'
  | 'preserveScrollAnchor'
  | 'pauseAutoScroll.release';

/**
 * Source hint for `observe(kind)` — what kind of geometry/content event
 * the caller witnessed. The controller maps kinds onto its two internal
 * notify paths:
 *
 * - `'content'` / `'host-layout'` → escape-aware instant re-pin
 *   (composer padding, pane reorder, sidebar/terminal layout settles).
 * - `'live-content'` / `'composer-geometry'` → the live-capable path
 *   that honors animationMode and structural-append marks, so active
 *   output can spring through the change instead of snapping (falls
 *   back to the same instant re-pin when no spring is warranted).
 *
 * MessageTimeline's pane-facing adapter overrides `'host-layout'` to
 * re-pin and revalidate its virtualizer geometry; the raw controller
 * mapping is the correct fallback for surfaces without one (ChannelView).
 */
export type ScrollObservationKind =
  | 'content'
  | 'live-content'
  | 'composer-geometry'
  | 'host-layout';

/**
 * Why the warm-up gate last opened for the current cycle — mirrors
 * observers.ts `markWarm`'s reasons ('quiet' | 'failsafe' | 'settled'),
 * plus 'skip' (`skipWarmup()` forced the gate open) and `null` before
 * it has opened since the last `attach()` / `armWarmup()` /
 * restore-reason `forceStick()`. Diagnostic-only — feeds the
 * `timeline.coldload` dev-trace record (see `utils/coldLoadTrace.ts`);
 * no consumer should branch scroll behavior on this value, only
 * `isWarm`.
 */
export type WarmReason = 'quiet' | 'failsafe' | 'settled' | 'skip' | null;

export interface UseStickToBottomController {
  /** True when sticky AND no lease is held. Drives auto-follow gating. */
  readonly isSticky: boolean;
  /**
   * True when the timeline should hide the ScrollToBottomButton: sticky
   * by intent, or geometrically near bottom while not explicitly escaped.
   * User escape wins over the near-bottom band.
   */
  readonly isAtBottom: boolean;
  /** True when the user has explicitly moved the outer scroller away from bottom. */
  readonly escapedFromLock: boolean;
  /**
   * True once the warm-up gate has cleared — a quiet window of content
   * silence elapsed, FAILSAFE_MS tripped, or an engine-sourced sample
   * carried settle evidence (window fully measured within epsilon of its
   * estimates — the priors-hit revisit) with the typesetting signal
   * settled or absent (the 'settled' fast-path, external geometry source
   * only). Use as a "the measurement cascade has settled" signal:
   * consumers can hide content during the cascade and reveal here to
   * avoid showing the user a brief estimated-size paint before the
   * measured-size correction lands. Reset to false on attach,
   * restore-reason forceStick, and explicit armWarmup() — the latter
   * is the seam for "I'm about to render fundamentally different
   * content (e.g. thread switch) and the DOM update will happen BEFORE
   * my next forceStick / attach call, so reset the gate now."
   */
  readonly isWarm: boolean;
  /**
   * Reports which path last opened the warm-up gate — see `WarmReason`.
   * Purely a reporting surface alongside `isWarm`; consumers should
   * still gate behavior on `isWarm` and only read this for diagnostics.
   */
  readonly warmReason: WarmReason;

  /** Depth-counted lease that suspends auto-scroll until released. */
  pauseAutoScroll(): () => void;
  /**
   * Notify the controller that geometry or content may have changed in a
   * way its own observers cannot see (composer padding, pane reorder,
   * live content advancing without a usable contentRO delta). The kind
   * is a source hint that picks the response path — see
   * `ScrollObservationKind`. Escape/pause/user-intent aware: never
   * yanks a user who scrolled away.
   */
  observe(kind: ScrollObservationKind): void;
  /**
   * Mark the next near-term content growth as append-like structural
   * transcript growth. This lets command/tool row batches spring-follow
   * instead of snapping, without making unrelated idle layout reflows
   * spring-eligible.
   */
  markStructuralContentPending(): void;
  /**
   * Run an explicit user disclosure action while preserving the user's
   * current follow intent. Sticky users stay pinned to bottom; escaped
   * users keep the clicked anchor at the same viewport position.
   */
  preserveScrollAnchor(anchor: HTMLElement, action: () => void | Promise<void>): Promise<void>;

  attach(scrollEl: HTMLElement, contentEl: HTMLElement): void;
  detach(): void;

  /**
   * Snap to the bottom and resume auto-follow. Two reasons:
   *
   * - `'user'` (default): an explicit bottom-follow action such as the
   *   scroll-to-bottom chip. Always clears `escapedFromLock` and lands
   *   scrollTop at the target.
   * - `'restore'`: a thread-restore-style snap. Honored ONLY if
   *   `armRestoreSnap()` was called since the last user-initiated
   *   escape; otherwise NO-OP. This prevents a stale or duplicated
   *   restore $effect from clobbering an existing user escape with a
   *   snap-to-bottom they didn't ask for (the seq-509 trace bug).
   *
   * Callers writing "user clicked to go to bottom" should pass `'user'`
   * (or nothing). Callers writing "I just landed on this thread and
   * the saved snapshot says bottom" should pass `'restore'` AND have
   * paired their call with an `armRestoreSnap()` from the same
   * thread-switch entry point.
   */
  forceStick(opts?: { reason?: 'user' | 'restore' }): void;
  /**
   * Begin a thread-restore transaction: defensively escape (cancel any
   * spring, drop stale consent, flip to escaped so nothing auto-follows
   * the outgoing thread's geometry during the DOM flush), then arm a
   * one-shot consent for the next `forceStick({reason:'restore'})`.
   * Called by the thread-switch entry point (MessageTimeline's
   * `$effect.pre`, ChannelView's initial-poll path) BEFORE the DOM
   * update flushes; the restore $effect consumes the consent after.
   * The consent auto-clears on outer-scroll escape intent (wheel / key /
   * touch / pointer that can reach the chat scroller) and on any
   * user-reason `forceStick()` — the load-bearing distinguisher between
   * "the user is explicitly escaped" and "the entry point defensively
   * escaped while preparing the new thread for restore." The arm and the
   * consume are deliberately separate calls: the consent's whole job is
   * to span the flush window where a user gesture can land, so a single
   * merged restore call could not provide this protection.
   */
  armRestoreSnap(): void;
  /**
   * Flip intent flags to sticky-bottom WITHOUT writing scrollTop.
   * Use only when the caller has already established bottom geometry
   * or when the timeline is empty and there is no geometry to write.
   */
  markAtBottom(): void;
  /**
   * Set the escape flag. Public so `handleLoadOlder` / `scrollToItem`
   * can opt out of auto-restick on programmatic jumps.
   *
   * Calling with `next=true` also (a) cancels any in-flight spring
   * chase and (b) clears any pending `armRestoreSnap()` consent — a
   * fresh escape invalidates a yet-to-be-consumed restore-snap.
   * `armRestoreSnap()` itself runs its defensive escape through here
   * BEFORE arming, so its arm survives this clear while any stale
   * consent from an earlier path does not.
   *
   * Calling with `next=false` flips intent only — it does not consume
   * the restore-snap consent (that's `forceStick({reason:'restore'})`
   * or `markAtBottom()`'s job).
   */
  setEscapedFromLock(next: boolean): void;
  /**
   * Re-arm the warm-up gate WITHOUT writing scrollTop or changing
   * intent / escape flags. Sets `isWarm` to false and restarts the
   * QUIET_MS / FAILSAFE_MS timers, exactly as `attach()` and
   * restore-reason `forceStick()` do.
   *
   * The use case is a consumer that needs `isWarm` to be false BEFORE
   * the next DOM flush, where calling `forceStick()` would be wrong
   * because it has unwanted side effects (writes scrollTop, clears
   * escape). Chat's MessageTimeline calls this from `$effect.pre` on
   * thread switch: contentEl and scrollEl don't change across switches
   * (so `attach()` early-returns), and the restore-effect `forceStick()`
   * runs in `$effect` which fires AFTER the DOM update — meaning the
   * first paint of the new thread would otherwise inherit the previous
   * thread's settled `isWarm=true`, defeating the cascade-hide gate.
   */
  armWarmup(): void;
  /**
   * Force the warm-up gate open immediately. Use when the caller knows
   * there is no measurement cascade to hide — e.g. the placeholder →
   * materialized transition where the timeline was empty.
   */
  skipWarmup(): void;
  /**
   * Notify the controller that the consumer's `quietContextSignal`
   * changed — in either direction. On a truthy notify: no-op if already
   * warm or if no content delivery has been seen since the gate was
   * armed; otherwise, when engine-sourced settle evidence is already
   * held (window measured within epsilon — the priors-hit revisit), the
   * gate opens immediately ('settled'); when it isn't, the quiet timer
   * is (re)armed with the geometry-gated window — SETTLED_QUIET_MS once
   * the surface has held still, the conservative QUIET_MS while it is
   * still moving in large steps. On a falsy notify: an armed quiet
   * timer is DISARMED (presence-based signals go true→false when a
   * ChatMarkdown mounts after the timer armed — the settled-by-absence
   * license was withdrawn); with no timer armed it is a no-op.
   *
   * This is the seam for "I just learned async typesetting finished
   * mid-cascade" — the measurements-settle-first / signal-flips-later
   * ordering, where waiting out the original conservative window would
   * delay the reveal for no reason — and its inverse, "typesetting just
   * became possible after all".
   */
  notifyQuietContextSignalChanged(): void;
  /**
   * The timeline engine's compensation entry point. The engine never
   * writes scrollTop itself: TimelineVirtualizer forwards each
   * `EngineCompensation` observation here synchronously (same task as
   * the geometry change it compensates), the resolver's
   * `resolveEngineCompensation` decides, and any write goes through the
   * controller chokepoint (tagged like every controller write). Returns
   * true if a write landed; a decline needs no follow-up — the engine
   * re-reads the DOM offset from the next scroll event, so an unapplied
   * compensation cannot desync it.
   *
   * The consumer wires this directly:
   * `<TimelineVirtualizer onCompensation={stick.applyEngineCompensation}>`.
   */
  applyEngineCompensation(compensation: EngineCompensation): boolean;
  /**
   * Perform a virtualizer-requested imperative scroll (the scrollToIndex
   * convergence passes) through the controller chokepoint, so the write
   * is tagged programmatic and can never be classified as user scroll
   * intent. Intent semantics stay with the caller: flip
   * `setEscapedFromLock` / `markAtBottom` alongside the navigation this
   * write serves.
   *
   * Wired as
   * `<TimelineVirtualizer applyScrollTarget={stick.applyScrollTarget}>`.
   */
  applyScrollTarget(top: number): void;
  /**
   * External content-geometry entry (`externalContentGeometry` option):
   * one engine-sourced sample into the same delivery pipeline a contentEl
   * ResizeObserver fire takes — same observation shape, delivered by the
   * virtualizer post-flush (DOM consistent, pre-paint) so the pipeline's
   * geometry reads are live, one frame earlier than the RO and without
   * re-observing an element whose height the engine just wrote. The
   * sample's settle evidence additionally lets the warm gate reveal a
   * priors-hit revisit immediately (see observers.ts).
   *
   * Wired as
   * `<TimelineVirtualizer onContentGeometry={stick.deliverContentGeometry}>`.
   */
  deliverContentGeometry(sample: ContentGeometrySample): void;
}

export interface UseStickToBottomOptions {
  /**
   * Picks animation behavior for autonomous content growth (contentRO
   * positive deltas). Called per-fire — return 'spring' to make the
   * delta spring-eligible, 'instant' to sync-pin. A one-shot
   * markStructuralContentPending() call can make the next near-term
   * structural append spring-eligible even while this returns 'instant'.
   * Width-driven layout correction still sync-pins even when this returns
   * 'spring'.
   * Defaults to () => 'instant' (sync-pin everywhere) so existing
   * callers behave identically to the pre-spring-restoration controller.
   *
   * Chat's MessageTimeline wires this to a content-keyed latch
   * (`latchedSpringMode(performance.now(), pane.lastLiveContentAt,
   * SPRING_MODE_HOLD_MS)`) so streaming chunks — and the end-of-turn
   * drain after the turn signal clears — animate; idle/settled threads,
   * width reflow corrections, and Discussion's polled channel surface
   * stay on sync-pin.
   */
  animationMode?: () => 'spring' | 'instant';
  /**
   * Optional consumer-supplied signal that the visible
   * async-typesetting context has settled (e.g. all currently-mounted
   * svelte-streamdown instances have signaled `onsettled` since the
   * warm gate was last armed). When truthy at the moment
   * `bumpQuietTimer` fires, the warm-gate quiet window is shortened
   * from QUIET_MS to SETTLED_QUIET_MS — we trust there is no late
   * typesetting wave still in flight that could land an RO event after
   * the gate lifts and produce a one-frame flicker. When falsy, the
   * conservative QUIET_MS is preserved.
   *
   * Read per-fire — this is not a subscription. To shorten an existing
   * timer mid-flight (signal flipped truthy AFTER a 100ms bump), the
   * consumer should call `notifyQuietContextSignalChanged()`.
   *
   * Defaults to undefined (preserves existing QUIET_MS behavior for
   * surfaces that have no async-typesetting signal — Discussion's
   * ChannelView is the canonical example).
   */
  quietContextSignal?: () => boolean;
  /**
   * When true, the controller creates NO contentEl ResizeObserver: the
   * consumer's virtualizer is the content-geometry source and delivers
   * `ContentGeometrySample`s through `deliverContentGeometry`. Chat's
   * MessageTimeline sets this — the engine's spacer height IS the content
   * height, so a second observer on the same element would just re-read
   * layout one frame later. Surfaces without a virtualizer (ChannelView)
   * leave it unset and keep the RO-backed source.
   *
   * The option and `deliverContentGeometry` come as a pair:
   * delivering a sample without the option throws (the RO would
   * double-report every height change).
   */
  externalContentGeometry?: boolean;
  /**
   * How long the content layer-promotion lease outlives the last scroll
   * activity (scroll event or spring tick) before the controller demotes
   * contentEl back to unpromoted (`will-change` cleared). See the
   * "Content layer-promotion lease" section in index.svelte.ts for the
   * memory economics. Override is a tuning/test seam; the default is
   * CONTENT_LEASE_RELEASE_MS.
   */
  contentLeaseReleaseMs?: number;
}
