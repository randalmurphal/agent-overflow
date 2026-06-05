// Sticky-bottom controller, shared by chat MessageTimeline and
// Discussion ChannelView.
//
// Port of stackblitz-labs/use-stick-to-bottom adapted to Svelte 5. Owns
// the user's intent ("glued to bottom" or "free") and a single
// ResizeObserver on the content element. Two animation behaviors for
// autonomous content growth, selected per-fire by the consumer via the
// `animationMode` option:
//
//   - 'instant' (default): sync-pin. The same paint frame where
//     contentEl grows also lands scrollTop at the new target, so the
//     user sees content arriving at the bottom with no perceptible
//     scroll motion. Used by Discussion's ChannelView and by chat
//     whenever no live content has advanced recently — late Streamdown
//     typesetting on settled content, virtua row remeasurement on a
//     freshly-mounted thread, etc.
//
//   - 'spring': velocity-spring chase. The viewport interpolates toward
//     the moving bottom across rAF ticks so the user sees a smooth
//     scroll-follow. Chat MessageTimeline selects it via a content-keyed
//     latch (`latchedSpringMode` over `pane.lastLiveContentAt`), so
//     streaming chunks — and the end-of-turn drain after the turn signal
//     clears — flow in with a smooth animation. Gated by a quiescence-based warm
//     state: spring stays off until contentRO has been quiet for
//     QUIET_MS or the FAILSAFE_MS deadline trips, whichever comes
//     first. The warm gate defends against the original 80LoC-spring-
//     delete regression (commit e00723f) where mount-time virtua
//     remeasurement and async Streamdown typesetting would spring-
//     chase a thread restore visibly.
//
// User-initiated snaps (the scroll-to-bottom chip) and thread restores
// go through `forceStick()` which writes scrollTop directly.
// Restore snaps also reset the warm gate so post-thread-switch
// measurement settling stays silent; user snaps keep already-settled
// content visible.
//
// Unlike the previous controller, this owns the scroll element directly.
// MessageTimeline pairs it with virtua's <Virtualizer scrollRef={scrollEl}>
// so virtua does its measurement work without owning the scroll container.
// ChannelView is virtua-free — the contentEl is just a `<div>` wrapping
// the `{#each}` over channel messages — and the same controller works
// because the algorithm is agnostic to what's inside contentEl.
//
// External consumers (sidebar resizers, ChatView composer-height
// publication, scrollLeaseDuringTransition helper) speak to this through
// the PaneScrollController interface — pauseAutoScroll() returns a
// depth-counted lease, notifyContentMaybeGrew() handles geometry changes
// outside the content element. Both ChatView (composer overlay RO) and
// ChannelView (composer flex-section RO) call notifyContentMaybeGrew
// when their out-of-content height changes; the seam is identical on
// both surfaces.

import { tick } from 'svelte';
import { isUiRenderTraceEnabled, recordUiTrace } from './uiRenderTrace';

// Diagnostic trace helper — no-op in production (gated by
// `isUiRenderTraceEnabled` which only returns true in dev with
// `VITE_AGENT_OVERFLOW_UI_TRACE=1`, set by `make dev DEBUG=1`). The
// thunk skips object construction when disabled. Records flow into
// `${configDir}/ui-trace/ui-render.jsonl` via the same batched
// `AppendUIRenderTraceBatch` binding the timeline render trace uses.
//
// Call sites double-gate with `if (isUiRenderTraceEnabled()) trace(…)`
// because Rolldown inlines the gate but does not eliminate the closure
// allocation — the outer guard prevents ~120 closures/sec during
// spring animation in production builds.
function trace(label: string, build: () => Record<string, unknown>): void {
  if (!isUiRenderTraceEnabled()) return;
  recordUiTrace(label, build());
}

// Three-band geometry — see docs/architecture/frontend-scroll.md for
// the full rationale. Tightening any one of these affects a
// different UX surface; the asymmetry is load-bearing.
//
// Visual near-bottom band: drives the scroll-to-bottom chip and the
// negative-delta repin's geometric branch. Loose so a user within 70px
// doesn't see the chip flicker.
const STICK_TO_BOTTOM_OFFSET_PX = 70;
// Auto-follow re-stick band: a DOWN-direction scroll that lands within
// this many pixels of the bottom flips the user back to sticky. Matches
// react-virtuoso's `atBottomThreshold` default — tolerates virtua row-
// height estimation + browser scrollTop rounding that routinely lands
// 1-3px short during streaming.
const AUTO_FOLLOW_BOTTOM_EPSILON_PX = 4;
// ResizeObserver width jitter below half a CSS pixel is usually rounding
// noise. Wider changes mean the content column reflowed; any paired height
// delta is layout correction, not new live transcript content.
const CONTENT_REFLOW_WIDTH_EPSILON_PX = 0.5;
// Width and height can arrive in separate ResizeObserver deliveries. Keep
// the layout-correction classification alive briefly so a width-only fire
// followed by renderer height settle still sync-pins.
const CONTENT_REFLOW_SETTLE_WINDOW_MS = 250;
const RESIZE_CLEAR_PADDING_MS = 1;
const DEFAULT_PROGRAMMATIC_SCROLL_DURATION_MS = 420;
const PROGRAMMATIC_SCROLL_DISTANCE_THRESHOLD_PX = 1;
const RECENT_DOWN_INTENT_WINDOW_MS = 250;
const SCROLLBAR_DRAG_SESSION_FAILSAFE_MS = 30_000;
const EXTERNAL_SCROLL_TAG_CLEAR_MS = 100;

// ===== Spring chase tuning =====
// Defaults match upstream stackblitz-labs/use-stick-to-bottom:
// damping 0.7, stiffness 0.05, mass 1.25. These produce a smooth
// scroll-follow that feels responsive without overshooting visibly.
const DEFAULT_SPRING = { damping: 0.7, stiffness: 0.05, mass: 1.25 } as const;
const SIXTY_FPS_INTERVAL_MS = 1000 / 60;
// Keep chasing for this long after the last positive grow event. Without
// this, the spring would consider itself "arrived" between streaming
// chunks and stop, then have to spin up again on the next chunk —
// visibly jittery at chunk boundaries. Once this window expires AND
// animationMode is still 'spring', the spring enters sentinel mode
// (re-rAFs without writing, keeping springToken non-zero) so the
// external write gate and negative-delta carve-out stay engaged
// across gaps > 350ms (async shiki loads, parseIncompleteMarkdown
// rebalances). The sentinel cancels on the next tick where
// animationMode flips to 'instant' (no live content advanced within the
// consumer's hold window — see MessageTimeline's content-keyed latch).
//
// Exported so the colocated test can assert the load-bearing relationship
// SPRING_MODE_HOLD_MS > RETAIN_ANIMATION_DURATION_MS against the LIVE
// constant: the latch must keep reporting 'spring' for at least the full
// sentinel lifetime after the last content stamp, or the gate opens
// mid-sentinel and virtua's $fixScrollJump snaps scrollTop.
export const RETAIN_ANIMATION_DURATION_MS = 350;
// Spring arrival thresholds: distance ≤1px from target AND velocity
// below 0.5 px-per-60fps-frame means we've effectively settled.
const ARRIVAL_DISTANCE_PX = 1;
const ARRIVAL_VELOCITY_THRESHOLD = 0.5;
// Spring tick writes fire at 60Hz during a chase. Sample so the
// dev-only trace file isn't dominated by predictable +1px increments.
// 12 ≈ 5Hz, which is enough to see the spring is running without
// crowding the rare gesture/escape events that diagnose scroll
// regressions. First and last ticks of every chase are always
// recorded via the springTickSinceLastTrace reset at chase boundaries.
const SPRING_TICK_TRACE_SAMPLE = 12;
// Overshoot magnitude at which the contentRO overshoot guard snaps
// scrollTop instantly instead of letting the symmetric spring chase
// the lower target. Small overshoots (≤ this) come from transient
// streamdown re-renders — parseIncompleteMarkdown auto-balancing
// unclosed code fences / backticks / lists momentarily shrinks
// scrollHeight by a handful of pixels — and snapping for them produced
// the user-visible "viewport jumps upward then springs back" regression
// on plain-text streams. Large overshoots (virtua applyJump-style
// mis-corrections, content collapse) still snap instantly so the user
// doesn't watch the viewport drift down across many frames.
const SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX = 50;

// ===== Warm-up (quiescence) gate =====
// After attach() or forceStick(), the controller stays in sync-pin mode
// until contentRO has been quiet for QUIET_MS or the FAILSAFE_MS
// deadline trips. This defends against the mount-time spring-chase
// regression: virtua's per-row ResizeObservers and svelte-streamdown's
// async typesetting (shiki/KaTeX/mermaid) fire many positive deltas
// while content is settling, and we don't want those to look like
// "real" content arrival.
//
// QUIET_MS is shorter than a typical streaming chunk interval so we
// adapt to "fast settle, then real streaming starts" — chunks arrive
// after the quiet window closes, and spring engages.
//
// FAILSAFE_MS bounds the worst case: re-entering a thread that's
// already mid-stream produces continuous contentRO fires, so the
// quiet window never closes. Without the failsafe we'd be stuck in
// sync-pin for the rest of the turn. 2500ms covers slow machines
// where shiki/mermaid worker startup + virtua first-paint can
// genuinely take >1s, while still letting the spring engage for the
// bulk of a typical multi-second response.
const QUIET_MS = 100;
const FAILSAFE_MS = 2500;
// Shortened quiet window used when the consumer's `quietContextSignal`
// is truthy at bump time — that signal means "I have first-hand evidence
// the visible async typesetting (svelte-streamdown's
// shiki / katex / mermaid) is done." We still want ONE frame of
// contentRO silence after the last event so the post-typesetting layout
// has time to settle, but the conservative 100ms wait that defends
// against late typesetting is no longer needed. 16ms ≈ one rAF tick.
const SETTLED_QUIET_MS = 16;

const UP_KEYS: ReadonlySet<string> = new Set(['PageUp', 'ArrowUp', 'Home']);
const DOWN_KEYS: ReadonlySet<string> = new Set(['PageDown', 'ArrowDown', 'End']);

let mouseDown = false;
let listenersInstalled = false;

function installModuleSelectionListeners(): void {
  if (listenersInstalled) return;
  if (typeof document === 'undefined') return;
  listenersInstalled = true;
  document.addEventListener('mousedown', () => {
    mouseDown = true;
  }, { capture: true });
  document.addEventListener('mouseup', () => {
    mouseDown = false;
  }, { capture: true });
  document.addEventListener('click', () => {
    mouseDown = false;
  }, { capture: true });
}

/** Test-only escape hatch to reset the module-global mouseDown flag. */
export function resetUseStickToBottomModuleStateForTest(): void {
  mouseDown = false;
}

function isSelectingInside(scrollEl: HTMLElement): boolean {
  if (!mouseDown) return false;
  if (typeof window === 'undefined') return false;
  const sel = window.getSelection?.();
  if (!sel || sel.rangeCount === 0) return false;
  const range = sel.getRangeAt(0);
  // Match upstream: a selection counts if it crosses the scroll element
  // in either direction. Drag-select inside the timeline OR a selection
  // whose anchor sits in the timeline both pause auto-scroll.
  return (
    range.commonAncestorContainer.contains(scrollEl) ||
    scrollEl.contains(range.commonAncestorContainer)
  );
}

function nowMs(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

function roundCssPx(value: number): number {
  return Math.round(value * 100) / 100;
}

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
   * True once the warm-up gate has cleared — either QUIET_MS of
   * contentRO silence, or FAILSAFE_MS elapsed (whichever first). Use
   * as a "virtua's measurement cascade has settled" signal: consumers
   * can hide content during the cascade and reveal here to avoid
   * showing the user a brief estimated-size paint before the
   * measured-size correction lands. Reset to false on attach,
   * restore-reason forceStick, and explicit armWarmup() — the latter
   * is the seam for "I'm about to render fundamentally different
   * content (e.g. thread switch) and the DOM update will happen BEFORE
   * my next forceStick / attach call, so reset the gate now."
   */
  readonly isWarm: boolean;

  /** Depth-counted lease that suspends auto-scroll until released. */
  pauseAutoScroll(): () => void;
  /**
   * Notify the controller that the geometry around the content might
   * have changed without contentEl resizing — composer height growth/
   * shrink is the canonical case. Re-pins to the new target if sticky.
   */
  notifyContentMaybeGrew(): void;
  /**
   * Notify the controller that live transcript content may have advanced
   * without the content ResizeObserver producing a usable positive delta.
   * Uses the same escape/pause/user-intent gates as notifyContentMaybeGrew,
   * but honors animationMode: active chat turns spring-chase instead of
   * sync-pinning.
   */
  notifyLiveContentMaybeGrew(): void;
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
   * Arm a one-shot consent for the next `forceStick({reason:'restore'})`
   * call. Set by the thread-switch entry point (MessageTimeline's
   * `$effect.pre`, ChannelView's initial-poll path) immediately
   * before the restore $effect runs. Auto-clears on consume by the
   * next restore-forceStick call, on outer-scroll escape intent
   * (wheel / key / touch / pointer that can reach the chat scroller),
   * animateScrollTo, stopScroll, and on any
   * user-reason `forceStick()`. This is
   * the load-bearing distinguisher between "the user is explicitly
   * escaped" and "I just defensively set escape=true while preparing
   * the new thread for restore."
   */
  armRestoreSnap(): void;
  /**
   * Flip intent flags to sticky-bottom WITHOUT writing scrollTop.
   * Use only when the caller has already established bottom geometry
   * or when the timeline is empty and there is no geometry to write.
   */
  markAtBottom(): void;
  /**
   * Controlled non-native scroll animation for arbitrary timeline jumps.
   * This owns the scrollTop writes so programmatic scroll tagging stays
   * in one place. Used by handleLoadOlder / scrollToItem.
   */
  animateScrollTo(targetTop: number, opts?: { durationMs?: number }): Promise<'completed' | 'cancelled'>;
  /**
   * Run an external programmatic scroll, such as virtua's
   * `listRef.scrollToIndex(...)`, under the controller's scroll-intent
   * tag. This is the escape hatch for scroll writers the controller
   * cannot perform itself.
   *
   * Most external jumps are explicit navigation and should escape bottom
   * follow. Host-layout reconciliation is different: it rewrites the
   * virtualizer's current offset after a pane move without changing user
   * intent, so it passes `preserveIntent`.
   */
  runExternalScroll(action: () => void, opts?: { preserveIntent?: boolean }): void;
  /**
   * Cancel any active controller-owned animation and mark the user as
   * escaped. Kept for callers that need to stop motion without performing
   * a scroll write.
   */
  stopScroll(): void;
  /**
   * Set the escape flag. Public so `handleLoadOlder` / `scrollToItem`
   * can opt out of auto-restick on programmatic jumps.
   *
   * Calling with `next=true` also (a) cancels any in-flight spring
   * chase, (b) cancels any in-flight `animateScrollTo`, and (c) clears
   * any pending `armRestoreSnap()` consent — a fresh escape
   * invalidates a yet-to-be-consumed restore-snap. The thread-switch
   * entry point that legitimately wants the restore-snap calls
   * `armRestoreSnap()` AFTER its defensive `setEscapedFromLock(true)`
   * so the clear-then-set order still leaves the arm valid when the
   * restore `$effect` runs.
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
   * flipped truthy. If the warm gate is still pending and a quiet
   * timer is currently armed, re-arm with SETTLED_QUIET_MS instead of
   * letting the original (longer) timer run to completion. No-op if
   * already warm, no quiet timer is in flight, or the signal is still
   * falsy at notify time.
   *
   * This is the seam for "I just learned async typesetting finished
   * mid-cascade, please shorten the wait." Without it, a 100ms bump
   * would run to completion even though our visibility into the
   * cascade said we could lift in 16ms.
   */
  notifyQuietContextSignalChanged(): void;
}

export interface UseStickToBottomOptions {
  /**
   * Picks animation behavior for autonomous content growth (contentRO
   * positive deltas). Called per-fire — return 'spring' to make the
   * delta spring-eligible, 'instant' to sync-pin. Width-driven layout
   * correction still sync-pins even when this returns 'spring'.
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
}

export function createUseStickToBottomController(
  options: UseStickToBottomOptions = {},
): UseStickToBottomController {
  installModuleSelectionListeners();

  // ===== Reactive state (consumed by templates / $derived) =====
  // Intent flag: "we want to be glued to the bottom". Mirrors upstream's
  // state.isAtBottom — set true on initial mount, on forceStick, and when
  // a re-stick condition fires from the scroll handler. Set false on
  // explicit escape (outer wheel/key/touch scroll, select) and on stopScroll. Crucially
  // this is NOT geometry-derived; the contentRO sync-pin path relies on
  // it staying true even when content grew the bottom out from under us
  // — that's the gate that keeps the pin from running after the user
  // explicitly scrolled away.
  let isAtBottomState = $state(true);
  // Geometric ≤70px-from-bottom flag. Updated by refreshIsNearBottom on
  // every scroll event and after every programmatic write. The public
  // `isAtBottom` getter returns false while escaped even inside this
  // visual band so the ScrollToBottomButton reflects user intent.
  let isNearBottomState = $state(true);
  let escapedFromLockState = $state(false);
  let pauseDepth = $state(0);

  // ===== Internal bookkeeping (non-reactive) =====
  let scrollEl: HTMLElement | undefined;
  let contentEl: HTMLElement | undefined;
  let contentRO: ResizeObserver | undefined;
  let detachWheel: (() => void) | undefined;
  let detachScroll: (() => void) | undefined;
  let detachPointer: (() => void) | undefined;
  let detachKeyTouch: (() => void) | undefined;

  let targetAnimationFrame: number | null = null;
  let targetAnimationResolve: ((result: 'completed' | 'cancelled') => void) | null = null;
  let restoreTargetScrollBehavior: (() => void) | null = null;
  let resizeDifference = 0;
  let resizeClearTimer: ReturnType<typeof setTimeout> | null = null;
  let ignoreScrollToTop = -1;
  let externalScrollIgnoreUntil = 0;
  let externalScrollClearTimer: ReturnType<typeof setTimeout> | null = null;

  // ===== External scrollTop write gate =====
  // virtua's `$fixScrollJump` writes `scrollTop` DIRECTLY when a row's
  // ResizeObserver fires for an above-viewport / viewport-spanning row
  // (virtua@0.49.1 core/index.js:259-266, called from the J:m hook).
  // That's an entirely separate writer from `listRef.scrollToIndex(...)`
  // — it fires during streaming when the assistant row grows past the
  // viewport top, with no `runExternalScroll` wrapper to tag it. The
  // resulting `scrollTop` jump (typically one line) pre-empts the
  // spring chase: the next spring tick reads the bumped `scrollTop`
  // as `current`, sees `diff === 0`, and arrives without animating —
  // user sees a 1–2 line snap mid-stream where the spring should have
  // chased smoothly. Captured live in a bookmarked trace (24 untagged
  // jumps in a single long-stream session; the spring.arrive at 29360
  // → untagged scroll at 29387 sequence is the canonical reproducer).
  //
  // Defense: own the scrollTop property descriptor on `scrollEl` while
  // attached. The controller's writes route through `writeProgrammatic
  // ScrollTop` which flips `controllerOwnsScrollTopWrite` to bypass the
  // gate. External writes (virtua, future libs, browser auto-anchor)
  // are evaluated against the controller's intent: when the user is
  // sticking-and-engaged AND the warm gate has cleared AND a spring
  // chase is currently in flight (`springToken !== 0`), drop them so
  // the spring stays the single writer for the duration of the chase.
  // Otherwise pass through:
  //   - user is reading mid-thread (escaped), auto-follow is paused, or
  //     the warm-up cascade is still running (attach + restore both
  //     reset `warm`) — above-viewport visual stability matters and
  //     virtua's mount-cascade `$fixScrollJump` compensation must land;
  //   - `animationMode === 'instant'` — the controller would sync-pin
  //     to the same target virtua is writing, so racing them produces a
  //     mis-pinned frame; letting virtua's write land first arrives at
  //     the right place in the same paint;
  //   - no spring is in flight (`springToken === 0`) — the gate's sole
  //     purpose is keeping the spring as single writer during a chase;
  //     with no chase, suppression has nothing to protect and just
  //     desynchronizes virtua's internal scrollOffset from DOM
  //     scrollTop (bug-report-20260524T200233Z: thread-switch flicker
  //     on actively-streaming threads — 15/15 suppressions at
  //     springToken=0 produced the visible mis-pinned frame).
  // `externalScrollIgnoreUntil` (set by `runExternalScroll`) also
  // passes through so explicit programmatic scrolls — load-older anchor
  // restore, search-hit scrollToIndex — are honored even while sticking.
  let savedScrollTopDescriptor: PropertyDescriptor | undefined;
  let savedScrollTopWasOwn = false;
  let externalScrollTopGateInstalled = false;
  let controllerOwnsScrollTopWrite = false;
  let recentDownIntentUntil = 0;
  let recentDownIntentVersion = 0;
  let recentDownIntentClearTimer: ReturnType<typeof setTimeout> | null = null;
  let scrollbarDragSessionActive = false;
  let scrollbarDragSessionVersion = 0;
  let scrollbarDragSessionFailsafeTimer: ReturnType<typeof setTimeout> | null = null;
  let detachScrollbarDragEnd: (() => void) | undefined;
  let previousHeight: number | undefined;
  let previousWidth: number | undefined;
  let contentReflowSettleUntil = 0;
  let touchStartY: number | null = null;
  let resizeCorrelatedUntaggedScrollBudget = 0;
  // Controller-scope baseline so `scrolledDown` can be computed for the
  // re-stick path. Re-stick requires both recent down input and a real
  // downward scroll event landing at the bottom; this baseline keeps
  // layout clamps from masquerading as user-driven down motion.
  let lastObservedScrollTopForRestick = -1;

  // ===== Spring chase state =====
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
  // The target when the sentinel first entered after a chase. When the
  // sentinel tick sees diff > 0 but target === sentinelEntryTarget, the
  // content oscillated and returned to the same height — snap instantly.
  // -1 means not in sentinel. Only set on the FIRST sentinel entry
  // after a chase (not on re-entry), so the value reflects the target
  // at the moment the spring settled. Cleared on cancelSpring() and
  // when the spring exits sentinel with a different target.
  let sentinelEntryTarget = -1;

  // ===== Restore-snap consent state =====
  // One-shot flag the thread-switch entry point arms immediately before
  // the restore $effect runs (after the defensive `setEscapedFromLock`).
  // `forceStick({reason: 'restore'})` consumes the flag and proceeds;
  // when the flag is unset, that call NO-OPs. Any outer-scroll escape
  // intent (wheel / key / touch / pointer that can reach the scroll element), plus
  // selection, animateScrollTo, stopScroll, or explicit user-reason forceStick,
  // also clears the flag, so a stale restore $effect that fires after
  // a user escape cannot clobber it. This is
  // the load-bearing distinguisher between "the user has explicitly
  // escaped" and "the $effect.pre just defensively set escape=true
  // while preparing the new thread for restore."
  let restoreSnapArmed = false;

  // ===== Warm-up (quiescence) state =====
  // `warm` flips true once the controller observes a quiet period of
  // QUIET_MS on contentRO, OR the FAILSAFE_MS deadline trips (whichever
  // comes first). Reset to false on attach, explicit armWarmup(), and
  // restore-reason forceStick. Backed by $state so consumers can
  // subscribe to the transition — chat's MessageTimeline hides contentEl
  // while warm is false, which is the canonical
  // "virtua's measurement cascade has settled" signal. Without this,
  // an uncached thread's first paint renders rows at virtua's
  // ESTIMATED_ROW_SIZE × N offsets; the RO-correction pass then shifts
  // every row by a fraction-of-a-page (the larger the thread, the
  // bigger the shift) producing the visible "lands wrong, then jumps"
  // regression.
  let warm = $state(false);
  let quietTimer: ReturnType<typeof setTimeout> | null = null;
  let failsafeTimer: ReturnType<typeof setTimeout> | null = null;
  let hasFirstContentRO = false;

  function clearWarmupTimers(): void {
    if (quietTimer) {
      clearTimeout(quietTimer);
      quietTimer = null;
    }
    if (failsafeTimer) {
      clearTimeout(failsafeTimer);
      failsafeTimer = null;
    }
  }

  function markWarm(reason: 'quiet' | 'failsafe'): void {
    if (warm) return;
    warm = true;
    clearWarmupTimers();
    if (isUiRenderTraceEnabled()) trace(`scroll.warmup.${reason}`, () => ({
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
    }));
  }

  function beginWarmup(): void {
    clearWarmupTimers();
    warm = false;
    hasFirstContentRO = false;
    // ONLY arm the failsafe. The quiet timer is armed by `bumpQuietTimer`
    // on the FIRST contentRO event — gating the "quiet" signal on actual
    // RO evidence is what defends against the load-bearing case where
    // contentEl is absent when the gate is armed: a thread switch where
    // the slice fetch is in flight, MessageTimeline is rendering the
    // loading-spinner / empty branch, and there is no contentEl for the
    // RO to observe. If we armed quietTimer here, it would fire at
    // QUIET_MS without any cascade evidence; once items finally arrived
    // and contentEl mounted, the consumer's hide-gate (gated on isWarm)
    // would be already open and the cascade would be visible. The
    // failsafe still bounds the worst case (slow shiki / mermaid / KaTeX
    // typesetting that keeps ROs continuously firing for > FAILSAFE_MS).
    failsafeTimer = setTimeout(() => markWarm('failsafe'), FAILSAFE_MS);
  }

  function bumpQuietTimer(): void {
    if (warm) return;
    hasFirstContentRO = true;
    if (quietTimer) clearTimeout(quietTimer);
    if (!options.quietContextSignal) {
      quietTimer = setTimeout(() => markWarm('quiet'), QUIET_MS);
      return;
    }
    const settled = options.quietContextSignal();
    if (!settled) {
      quietTimer = null;
      return;
    }
    quietTimer = setTimeout(() => markWarm('quiet'), SETTLED_QUIET_MS);
  }

  function notifyQuietContextSignalChanged(): void {
    const settled = options.quietContextSignal?.() ?? false;
    const haveTimer = quietTimer !== null;
    let outcome: 'armed' | 'rearmed' | 'noop_warm' | 'noop_no_ro' | 'noop_signal_falsy';
    if (warm) outcome = 'noop_warm';
    else if (!settled) outcome = 'noop_signal_falsy';
    else if (!haveTimer && !hasFirstContentRO) outcome = 'noop_no_ro';
    else if (haveTimer) outcome = 'rearmed';
    else outcome = 'armed';
    if (isUiRenderTraceEnabled()) trace('scroll.warmup.signalChanged', () => ({
      outcome,
      settled,
      haveTimer,
      warm,
    }));
    if (outcome === 'noop_warm' || outcome === 'noop_no_ro' || outcome === 'noop_signal_falsy') return;
    if (quietTimer) clearTimeout(quietTimer);
    quietTimer = setTimeout(() => markWarm('quiet'), SETTLED_QUIET_MS);
  }

  // ===== Geometry =====
  function targetScrollTop(): number {
    if (!scrollEl) return 0;
    // Land at the actual bottom (scrollHeight - clientHeight). Upstream
    // use-stick-to-bottom subtracts an extra -1 px to avoid sub-pixel
    // rounding flipping their geometric isAtBottom check, but this
    // controller's public isAtBottom uses a 70 px visual band while
    // auto-follow re-stick uses a sub-pixel epsilon. Neither needs the
    // target itself to sit short of the actual browser bottom.
    // Subtracting -1 here just left the user 1 px above the actual
    // bottom; the scrollbar showed a one-tick gap and the snap felt
    // incomplete.
    return Math.max(0, scrollEl.scrollHeight - scrollEl.clientHeight);
  }
  function distanceFromBottom(): number {
    if (!scrollEl) return 0;
    return scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight;
  }
  function refreshIsNearBottom(): number {
    const dist = distanceFromBottom();
    const next = dist <= STICK_TO_BOTTOM_OFFSET_PX;
    if (next !== isNearBottomState) isNearBottomState = next;
    return dist;
  }

  function clearRecentDownIntent(opts: { bumpVersion?: boolean } = {}): void {
    recentDownIntentUntil = 0;
    if (opts.bumpVersion ?? true) recentDownIntentVersion += 1;
    if (recentDownIntentClearTimer) {
      clearTimeout(recentDownIntentClearTimer);
      recentDownIntentClearTimer = null;
    }
  }

  function hasRecentDownIntent(): boolean {
    return recentDownIntentUntil > nowMs();
  }

  function clearScrollbarDragSession(opts: { invalidateCapturedScrolls?: boolean } = {}): void {
    scrollbarDragSessionActive = false;
    if (opts.invalidateCapturedScrolls ?? true) scrollbarDragSessionVersion += 1;
    detachScrollbarDragEnd?.();
    detachScrollbarDragEnd = undefined;
    if (scrollbarDragSessionFailsafeTimer) {
      clearTimeout(scrollbarDragSessionFailsafeTimer);
      scrollbarDragSessionFailsafeTimer = null;
    }
  }

  function handleScrollbarDragEnd(): void {
    clearScrollbarDragSession({ invalidateCapturedScrolls: false });
  }

  function armScrollbarDragSession(): void {
    clearScrollbarDragSession({ invalidateCapturedScrolls: false });
    scrollbarDragSessionActive = true;
    scrollbarDragSessionVersion += 1;
    document.addEventListener('pointerup', handleScrollbarDragEnd, { capture: true });
    document.addEventListener('pointercancel', handleScrollbarDragEnd, { capture: true });
    window.addEventListener('blur', handleScrollbarDragEnd, { capture: true });
    detachScrollbarDragEnd = () => {
      document.removeEventListener('pointerup', handleScrollbarDragEnd, { capture: true });
      document.removeEventListener('pointercancel', handleScrollbarDragEnd, { capture: true });
      window.removeEventListener('blur', handleScrollbarDragEnd, { capture: true });
    };
    scrollbarDragSessionFailsafeTimer = setTimeout(
      handleScrollbarDragEnd,
      SCROLLBAR_DRAG_SESSION_FAILSAFE_MS,
    );
  }

  function restickFromUserInput(source: 'wheel' | 'key' | 'touch' | 'scroll'): void {
    clearRecentDownIntent({ bumpVersion: false });
    escapedFromLockState = false;
    isAtBottomState = true;
    springStopRequested = false;
    if (isUiRenderTraceEnabled()) trace('scroll.restick.input', () => ({
      source,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      distFromBottom: scrollEl ? Math.round(distanceFromBottom()) : null,
    }));
  }

  function recordRecentDownIntent(source: 'wheel' | 'key' | 'touch'): void {
    if (!escapedFromLockState) return;
    cancelTargetAnimation();
    restoreSnapArmed = false;
    recentDownIntentUntil = nowMs() + RECENT_DOWN_INTENT_WINDOW_MS;
    recentDownIntentVersion += 1;
    if (recentDownIntentClearTimer) clearTimeout(recentDownIntentClearTimer);
    const expiresAt = recentDownIntentUntil;
    const version = recentDownIntentVersion;
    recentDownIntentClearTimer = setTimeout(() => {
      if (
        recentDownIntentVersion === version
        && recentDownIntentUntil === expiresAt
      ) clearRecentDownIntent();
    }, RECENT_DOWN_INTENT_WINDOW_MS);
    if (isUiRenderTraceEnabled()) trace('scroll.intent.down', () => ({
      source,
      recentDownIntentUntil: Math.round(recentDownIntentUntil),
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      distFromBottom: scrollEl ? Math.round(distanceFromBottom()) : null,
      escapedFromLockState,
    }));
    if (
      scrollEl
      && escapedFromLockState
      && distanceFromBottom() <= AUTO_FOLLOW_BOTTOM_EPSILON_PX
    ) {
      restickFromUserInput(source);
    }
  }

  // ===== Programmatic scroll write =====
  // Diagnostic: `writeCaller` is set by the public-facing scrollTop
  // writer (forceStick / notifyContentMaybeGrew / contentRO /
  // animateScrollTo / overscroll-guard) before delegating to
  // `writeScrollTop` so the trace can attribute every write to its
  // origin. No semantic effect; production builds short-circuit at the
  // `isUiRenderTraceEnabled` check inside `trace()`.
  let writeCaller: string = 'unknown';
  // Spring-tick writes fire at 60Hz during a chase — predictable
  // increment-by-1px from the spring solver, and each record is
  // ~300 bytes. Without sampling, they were 5% of the 10 MB rotation
  // file and crowded out the rare gesture/escape events that actually
  // matter for diagnosing scroll regressions. We trace one in every
  // SPRING_TICK_TRACE_SAMPLE writes (~5Hz) plus the very first tick
  // of each chase via the `springTickSinceLastTrace` reset in
  // `startSpringIfNeeded`. Starts at `SAMPLE - 1` so the first write
  // is recorded (the gating predicate is `<`, so equal-or-greater
  // values record and reset).
  let springTickSinceLastTrace = SPRING_TICK_TRACE_SAMPLE - 1;
  function writeProgrammaticScrollTop(value: number): void {
    if (!scrollEl) return;
    // Determine whether this write will be traced BEFORE reading any
    // pre-write geometry. The sampling decision and the
    // isUiRenderTraceEnabled gate are both pure reads with no side
    // effects, so hoisting them above the write is safe. This lets us
    // skip the three layout reads (scrollTop, scrollHeight, clientHeight)
    // on the hot path — spring ticks fire at 60Hz and the trace is
    // sampled to ~5Hz, so ~92% of ticks skip the reads entirely.
    let recordTrace = true;
    if (writeCaller === 'spring.tick') {
      if (springTickSinceLastTrace < SPRING_TICK_TRACE_SAMPLE - 1) {
        springTickSinceLastTrace += 1;
        recordTrace = false;
      } else {
        springTickSinceLastTrace = 0;
      }
    }
    const shouldTrace = recordTrace && isUiRenderTraceEnabled();
    let beforeTop = 0, beforeHeight = 0, beforeClient = 0;
    if (shouldTrace) {
      beforeTop = scrollEl.scrollTop;
      beforeHeight = scrollEl.scrollHeight;
      beforeClient = scrollEl.clientHeight;
    }
    // Bypass the external-write gate for our own writes. The gate's
    // job is to filter writes from OTHER writers (virtua's
    // $fixScrollJump, browser auto-anchor); the controller is always
    // allowed to write through. Try/finally rather than a guard at
    // top of the gate's setter so the flag is cleared even if the
    // browser throws on a clamped write.
    controllerOwnsScrollTopWrite = true;
    try {
      scrollEl.scrollTop = value;
    } finally {
      controllerOwnsScrollTopWrite = false;
    }
    // Tag using the BROWSER-rounded read so the scroll handler's
    // `scrollTop === ignoreScrollToTop` check matches.
    ignoreScrollToTop = scrollEl.scrollTop;
    lastObservedScrollTopForRestick = scrollEl.scrollTop;
    refreshIsNearBottom();
    if (shouldTrace) {
      trace('scroll.write', () => ({
        caller: writeCaller,
        requested: Math.round(value),
        beforeTop: Math.round(beforeTop),
        afterTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: Math.round(beforeHeight),
        clientHeight: Math.round(beforeClient),
        maxTarget: Math.round(Math.max(0, beforeHeight - beforeClient)),
        ignoreScrollToTop,
        isAtBottomState,
        escapedFromLockState,
        pauseDepth,
        isNearBottomState,
      }));
    }
  }

  function writeScrollTop(value: number): void {
    if (!scrollEl) return;
    // Hot path: spring follow can call this every frame. The app contract is
    // that controller-owned scrollers do not get CSS-authored smooth scroll;
    // only inline values need temporary suppression here.
    const original = scrollEl.style.scrollBehavior;
    if (original && original !== 'auto') scrollEl.style.scrollBehavior = 'auto';
    writeProgrammaticScrollTop(value);
    if (original && original !== 'auto') scrollEl.style.scrollBehavior = original;
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

  // Cached MediaQueryList — `matchMedia('(prefers-reduced-motion: reduce)')`
  // is called inside both the contentRO positive-delta branch and the
  // spring tick (which runs at 60Hz). Parsing the query and constructing
  // a fresh `MediaQueryList` per call is wasted work; query the cached
  // list's `matches` instead. Cached lazily so SSR / non-window contexts
  // don't blow up.
  let reducedMotionQuery: MediaQueryList | null | undefined;
  function prefersReducedMotion(): boolean {
    if (reducedMotionQuery === undefined) {
      reducedMotionQuery = typeof window !== 'undefined'
        && typeof window.matchMedia === 'function'
        ? window.matchMedia('(prefers-reduced-motion: reduce)')
        : null;
    }
    return reducedMotionQuery?.matches ?? false;
  }

  function maxScrollTop(): number {
    if (!scrollEl) return 0;
    return Math.max(0, scrollEl.scrollHeight - scrollEl.clientHeight);
  }

  function clampScrollTop(value: number): number {
    return Math.max(0, Math.min(value, maxScrollTop()));
  }

  function easeOutCubic(t: number): number {
    const remaining = 1 - t;
    return 1 - remaining * remaining * remaining;
  }

  function finishTargetAnimation(result: 'completed' | 'cancelled'): void {
    if (targetAnimationFrame !== null) {
      cancelFrame(targetAnimationFrame);
      targetAnimationFrame = null;
    }
    restoreTargetScrollBehavior?.();
    restoreTargetScrollBehavior = null;
    const resolve = targetAnimationResolve;
    targetAnimationResolve = null;
    if (resolve) resolve(result);
  }

  function cancelTargetAnimation(): void {
    finishTargetAnimation('cancelled');
  }

  // ===== Spring chase =====
  function cancelSpring(): void {
    if (springFrameHandle !== null) {
      cancelFrame(springFrameHandle);
      springFrameHandle = null;
    }
    springToken = 0;
    velocity = 0;
    accumulated = 0;
    lastTickAt = null;
    // Reset the target-change timestamp so a stale value can't trick a
    // fresh chase into thinking it's within the retain window right out
    // of the gate (matches the historical 80LoC-spring cleanup semantics).
    lastTargetChangedAt = 0;
    sentinelEntryTarget = -1;
  }

  // Shared gate predicate. Used by both `startSpringIfNeeded` and the
  // contentRO positive-delta branch so the two sites can't drift on
  // which conditions allow the spring. The `warm` check is intentionally
  // omitted here — startSpringIfNeeded is called from inside the
  // already-warm branch of contentRO; warm-checking inside it would
  // double-gate and confuse the read.
  function springGateOpen(): boolean {
    return !springStopRequested
      && pauseDepth === 0
      && isAtBottomState
      && !escapedFromLockState
      && !prefersReducedMotion()
      && options.animationMode?.() === 'spring';
  }

  function startSpringIfNeeded(): void {
    if (springToken !== 0) return;
    if (!springGateOpen()) return;
    const myToken = ++springGen;
    springToken = myToken;
    lastTickAt = null;
    // Force the first tick of this chase to record so the trace shows
    // every chase boundary, not just every ~12th sampled write.
    springTickSinceLastTrace = SPRING_TICK_TRACE_SAMPLE - 1;

    const tick = (now: number): void => {
      springFrameHandle = null;
      if (springToken !== myToken || !scrollEl) return;
      // Bail conditions: lease acquired, escape set, or stop requested.
      // All three are handled by `cancelSpring()` cleanup at exit.
      if (springStopRequested || pauseDepth > 0 || !isAtBottomState || escapedFromLockState) {
        cancelSpring();
        return;
      }
      if (isSelectingInside(scrollEl)) {
        // Selection drag should never fight the user — re-rAF without
        // advancing scrollTop so the spring effectively pauses.
        springFrameHandle = requestFrame(tick);
        return;
      }

      const dt = lastTickAt === null ? 1 : (now - lastTickAt) / SIXTY_FPS_INTERVAL_MS;
      lastTickAt = now;

      // Cache per-tick. `targetScrollTop()` reads `scrollHeight` /
      // `clientHeight` — both force layout. Compute once per frame.
      const target = targetScrollTop();
      const current = scrollEl.scrollTop;
      const diff = target - current;

      if (diff !== 0) {
        // Content oscillation guard: if the sentinel was idle
        // (sentinelEntryTarget set) and the target returned to the
        // sentinel entry value, the content layer oscillated in
        // height (-N then +N from async Streamdown typesetting /
        // virtua row remount). The browser auto-clamped scrollTop
        // during the low point (native engine operation that
        // bypasses the property-descriptor gate), stranding scrollTop
        // below the restored target. Snap back instantly — a spring
        // chase for zero net content change is a visible artifact.
        if (sentinelEntryTarget >= 0 && target === sentinelEntryTarget) {
          writeCaller = 'spring.oscillationSnap';
          writeScrollTop(target);
          velocity = 0;
          accumulated = 0;
          sentinelEntryTarget = -1;
        } else {
          sentinelEntryTarget = -1;
          velocity = (DEFAULT_SPRING.damping * velocity + DEFAULT_SPRING.stiffness * diff) / DEFAULT_SPRING.mass;
          accumulated += velocity * dt;
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
          writeCaller = crossedTarget ? 'spring.overshoot' : 'spring.tick';
          writeScrollTop(clamped);
          if (scrollEl.scrollTop !== current) accumulated = 0;
        }
      } else {
        // Nothing to chase — zero residual velocity so the arrival
        // check can pass. Without this, an external instant-pin
        // (notifyContentMaybeGrew) or cross-target clamp landing at
        // exactly the target freezes velocity at its mid-chase value;
        // the arrival check (|velocity| < 0.5) never passes and the
        // spring ticks at 60fps forever without writing.
        velocity = 0;
        accumulated = 0;
      }

      // Arrival check uses the cached `target` for the position
      // comparison; the time delta uses rAF's `now` (matches
      // `nowMs()` in test environments because `performance.now` is
      // mocked to read the same source rAF passes the callback).
      // Mode flip mid-flight (turn ended) or RETAIN_ANIMATION_DURATION_MS
      // elapsing without another target-change event makes
      // `withinTargetChangeRetainWindow` false, so the spring lands on
      // its next arrival check rather than chasing forever. Bidirectional
      // — applies to downward chases (shrinks) as well as upward (growth).
      const wantsSpringNow = options.animationMode?.() === 'spring';
      const withinTargetChangeRetainWindow = wantsSpringNow && now - lastTargetChangedAt < RETAIN_ANIMATION_DURATION_MS;
      const arrived =
        Math.abs(scrollEl.scrollTop - target) < ARRIVAL_DISTANCE_PX
        && Math.abs(velocity) < ARRIVAL_VELOCITY_THRESHOLD;
      if (arrived && !withinTargetChangeRetainWindow) {
        if (wantsSpringNow) {
          // Streaming active but no target change within the retain
          // window (async shiki load, inter-chunk gap, parseIncomplete
          // Markdown rebalance). Keep the spring sentinel-alive so the
          // external write gate (springToken !== 0 suppression) and the
          // negative-delta carve-out stay engaged. Without this,
          // cancelSpring sets springToken=0 and the dead window lets
          // virtua $fixScrollJump or a negative contentRO sync-pin snap
          // scrollTop — visible as 1-2 lines of instant jump mid-stream.
          // The next positive contentRO delta bumps lastTargetChangedAt
          // and the chase resumes on the following tick.
          //
          // Snap pixel-perfect on sentinel entry (diff non-zero means
          // the spring converged within 1px but didn't land exactly);
          // subsequent sentinel ticks see diff===0 and skip the write,
          // avoiding trace noise. Zeroing velocity/accumulated keeps the
          // arrival check stable across sentinel ticks.
          if (diff !== 0) {
            writeCaller = 'spring.arrive';
            writeScrollTop(target);
          }
          velocity = 0;
          accumulated = 0;
          if (sentinelEntryTarget < 0) {
            sentinelEntryTarget = target;
          }
          springFrameHandle = requestFrame(tick);
          return;
        }
        // Snap to the exact target on arrival so the final paint lands
        // pixel-perfect rather than 0.5px above the bottom.
        writeCaller = 'spring.arrive';
        writeScrollTop(target);
        cancelSpring();
        return;
      }
      springFrameHandle = requestFrame(tick);
    };
    springFrameHandle = requestFrame(tick);
  }

  // ===== Content RO =====
  function setupContentRO(): void {
    if (!contentEl) return;
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (!entry || !contentEl || !scrollEl) return;
      const nextHeight = entry.contentRect.height;
      const nextWidth = entry.contentRect.width;
      const prev = previousHeight;
      const prevWidth = previousWidth;
      previousHeight = nextHeight;
      previousWidth = nextWidth;
      const widthChanged = prevWidth !== undefined
        && Math.abs(nextWidth - prevWidth) > CONTENT_REFLOW_WIDTH_EPSILON_PX;
      if (widthChanged) {
        contentReflowSettleUntil = nowMs() + CONTENT_REFLOW_SETTLE_WINDOW_MS;
      }

      // Every RO activity counts as "still settling" — reset the quiet
      // timer regardless of delta direction. virtua's per-row
      // remeasurement, Streamdown's typesetting backfill, and
      // parseIncompleteMarkdown rebalance all fire multiple RO callbacks
      // in close succession during mount; we want warm to fire only
      // once they're done.
      bumpQuietTimer();

      if (prev === undefined) {
        // First fire: snap to bottom synchronously so the initial paint
        // lands at the right place. Matches upstream's `initial` behavior
        // when isAtBottom starts true.
        const willPin = isAtBottomState && !escapedFromLockState;
        if (isUiRenderTraceEnabled()) trace('scroll.contentRO.firstFire', () => ({
          nextHeight: Math.round(nextHeight),
          willPin,
          isAtBottomState,
          escapedFromLockState,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
          scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
          clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        }));
        if (willPin) {
          writeCaller = 'contentRO.firstFire';
          writeScrollTop(targetScrollTop());
        }
        refreshIsNearBottom();
        return;
      }

      const delta = nextHeight - prev;
      const widthReflowActive = widthChanged || contentReflowSettleUntil > nowMs();
      // Common case: virtua re-measures a same-height row, padding-bottom
      // CSS variable updates with identical computed value, etc. No
      // geometry change → nothing to chase, no scroll-event tagging needed.
      if (delta === 0) {
        if (widthChanged && isUiRenderTraceEnabled()) trace('scroll.contentRO.widthReflow', () => ({
          prevWidth: prevWidth === undefined ? null : roundCssPx(prevWidth),
          nextWidth: roundCssPx(nextWidth),
          widthDelta: prevWidth === undefined ? null : roundCssPx(nextWidth - prevWidth),
          widthReflowActive,
          settleWindowMs: CONTENT_REFLOW_SETTLE_WINDOW_MS,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
          scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
          clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        }));
        return;
      }
      resizeDifference = delta;
      // Virtua can emit one untagged scroll jump as part of the same
      // measurement correction that produced this content resize. The
      // timer/rAF `resizeDifference` guard catches the normal ordering;
      // this one-event budget covers environments where the clear races
      // ahead of the scroll handler. Pending user intent still wins.
      resizeCorrelatedUntaggedScrollBudget = 1;

      // Refresh the geometric near-bottom flag BEFORE computing any
      // pin predicate so the trace and the gate both see the same
      // post-resize geometry. Without the lift, the negative branch
      // had to mirror the geometric check via an IIFE to avoid the
      // refresh side effect; with the lift, the trace and gate read
      // the same `isNearBottomState` and the IIFE disappears.
      refreshIsNearBottom();
      const overshootMagnitude = Math.max(0, scrollEl.scrollTop - targetScrollTop());
      const overshoot = overshootMagnitude > 0;
      const positiveWillPin = delta > 0
        && isAtBottomState
        && !escapedFromLockState
        && pauseDepth === 0;
      const negativeWillPin = delta < 0
        && (isAtBottomState || isNearBottomState)
        && !escapedFromLockState
        && pauseDepth === 0;
      if (isUiRenderTraceEnabled()) trace('scroll.contentRO', () => ({
        prev: Math.round(prev),
        next: Math.round(nextHeight),
        delta: Math.round(delta),
        overshoot,
        overshootMagnitude: Math.round(overshootMagnitude),
        positiveWillPin,
        negativeWillPin,
        isAtBottomState,
        escapedFromLockState,
        pauseDepth,
        isNearBottomState,
        prevWidth: prevWidth === undefined ? null : roundCssPx(prevWidth),
        nextWidth: roundCssPx(nextWidth),
        widthDelta: prevWidth === undefined ? null : roundCssPx(nextWidth - prevWidth),
        widthChanged,
        widthReflowActive,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        target: scrollEl ? Math.round(targetScrollTop()) : null,
      }));

      // Overscroll guard: if browser auto-clamping or virtua corrections
      // pushed us past the target, snap back. Two clauses past the
      // escape / pause / overshoot gates:
      //
      // 1. No spring is in flight (`springToken === 0`): any overshoot
      //    snaps. There is no other writer that will absorb it. This is
      //    the original Bug-A defense for virtua applyJump landing past
      //    the bottom while the cascade is still settling — the warm
      //    gate keeps the spring suppressed, so this branch is always
      //    the one reached during the cascade.
      // 2. Spring is in flight AND magnitude exceeds the threshold:
      //    snap. A large overshoot absorbed by the spring is fatal to
      //    follow UX (the user watches the viewport drift down 100+ px
      //    across many frames). Snapping keeps the existing "negative
      //    delta mid-spring lets the spring converge" contract.
      //
      // Small overshoots during a spring chase (≤ threshold) fall
      // through both clauses and are absorbed by the symmetric spring:
      // it sees `diff < 0` and damps `current` down to `target` across
      // rAF ticks. This is what fixes the parseIncompleteMarkdown
      // jitter — token-close-then-reopen rebalances shrink scrollHeight
      // by a handful of pixels, the old unconditional guard snapped
      // scrollTop down inside the RO callback, the spring re-extended
      // back up on the very next tick, and the user saw a few-pixel
      // up-down oscillation per chunk.
      //
      // Escape and pause gates remain unchanged — the threshold is an
      // additional relaxation, not a replacement.
      // prefers-reduced-motion users still get the snap on small
      // overshoots: `springGateOpen()` returns false for them so
      // `springToken === 0` and clause 1 fires.
      if (
        overshoot
        && !escapedFromLockState
        && pauseDepth === 0
        && (springToken === 0 || overshootMagnitude > SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX)
      ) {
        writeCaller = 'contentRO.overshoot';
        writeScrollTop(targetScrollTop());
      }

      if (delta > 0) {
        // Positive delta: choose between sync-pin (default) and spring
        // chase (when the consumer signals "real content streaming and
        // the controller has warmed past mount settle").
        //
        // Sync-pin path: writes scrollTop in the same paint frame as
        // contentEl growth — no perceptible scroll motion, just content
        // arriving at the bottom. Used when animationMode is 'instant',
        // when we haven't warmed yet (mount-time virtua remeasurement +
        // Streamdown typesetting still settling), or when the user has
        // requested reduced motion.
        //
        // Spring path: starts a velocity-spring chase that interpolates
        // toward the moving bottom across rAF frames. The user sees the
        // viewport smoothly follow streaming content. Each subsequent
        // positive delta during the chase bumps `lastTargetChangedAt` so
        // the spring keeps chasing across chunk boundaries instead of
        // arriving-then-restarting (visibly jittery).
        //
        // Width reflow carve-out: Mermaid, KaTeX, Shiki, images, and
        // normal prose can all change height when the content column width
        // changes. If live content advanced in the last few hundred ms,
        // the animation latch still reports "spring", but this resize is
        // layout correction for already-rendered content. Width and height
        // can arrive in separate ResizeObserver deliveries, so the reflow
        // classification is held briefly after a width change. Sync-pin it
        // so a pane/sidebar/window reflow cannot produce a half-viewport
        // spring chase from a stale bottom.
        if (positiveWillPin) {
          if (warm && springGateOpen() && !widthReflowActive) {
            lastTargetChangedAt = nowMs();
            startSpringIfNeeded();
          } else {
            writeCaller = 'contentRO.positiveDelta';
            writeScrollTop(targetScrollTop());
          }
        }
      } else if (delta < 0) {
        // Negative delta: re-stick when the controller's intent is
        // "stay at bottom" — EITHER the logical flag (isAtBottomState)
        // OR the geometric near-bottom band (isNearBottomState) says
        // so. The geometric branch matches upstream's negative-resize
        // re-stick. The intent branch defends against virtua's jump
        // correction during the layout-measurement cascade: when
        // rows above the viewport remeasure, virtua may shift
        // scrollTop hundreds of pixels off the bottom and flip
        // isNearBottomState=false purely as a downstream effect of
        // layout — not user intent. Without the isAtBottomState
        // disjunct, the controller abandoned the pin in that case
        // and left the viewport stuck mid-cascade until the next
        // shrink happened to land scrollTop at the new bottom by
        // coincidence. User-visible as a "half-screen jump to
        // bottom" on heavy uncached threads — see
        // docs/architecture/frontend-scroll.md for the cascade pattern
        // this defends.
        if (negativeWillPin) {
          isAtBottomState = true;
          // Spring carve-out: suppress this sync write while a spring
          // is chasing (springToken !== 0) so virtua's +ESTIMATE /
          // -CORRECTION pair on row-append (e.g. +90 then -56 within
          // ~5ms) doesn't race the spring. Without it, the negative
          // write lands scrollTop at the corrected target before the
          // spring's first paint and the spring ticks against
          // current==target with no perceptible motion. The spring
          // reads targetScrollTop() each tick and absorbs the
          // corrected target naturally. Note: the overshoot guard
          // above is threshold-gated on the spring (`springToken === 0
          // OR overshootMagnitude > SPRING_OVERSHOOT_INSTANT_SNAP_
          // THRESHOLD_PX`). Large overshoots (e.g. the existing
          // "negative delta mid-spring lets the spring converge"
          // test's 200+px shrink) still snap inside the RO callback;
          // small overshoots (the parseIncompleteMarkdown token-
          // close-then-reopen regression — a few-pixel shrink) are
          // absorbed by the symmetric spring instead. For the +90 /
          // -56 estimate-correct pair the spring has barely moved by
          // the time the correction arrives, so overshoot is false
          // and the negative-delta gate is the only path that needed
          // suppression. Bug A defense (sync-pin running during the
          // !warm cascade) is preserved by warm-gate ordering: the
          // cascade fires while `!warm`, springGateOpen requires
          // `warm`, so springToken stays 0 during the cascade and
          // the sync-pin runs as before. See
          // docs/architecture/frontend-scroll.md.
          if (springToken === 0 || widthReflowActive) {
            writeCaller = widthReflowActive
              ? 'contentRO.negativeDeltaReflow'
              : 'contentRO.negativeDelta';
            writeScrollTop(targetScrollTop());
          } else {
            // Spring is the single writer mid-chase; sync write is
            // suppressed above. The target nonetheless moved (downward),
            // so bump the retain timestamp — otherwise a small negative
            // correction between chunks could let
            // `withinTargetChangeRetainWindow` lapse and the spring
            // would arrive-and-stop while a follow-up chunk was on its
            // way.
            lastTargetChangedAt = nowMs();
          }
        }
      }

      // Schedule resizeDifference clear AFTER the scroll handler's 1ms.
      // The scroll event fired by the layout change above must observe
      // resizeDifference !== 0 so it is treated as layout, not input.
      if (resizeClearTimer) clearTimeout(resizeClearTimer);
      const myDelta = delta;
      resizeClearTimer = setTimeout(() => {
        requestAnimationFrame(() => {
          if (resizeDifference === myDelta) {
            resizeDifference = 0;
            resizeCorrelatedUntaggedScrollBudget = 0;
          }
        });
      }, RESIZE_CLEAR_PADDING_MS);
    });
    ro.observe(contentEl);
    contentRO = ro;
  }

  // ===== Wheel handler =====
  function targetIsInsideScrollEl(e: Event): boolean {
    if (!scrollEl) return false;
    let cur: Element | null = e.target instanceof Element ? e.target : null;
    while (cur) {
      if (cur === scrollEl) return true;
      cur = cur.parentElement;
    }
    return false;
  }

  function escapeFromUserInput(source: 'wheel' | 'key' | 'touch' | 'pointer'): void {
    if (!scrollEl) return;
    if (source !== 'pointer') clearScrollbarDragSession();
    const targetScrollEl = scrollEl;
    if (!escapedFromLockState && isUiRenderTraceEnabled()) {
      trace('scroll.intent.escape', () => ({
        source,
        scrollTop: Math.round(targetScrollEl.scrollTop),
        isAtBottomState,
        escapedFromLockState,
      }));
    }
    setEscapedFromLock(true);
  }

  function handleWheel(e: WheelEvent): void {
    if (!scrollEl) return;
    if (e.ctrlKey) return;
    if (e.deltaY === 0) return;
    if (!targetIsInsideScrollEl(e)) return;
    if (scrollEl.scrollHeight <= scrollEl.clientHeight) return;

    if (e.deltaY < 0) {
      escapeFromUserInput('wheel');
      return;
    }

    recordRecentDownIntent('wheel');
  }

  function handlePointerDown(e: PointerEvent): void {
    if (!scrollEl) return;
    if (scrollEl.scrollHeight <= scrollEl.clientHeight) return;
    if (!targetIsInsideScrollEl(e)) return;

    if (e.button === 1) {
      escapeFromUserInput('pointer');
      return;
    }

    if (e.isPrimary === false) return;

    const scrollbarWidth = scrollEl.offsetWidth - scrollEl.clientWidth;
    if (scrollbarWidth <= 0) return;

    const rect = scrollEl.getBoundingClientRect();
    const style = window.getComputedStyle(scrollEl);
    const inRightGutter = e.clientX >= rect.right - scrollbarWidth;
    const inLeftGutter = style.direction === 'rtl' && e.clientX <= rect.left + scrollbarWidth;
    if (!inRightGutter && !inLeftGutter) return;

    if (isUiRenderTraceEnabled()) trace('scroll.pointer.intent', () => ({
      clientX: Math.round(e.clientX),
      scrollbarWidth: Math.round(scrollbarWidth),
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      isAtBottomState,
      escapedFromLockState,
    }));
    escapeFromUserInput('pointer');
    armScrollbarDragSession();
  }

  // ===== Scroll handler =====
  function handleScroll(): void {
    if (!scrollEl) return;
    const scrollTopAtEvent = scrollEl.scrollTop;
    // Bug A fix (Change 1): capture distance-from-bottom synchronously
    // at scroll-event time. The deferred re-stick check (below) must
    // use THIS value, not re-read distanceFromBottom() from inside the
    // 1ms timer — by the time the deferred handler runs, a concurrent
    // contentRO fire from a streaming chunk may have grown scrollHeight
    // and pushed the user past the re-stick epsilon, producing a false
    // negative (the user reached the bottom but the bottom moved away
    // in the 1ms window). This was Bug A in long Opus threads.
    // Capture and consume the programmatic-write tag synchronously so
    // it only suppresses ONE scroll event. Otherwise a later genuine
    // user scroll back to the same scrollTop value would be ignored.
    const tag = ignoreScrollToTop;
    ignoreScrollToTop = -1;
    const externalTagged = externalScrollIgnoreUntil > nowMs();
    const tagged = scrollTopAtEvent === tag || externalTagged;
    // Tagged programmatic write — bail synchronously without scheduling
    // the deferral timer. Steady-state streaming fires a sync-pin write
    // on every contentRO positive delta; allocating a closure + timer
    // registration for each one just to no-op inside the callback was
    // hundreds of throwaway allocs/sec on long assistant turns. The 1 ms
    // RO-race deferral below isn't needed for tagged writes — the tag is
    // set synchronously by writeScrollTop, so we already know this event
    // reflects our own write, not user intent. The refreshIsNearBottom
    // call and trace allocation below are also skipped on the tagged
    // path — the spring fires at 60Hz during a chase, and both are
    // wasted work when we already know this is our own write.
    if (tagged) return;
    const distFromBottomAtEvent = refreshIsNearBottom();
    if (isUiRenderTraceEnabled()) trace('scroll.scrollEvent', () => ({
      scrollTop: Math.round(scrollTopAtEvent),
      tag: Math.round(tag),
      externalTagged,
      tagged,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      resizeDifference: Math.round(resizeDifference),
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      isNearBottomState,
    }));
    const resizeCorrelatedScroll = resizeDifference !== 0 || resizeCorrelatedUntaggedScrollBudget > 0;
    if (resizeCorrelatedUntaggedScrollBudget > 0) resizeCorrelatedUntaggedScrollBudget -= 1;
    const previousObserved = lastObservedScrollTopForRestick;
    const downIntentVersionAtEvent = recentDownIntentVersion;
    const scrollbarDragSessionAtEvent = scrollbarDragSessionActive;
    const scrollbarDragSessionVersionAtEvent = scrollbarDragSessionVersion;
    const scrolledDown = previousObserved < 0
      ? false
      : scrollTopAtEvent > previousObserved;
    const scrolledUp = previousObserved < 0
      ? false
      : scrollTopAtEvent < previousObserved;
    lastObservedScrollTopForRestick = scrollTopAtEvent;
    const shouldRunDeferredScrollIntentCheck =
      escapedFromLockState
      || mouseDown
      || scrollbarDragSessionActive;
    if (!shouldRunDeferredScrollIntentCheck) return;
    cancelTargetAnimation();
    // Defer 1ms so a concurrent RO callback can update resizeDifference
    // before we interpret direction. Mirrors upstream.
    setTimeout(() => {
      if (!scrollEl) return;
      const eventCameFromCurrentScrollbarDrag =
        scrollbarDragSessionAtEvent
        && scrollbarDragSessionVersionAtEvent === scrollbarDragSessionVersion;
      if (eventCameFromCurrentScrollbarDrag && scrolledUp) {
        escapeFromUserInput('pointer');
      }

      // RO race — content just resized; the scroll event reflects layout,
      // not user intent. Most importantly: virtua's $fixScrollJump can
      // adjust scrollTop to keep above-viewport rows stable, which would
      // otherwise look like user scroll movement. For non-virtua consumers
      // (Discussion's ChannelView) this gate is a 1ms suppression window
      // after each content-RO fire — vanishingly unlikely to swallow a
      // real user gesture, since the window only opens immediately after
      // a layout change.
      //
      // A recent down-intent is proof the user is actively trying to
      // re-stick, so let that bottom-landing through even during a
      // measurement cascade. Without recent down intent, layout wins.
      const hasLiveRecentDownIntent =
        hasRecentDownIntent()
        && recentDownIntentVersion === downIntentVersionAtEvent;
      const hasLiveScrollbarDragRestickIntent =
        eventCameFromCurrentScrollbarDrag
        && scrolledDown;
      const hasLiveDownIntent = hasLiveRecentDownIntent || hasLiveScrollbarDragRestickIntent;
      if (resizeCorrelatedScroll && !hasLiveDownIntent) {
        if (isUiRenderTraceEnabled()) trace('scroll.scrollEvent.deferred.bailRO', () => ({
          resizeDifference: Math.round(resizeDifference),
          resizeCorrelatedScroll,
          hasLiveDownIntent,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        }));
        return;
      }

      if (isSelectingInside(scrollEl)) {
        if (isUiRenderTraceEnabled()) trace('scroll.scrollEvent.deferred.escapeSelection', () => ({
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        }));
        setEscapedFromLock(true);
        return;
      }

      const willRestick = hasLiveDownIntent
        && scrolledDown
        && escapedFromLockState
        && distFromBottomAtEvent <= AUTO_FOLLOW_BOTTOM_EPSILON_PX;
      if (isUiRenderTraceEnabled()) trace('scroll.scrollEvent.deferred', () => ({
        scrollTop: Math.round(scrollTopAtEvent),
        previousObserved: Math.round(previousObserved),
        scrolledDown,
        hasLiveRecentDownIntent,
        hasLiveScrollbarDragRestickIntent,
        hasLiveDownIntent,
        distFromBottomAtEvent: Math.round(distFromBottomAtEvent),
        distFromBottomNow: scrollEl ? Math.round(distanceFromBottom()) : null,
        willRestick,
        isAtBottomState,
        escapedFromLockState,
      }));
      if (willRestick) {
        restickFromUserInput('scroll');
      }
    }, RESIZE_CLEAR_PADDING_MS);
  }

  // ===== Keydown / touch handlers (intent signals) =====
  function handleKeydown(e: KeyboardEvent): void {
    if (UP_KEYS.has(e.key)) escapeFromUserInput('key');
    if (DOWN_KEYS.has(e.key)) recordRecentDownIntent('key');
  }
  function handleTouchStart(e: TouchEvent): void {
    touchStartY = e.touches[0]?.clientY ?? null;
  }
  function handleTouchMove(e: TouchEvent): void {
    if (touchStartY === null) return;
    const y = e.touches[0]?.clientY ?? touchStartY;
    const dy = y - touchStartY;
    touchStartY = y;
    // Finger moves DOWN visually → page scrolls UP (scrollTop decreases)
    // → user wants to see content above → escape (UP intent). Finger
    // moves UP while already escaped is the matching "scroll back toward
    // bottom" input signal (DOWN intent).
    if (dy > 1) escapeFromUserInput('touch');
    if (dy < -1) recordRecentDownIntent('touch');
  }
  function handleTouchEnd(): void {
    touchStartY = null;
  }

  // ===== Public actions =====
  function setEscapedFromLock(next: boolean): void {
    if (!next && !scrollbarDragSessionActive) clearScrollbarDragSession();
    if (next) {
      clearRecentDownIntent();
      cancelTargetAnimation();
      // User explicitly broke from auto-follow — bail any in-flight
      // spring chase. The tick observes springStopRequested + new
      // state and clears the token on the next frame, but we also
      // null it here for the "no rAF before next attach" edge case.
      springStopRequested = true;
      cancelSpring();
      // Clear any pending restore-snap consent: a fresh user escape
      // invalidates a yet-to-be-consumed restore-snap. The thread-
      // switch entry point that legitimately wants a restore-snap
      // calls `armRestoreSnap()` AFTER its defensive setEscape, so
      // this clear is the right default — a stale consent left over
      // from an earlier path can't slip through.
      restoreSnapArmed = false;
    }
    if (escapedFromLockState === next) return;
    const previousIsAtBottom = isAtBottomState;
    escapedFromLockState = next;
    if (next) {
      isAtBottomState = false;
    }
    if (isUiRenderTraceEnabled()) trace('scroll.escape.set', () => ({
      next,
      previousIsAtBottom,
      isAtBottomState,
      pauseDepth,
      isNearBottomState,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
    }));
  }

  function armRestoreSnap(): void {
    restoreSnapArmed = true;
    if (isUiRenderTraceEnabled()) trace('scroll.restoreSnap.arm', () => ({
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
    }));
  }

  function stopScroll(): void {
    clearRecentDownIntent();
    clearScrollbarDragSession();
    setEscapedFromLock(true);
  }

  // Tags a non-controller scroll writer so the scroll handler does not
  // misread it as user intent. This does not change follow/escape state;
  // callers decide that explicitly before invoking it.
  function tagExternalProgrammaticScroll(action: () => void): void {
    externalScrollIgnoreUntil = nowMs() + EXTERNAL_SCROLL_TAG_CLEAR_MS;
    if (externalScrollClearTimer) clearTimeout(externalScrollClearTimer);
    externalScrollClearTimer = setTimeout(() => {
      externalScrollIgnoreUntil = 0;
      externalScrollClearTimer = null;
    }, EXTERNAL_SCROLL_TAG_CLEAR_MS);
    action();
    if (scrollEl) {
      lastObservedScrollTopForRestick = scrollEl.scrollTop;
      refreshIsNearBottom();
    }
  }

  function runExternalScroll(action: () => void, opts: { preserveIntent?: boolean } = {}): void {
    if (!opts.preserveIntent) setEscapedFromLock(true);
    tagExternalProgrammaticScroll(action);
  }

  async function preserveScrollAnchor(
    anchor: HTMLElement,
    action: () => void | Promise<void>,
  ): Promise<void> {
    if (!scrollEl || !anchor.isConnected) {
      await action();
      return;
    }

    const targetScrollEl = scrollEl;
    const hadBottomFollowIntent = isAtBottomState && !escapedFromLockState;
    const release = pauseAutoScroll();
    let actionError: unknown;
    let actionPromise: Promise<void> | undefined;
    try {
      const beforeTop = hadBottomFollowIntent ? null : anchor.getBoundingClientRect().top;
      actionPromise = (async () => {
        await action();
      })().catch((err: unknown) => {
        actionError = err;
      });
      await tick();
      if (
        !hadBottomFollowIntent &&
        beforeTop !== null &&
        scrollEl === targetScrollEl &&
        anchor.isConnected
      ) {
        const afterTop = anchor.getBoundingClientRect().top;
        const delta = afterTop - beforeTop;
        if (Number.isFinite(delta) && Math.abs(delta) >= 0.5) {
          writeCaller = 'preserveScrollAnchor';
          writeScrollTop(targetScrollEl.scrollTop + delta);
        }
      }
    } finally {
      release();
    }
    await actionPromise;
    if (actionError !== undefined) throw actionError;
  }

  function animateScrollTo(
    rawTargetTop: number,
    opts?: { durationMs?: number },
  ): Promise<'completed' | 'cancelled'> {
    if (!scrollEl) return Promise.resolve('cancelled');
    const targetScrollEl = scrollEl;
    cancelTargetAnimation();

    const targetTop = clampScrollTop(rawTargetTop);
    const startTop = targetScrollEl.scrollTop;
    const distance = targetTop - startTop;
    if (Math.abs(distance) <= PROGRAMMATIC_SCROLL_DISTANCE_THRESHOLD_PX) {
      return Promise.resolve('completed');
    }
    setEscapedFromLock(true);
    const durationMs = opts?.durationMs ?? DEFAULT_PROGRAMMATIC_SCROLL_DURATION_MS;
    if (
      prefersReducedMotion()
      || durationMs <= 0
    ) {
      writeCaller = 'animateScrollTo.instant';
      writeScrollTop(targetTop);
      return Promise.resolve('completed');
    }

    return new Promise((resolve) => {
      targetAnimationResolve = resolve;
      const startedAt = nowMs();
      const originalInlineScrollBehavior = targetScrollEl.style.scrollBehavior;
      if (window.getComputedStyle(targetScrollEl).scrollBehavior !== 'auto') {
        targetScrollEl.style.scrollBehavior = 'auto';
        restoreTargetScrollBehavior = () => {
          targetScrollEl.style.scrollBehavior = originalInlineScrollBehavior;
        };
      }

      const tick = (now: number): void => {
        if (!scrollEl || targetAnimationResolve !== resolve) return;
        const elapsed = Math.max(0, now - startedAt);
        const progress = Math.min(1, elapsed / durationMs);
        const eased = easeOutCubic(progress);
        writeCaller = 'animateScrollTo.tick';
        writeProgrammaticScrollTop(startTop + distance * eased);
        if (progress >= 1 || Math.abs(scrollEl.scrollTop - targetTop) <= PROGRAMMATIC_SCROLL_DISTANCE_THRESHOLD_PX) {
          writeCaller = 'animateScrollTo.finish';
          writeProgrammaticScrollTop(targetTop);
          finishTargetAnimation('completed');
          return;
        }
        targetAnimationFrame = requestFrame(tick);
      };

      targetAnimationFrame = requestFrame(tick);
    });
  }

  function forceStick(opts: { reason?: 'user' | 'restore' } = {}): void {
    const reason = opts.reason ?? 'user';
    // Restore-reason consent gate: when called from a restore $effect
    // (thread switch, channel initial poll), only proceed if the
    // caller's entry point armed the consent first. This stops a
    // stale or duplicate restore from clobbering an existing user
    // escape with a snap-to-bottom they didn't ask for (the seq-509
    // trace bug — a `restoreToBottom()` firing 17s after the user
    // wheel-escaped, slamming them to the bottom and wiping escape).
    // User-reason calls always proceed (chip click / explicit
    // intent), and they ALSO consume any pending restore consent so
    // the next call doesn't see a stale arm.
    if (reason === 'restore' && !restoreSnapArmed) {
      if (isUiRenderTraceEnabled()) trace('scroll.forceStick.skipRestore', () => ({
        reason,
        restoreSnapArmed,
        isAtBottomState,
        escapedFromLockState,
        pauseDepth,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      }));
      return;
    }
    restoreSnapArmed = false;
    clearRecentDownIntent();
    clearScrollbarDragSession();
    if (isUiRenderTraceEnabled()) trace('scroll.forceStick.entry', () => ({
      reason,
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      target: scrollEl ? Math.round(targetScrollTop()) : null,
    }));
    setEscapedFromLock(false);
    cancelSpring();
    // Reset the stop flag AFTER cancel — cancelSpring observes
    // springStopRequested via the rAF tick guard, but the value at the
    // current synchronous frame doesn't affect cancellation (the token
    // mismatch on the next tick handles it). We clear it now so the
    // next streaming chunk can re-engage the spring.
    springStopRequested = false;
    // Only restore/thread-switch snaps should re-hide content for the
    // measurement warmup. A user click on the scroll-to-bottom chip is
    // an explicit visible action in an already-mounted thread; blanking
    // the transcript until the failsafe fires is worse than the small
    // chance of a post-snap measurement correction.
    if (reason === 'restore') beginWarmup();
    if (!scrollEl) return;
    isAtBottomState = true;
    writeCaller = 'forceStick';
    writeScrollTop(targetScrollTop());
  }

  function markAtBottom(): void {
    // Flag-only counterpart to forceStick: caller already established
    // bottom geometry, or there is no geometry yet because the
    // timeline is empty. Used by chat's restoreToBottom empty-timeline
    // branch — so the restore-snap consent must be consumed here too,
    // otherwise the arm leaks past a completed empty-thread restore
    // and admits a later stale restore-stick.
    if (isUiRenderTraceEnabled()) trace('scroll.markAtBottom', () => ({
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
    }));
    restoreSnapArmed = false;
    clearRecentDownIntent();
    clearScrollbarDragSession();
    setEscapedFromLock(false);
    isAtBottomState = true;
    refreshIsNearBottom();
  }

  function readNotifyContentGate(): {
    gateScrollEl: boolean;
    gateEscape: boolean;
    gatePause: boolean;
    gateNotAtBottom: boolean;
    canPin: boolean;
  } {
    const gateScrollEl = scrollEl !== undefined;
    const gateEscape = escapedFromLockState;
    const gatePause = pauseDepth > 0;
    const gateNotAtBottom = !isAtBottomState;
    const canPin = gateScrollEl
      && !gateEscape
      && !gatePause
      && !gateNotAtBottom;

    return {
      gateScrollEl,
      gateEscape,
      gatePause,
      gateNotAtBottom,
      canPin,
    };
  }

  function instantPinAfterExternalGeometryChange(caller: string): void {
    // Stamp resizeDifference BEFORE writing scrollTop so the resulting
    // scroll event is treated as RO-correlated, not user-driven. Without
    // this, a textarea-shrink could cause the scroll handler's re-stick
    // path to flip isAtBottom in a way that surprises the user.
    resizeDifference = 1;
    if (resizeClearTimer) clearTimeout(resizeClearTimer);
    resizeClearTimer = setTimeout(() => {
      requestAnimationFrame(() => {
        if (resizeDifference === 1) {
          resizeDifference = 0;
          resizeCorrelatedUntaggedScrollBudget = 0;
        }
      });
    }, RESIZE_CLEAR_PADDING_MS);
    writeCaller = caller;
    writeScrollTop(targetScrollTop());
  }

  function notifyContentMaybeGrew(): void {
    const gate = readNotifyContentGate();
    if (isUiRenderTraceEnabled()) trace('scroll.notifyContentMaybeGrew', () => ({
      willPin: gate.canPin,
      gateScrollEl: gate.gateScrollEl,
      gateEscape: gate.gateEscape,
      gatePause: gate.gatePause,
      gateNotAtBottom: gate.gateNotAtBottom,
      pauseDepth,
      isNearBottomState,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      target: scrollEl ? Math.round(targetScrollTop()) : null,
    }));
    if (!gate.canPin) return;
    instantPinAfterExternalGeometryChange('notifyContentMaybeGrew');
  }

  function notifyLiveContentMaybeGrew(): void {
    const gate = readNotifyContentGate();
    const willSpring = gate.canPin && warm && springGateOpen();
    if (isUiRenderTraceEnabled()) trace('scroll.notifyLiveContentMaybeGrew', () => ({
      canPin: gate.canPin,
      willSpring,
      gateScrollEl: gate.gateScrollEl,
      gateEscape: gate.gateEscape,
      gatePause: gate.gatePause,
      gateNotAtBottom: gate.gateNotAtBottom,
      pauseDepth,
      isNearBottomState,
      warm,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      target: scrollEl ? Math.round(targetScrollTop()) : null,
    }));
    if (!gate.canPin) return;

    const target = targetScrollTop();
    if (willSpring && scrollEl) {
      const current = scrollEl.scrollTop;
      if (current < target) {
        lastTargetChangedAt = nowMs();
        startSpringIfNeeded();
        return;
      }
      const overshootMagnitude = current - target;
      if (
        overshootMagnitude > 0
        && springToken !== 0
        && overshootMagnitude <= SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX
      ) {
        // Match contentRO's spring policy: a small corrected-target
        // overshoot while the spring is already chasing should damp
        // through the symmetric spring, not snap via the structural
        // nudge's instant fallback.
        lastTargetChangedAt = nowMs();
        return;
      }
    }

    // Same instant fallback as notifyContentMaybeGrew for non-spring
    // modes, warm-up, reduced-motion users, and no-distance/overshoot
    // nudges where a spring has nothing useful to chase.
    instantPinAfterExternalGeometryChange('notifyLiveContentMaybeGrew');
  }

  function pauseAutoScroll(): () => void {
    pauseDepth += 1;
    if (isUiRenderTraceEnabled()) trace('scroll.pause.acquire', () => ({
      pauseDepth,
      isAtBottomState,
      escapedFromLockState,
    }));
    let released = false;
    return () => {
      if (released) return;
      released = true;
      pauseDepth = Math.max(0, pauseDepth - 1);
      const willRepin = pauseDepth === 0
        && !escapedFromLockState
        && isAtBottomState;
      if (isUiRenderTraceEnabled()) trace('scroll.pause.release', () => ({
        pauseDepth,
        willRepin,
        isAtBottomState,
        escapedFromLockState,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        target: scrollEl ? Math.round(targetScrollTop()) : null,
      }));
      if (willRepin) {
        // Re-pin on lease release: layout-changing surfaces (sidebar
        // resize, terminal toggle) shrink/grow the chat column during
        // the lease; without this re-pin, sticky users drift.
        writeCaller = 'pauseAutoScroll.release';
        writeScrollTop(targetScrollTop());
      }
    };
  }

  // ===== External scrollTop write gate (install / uninstall) =====
  // See the state block earlier in the file for the motivating
  // regression. The gate is installed as a property descriptor on the
  // scroll element's `scrollTop` accessor; that captures BOTH the
  // production case (inherited from Element.prototype) and the test
  // case (where stubGeometry installs an own-property accessor to back
  // a plain Geometry object). The captured accessor is the "real"
  // setter; the gate's setter only decides whether to delegate.
  function installExternalScrollTopGate(el: HTMLElement): void {
    if (externalScrollTopGateInstalled) return;
    const own = Object.getOwnPropertyDescriptor(el, 'scrollTop');
    const inherited = own ? undefined : Object.getOwnPropertyDescriptor(Element.prototype, 'scrollTop');
    const captured = own ?? inherited;
    if (!captured || typeof captured.get !== 'function' || typeof captured.set !== 'function') {
      // Environments without an accessor (very old browsers, exotic
      // mocks) skip the gate. The controller still functions; it just
      // can't filter external writes.
      return;
    }
    savedScrollTopDescriptor = own;
    savedScrollTopWasOwn = own !== undefined;
    const origGet = captured.get;
    const origSet = captured.set;
    Object.defineProperty(el, 'scrollTop', {
      configurable: true,
      enumerable: captured.enumerable ?? true,
      get(): number {
        return origGet.call(el) as number;
      },
      set(value: number): void {
        // Controller-owned writes always pass through. The flag is set
        // by `writeProgrammaticScrollTop` around the actual assignment
        // and cleared in its finally block.
        if (controllerOwnsScrollTopWrite) {
          origSet.call(el, value);
          return;
        }
        // Explicit programmatic scrolls via `runExternalScroll` (load-
        // older anchor restore, search-hit `scrollToIndex`) tag a
        // short ignore window before the write fires. Pass through so
        // the user's intentional jump lands even while sticking.
        if (externalScrollIgnoreUntil > nowMs()) {
          origSet.call(el, value);
          return;
        }
        // Pre-warm: attach() and `forceStick({reason:'restore'})` both
        // call `beginWarmup()` so `warm===false` covers the mount-
        // cascade window (per-row ROs firing as virtua first mounts a
        // slice, and the post-restore measurement settle). virtua's
        // `$fixScrollJump` writes during that window are legitimate
        // compensation for above-viewport remeasurements — suppressing
        // them desynchronizes virtua's internal scrollOffset from the
        // DOM, which manifested as the revert-puts-you-at-top
        // regression and the thread-switch flicker (right→wrong→right
        // settle). The consumer hides the surface while `!isWarm`
        // (MessageTimeline cascade-hide), so any pre-warm $fixScrollJump
        // isn't user-visible. Once warm, the surface is shown and the
        // gate becomes the single writer arbiter again.
        //
        // The user is reading mid-thread (escaped) or another surface
        // has paused auto-follow. Above-viewport visual stability
        // matters here — virtua's `$fixScrollJump` is keeping the
        // visible row anchored against a remeasure above the viewport.
        // Pass through.
        if (!warm || !isAtBottomState || escapedFromLockState || pauseDepth !== 0) {
          origSet.call(el, value);
          return;
        }
        // Active content-width reflow: the paired contentRO is going to
        // sync-pin, not spring-chase. Let virtua's anchor-preserving
        // compensation land in the same paint instead of suppressing it
        // and waiting for the later contentRO delivery.
        if (contentReflowSettleUntil > nowMs()) {
          origSet.call(el, value);
          return;
        }
        // animationMode === 'instant': the controller's contentRO
        // would respond to this growth with a synchronous sync-pin,
        // not a spring chase. virtua's `$fixScrollJump` and the
        // controller's pin would BOTH write the same target (the new
        // bottom). Letting virtua's write land first is strictly
        // better: contentEl's contentRO fires in a separate RO
        // delivery loop from virtua's per-row ROs (different observer
        // instances), so the controller's pin lands ~3ms later. In
        // that gap the DOM sits with the new scrollHeight but the
        // old scrollTop — a visible "right → wrong → right" flicker
        // where the bottom of a row that grew is briefly cut off
        // (bug-report-20260524T183128Z: seq 149 reveal at correct
        // bottom, seq 150 suppressed virtua write to 21839, seq 151
        // contentRO 3ms later pinning to 21839). Pass through so
        // virtua and the controller arrive at the same target in the
        // same paint.
        if (options.animationMode?.() === 'instant') {
          origSet.call(el, value);
          return;
        }
        // No spring is in flight (`springToken === 0`). The gate's
        // sole purpose is to keep the spring as the single writer
        // during a chase — when no chase exists, suppression has no
        // target to protect and only blocks legitimate virtua
        // `$fixScrollJump` anchor preservation. The original sync-pin
        // path that would otherwise race virtua only runs while
        // `mode === 'instant'` (handled above); in spring mode without
        // an active chase the controller is dormant, so virtua's write
        // is the only writer and must land. Regression evidence:
        // bug-report-20260524T200233Z had 15/15 externalWriteSuppressed
        // events at springToken=0 producing the thread-switch flicker
        // on actively-streaming threads (mode=spring, warm cleared
        // between chunks, no chase running when virtua compensated).
        if (springToken === 0) {
          origSet.call(el, value);
          return;
        }
        // Warm + sticking-and-engaged + spring chase in flight. Drop
        // the write so the spring in the contentRO callback is the
        // single writer responding to content growth. virtua's
        // `$fixScrollJump` would otherwise snap scrollTop to the
        // new bottom in one paint, pre-empting the spring's
        // interpolation and producing a user-visible 1–2 line snap.
        // The contentRO fire that follows the row remeasure will
        // see `scrollTop` still at the spring's current position,
        // compute a positive delta, and continue the chase smoothly.
        if (isUiRenderTraceEnabled()) trace('scroll.externalWriteSuppressed', () => ({
          requested: Math.round(value),
          current: Math.round(origGet.call(el) as number),
          springToken,
          warm,
          isAtBottomState,
          escapedFromLockState,
          pauseDepth,
          scrollHeight: Math.round((scrollEl?.scrollHeight ?? 0)),
          clientHeight: Math.round((scrollEl?.clientHeight ?? 0)),
        }));
      },
    });
    externalScrollTopGateInstalled = true;
  }

  function uninstallExternalScrollTopGate(el: HTMLElement): void {
    if (!externalScrollTopGateInstalled) return;
    if (savedScrollTopWasOwn && savedScrollTopDescriptor) {
      // Restore the prior own-property accessor (test stubGeometry,
      // or anything else that owned it before us).
      Object.defineProperty(el, 'scrollTop', savedScrollTopDescriptor);
    } else {
      // We installed an own-property where there was only an inherited
      // one (production). Removing it lets the prototype accessor
      // resume answering reads / writes.
      delete (el as unknown as { scrollTop?: number }).scrollTop;
    }
    savedScrollTopDescriptor = undefined;
    savedScrollTopWasOwn = false;
    externalScrollTopGateInstalled = false;
  }

  // ===== Lifecycle =====
  function attach(nextScrollEl: HTMLElement, nextContentEl: HTMLElement): void {
    if (scrollEl === nextScrollEl && contentEl === nextContentEl) return;
    detach();
    scrollEl = nextScrollEl;
    contentEl = nextContentEl;
    // Install the external-write gate BEFORE anything else can fire
    // an RO or touch scrollTop, so virtua's per-row ResizeObservers
    // (which fire on mount) hit the gate from frame zero.
    installExternalScrollTopGate(nextScrollEl);
    beginWarmup();
    if (isUiRenderTraceEnabled()) trace('scroll.attach', () => ({
      surface: nextScrollEl.dataset?.testid ?? '',
      scrollTop: Math.round(nextScrollEl.scrollTop),
      scrollHeight: Math.round(nextScrollEl.scrollHeight),
      clientHeight: Math.round(nextScrollEl.clientHeight),
      contentHeight: Math.round(nextContentEl.getBoundingClientRect().height),
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
    }));
    setupContentRO();
    nextScrollEl.addEventListener('wheel', handleWheel, { passive: true });
    nextScrollEl.addEventListener('scroll', handleScroll, { passive: true });
    nextScrollEl.addEventListener('pointerdown', handlePointerDown, { passive: true });
    nextScrollEl.addEventListener('keydown', handleKeydown);
    nextScrollEl.addEventListener('touchstart', handleTouchStart, { passive: true });
    nextScrollEl.addEventListener('touchmove', handleTouchMove, { passive: true });
    nextScrollEl.addEventListener('touchend', handleTouchEnd, { passive: true });
    nextScrollEl.addEventListener('touchcancel', handleTouchEnd, { passive: true });
    detachWheel = () => nextScrollEl.removeEventListener('wheel', handleWheel);
    detachScroll = () => nextScrollEl.removeEventListener('scroll', handleScroll);
    detachPointer = () => nextScrollEl.removeEventListener('pointerdown', handlePointerDown);
    detachKeyTouch = () => {
      nextScrollEl.removeEventListener('keydown', handleKeydown);
      nextScrollEl.removeEventListener('touchstart', handleTouchStart);
      nextScrollEl.removeEventListener('touchmove', handleTouchMove);
      nextScrollEl.removeEventListener('touchend', handleTouchEnd);
      nextScrollEl.removeEventListener('touchcancel', handleTouchEnd);
    };
    lastObservedScrollTopForRestick = nextScrollEl.scrollTop;
    refreshIsNearBottom();
    installStickStateDevHook();
  }

  // Dev-only window hook so a user reproducing a sticky-scroll bug can
  // dump the controller's full internal state in one console call.
  // Active only when uiRenderTrace is enabled (DEBUG=1 build flag) so
  // production builds carry no surface. Each attach() reinstalls the
  // hook against the current closure so multiple panes don't fight; the
  // most recently attached controller wins, which matches the trace
  // semantics. Returns a structured snapshot — copy/paste-friendly for
  // pasting into bug reports.
  function installStickStateDevHook(): void {
    if (typeof window === 'undefined' || !isUiRenderTraceEnabled()) return;
    (window as Window & {
      __stickState?: () => Record<string, unknown>;
    }).__stickState = () => {
      const dist = scrollEl
        ? scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight
        : null;
      return {
        // Core decision flags — these are the gates that determine whether
        // contentRO positive-delta will pin and whether the spring can start.
        isAtBottomState,
        escapedFromLockState,
        isNearBottomState,
        pauseDepth,
        warm,
        springStopRequested,
        springToken,
        recentDownIntentActive: hasRecentDownIntent(),
        recentDownIntentUntil: Math.round(recentDownIntentUntil),
        scrollbarDragSessionActive,
        scrollbarDragSessionVersion,
        // Restore-snap consent (consumed by forceStick({reason:'restore'})).
        restoreSnapArmed,
        // Geometry snapshot for cross-referencing the trace.
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        distanceFromBottom: dist === null ? null : Math.round(dist),
        target: scrollEl ? Math.round(targetScrollTop()) : null,
        // Public getters as the consumer would see them — so the dump
        // makes "isAtBottom returned the wrong value" diagnoses obvious.
        publicIsSticky:
          isAtBottomState && !escapedFromLockState && pauseDepth === 0,
        publicIsAtBottom: !escapedFromLockState && (isAtBottomState || isNearBottomState),
      };
    };
  }

  function detach(): void {
    // Restore the original scrollTop descriptor BEFORE tearing down
    // the rest. Once uninstalled, virtua's writes (if any) flow
    // through unchanged — the symmetric case of attach() installing
    // before any RO can fire.
    if (scrollEl) uninstallExternalScrollTopGate(scrollEl);
    contentRO?.disconnect();
    contentRO = undefined;
    detachWheel?.();
    detachWheel = undefined;
    detachScroll?.();
    detachScroll = undefined;
    detachPointer?.();
    detachPointer = undefined;
    detachKeyTouch?.();
    detachKeyTouch = undefined;
    if (resizeClearTimer) {
      clearTimeout(resizeClearTimer);
      resizeClearTimer = null;
    }
    if (externalScrollClearTimer) {
      clearTimeout(externalScrollClearTimer);
      externalScrollClearTimer = null;
    }
    externalScrollIgnoreUntil = 0;
    clearRecentDownIntent();
    clearScrollbarDragSession();
    cancelTargetAnimation();
    cancelSpring();
    springStopRequested = false;
    lastTargetChangedAt = 0;
    clearWarmupTimers();
    warm = false;
    resizeDifference = 0;
    resizeCorrelatedUntaggedScrollBudget = 0;
    previousHeight = undefined;
    previousWidth = undefined;
    contentReflowSettleUntil = 0;
    touchStartY = null;
    lastObservedScrollTopForRestick = -1;
    // DELIBERATELY leave `restoreSnapArmed` untouched. attach() calls
    // detach() up-front when scrollEl / contentEl change, and on first
    // mount that wipe ran BETWEEN the consumer's $effect.pre arm and
    // the restore $effect's forceStick({reason:'restore'}), making
    // the consent effectively unusable for the initial-mount path.
    // The flag is invalidated by outer-scroll escape intent (wheel / key /
    // touch / pointer that can reach the scroll element), selection, animateScrollTo /
    // stopScroll, explicit user-reason forceStick, and the
    // consume-on-restore path itself — that's
    // enough to keep it from leaking stale consent across legitimate
    // lifecycles. True teardown also discards the entire controller
    // instance, so a residual `true` here has no observable effect.
    scrollEl = undefined;
    contentEl = undefined;
  }

  return {
    get isSticky() {
      return isAtBottomState && !escapedFromLockState && pauseDepth === 0;
    },
    get isAtBottom() {
      return !escapedFromLockState && (isAtBottomState || isNearBottomState);
    },
    get escapedFromLock() {
      return escapedFromLockState;
    },
    get isWarm() {
      return warm;
    },
    pauseAutoScroll,
    notifyContentMaybeGrew,
    notifyLiveContentMaybeGrew,
    preserveScrollAnchor,
    attach,
    detach,
    forceStick,
    markAtBottom,
    animateScrollTo,
    runExternalScroll,
    stopScroll,
    setEscapedFromLock,
    armWarmup: beginWarmup,
    skipWarmup(): void {
      if (!warm) {
        warm = true;
        clearWarmupTimers();
      }
    },
    notifyQuietContextSignalChanged,
    armRestoreSnap,
  };
}
