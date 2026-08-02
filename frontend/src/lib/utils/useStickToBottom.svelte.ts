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
//     first. A one-shot structural-append mark can also make the next
//     near-term command/tool row growth spring-eligible while
//     animationMode is 'instant'; that path cancels after arrival and does
//     not enter the streaming sentinel. The warm gate defends against the
//     original 80LoC-spring-delete regression (commit e00723f) where
//     mount-time virtua remeasurement and async Streamdown typesetting
//     would spring-chase a thread restore visibly.
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
const PROGRAMMATIC_SCROLL_EVENT_TOKEN_TTL_MS = 500;
const MAX_PROGRAMMATIC_SCROLL_EVENT_TOKENS = 128;
const PROGRAMMATIC_SCROLL_EVENT_DUPLICATE_BUDGET = 4;
const STRUCTURAL_APPEND_SPRING_WINDOW_MS = 250;

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
// Idle sync-pin deadband. virtua re-measures its explicit container height
// (totalSize) on a 150ms scroll-end debounce (see node_modules/virtua scroller
// `$update(2)`); on a fractional-DPR grid that re-measure lands a few sub-pixels
// apart depending on the current scroll offset's sub-pixel phase. While pinned
// at the bottom and idle (no spring in flight), the contentRO sync-pin used to
// re-pin scrollTop on EVERY nonzero delta — including that ±2–3px jitter. The
// re-pin nudged the offset to the other phase AND re-armed virtua's 150ms timer,
// which re-measured back: a sustained net-zero limit cycle the user saw as the
// "settle vibration" (separator lines shimmying, thinking text jittering once a
// spring scroll reaches the end, the timeline trembling when the composer wraps
// a line — all while pinned at bottom, idle or at stream end). The browser's
// shrink-at-bottom scrollTop clamp is one-shot per direction (shrinks clamp,
// grows never do); the SUSTAINING energy is the controller's re-pin. Skipping
// the re-pin once scrollTop is already within this band of the bottom breaks the
// feedback at its source. Must be ≥ the observed scrollTop discrepancy (~3px,
// bug-report-20260629T203231Z) and well below one line (~20px) so genuine
// line-sized growth still pins exactly and the resting gap stays imperceptible.
// Distance-to-target (not delta-magnitude): it directly means "close enough to
// the bottom that a re-pin only chases sub-pixel noise."
export const IDLE_SYNC_PIN_DEADBAND_PX = 6;
// The onset-detach discriminator: how long scrollTop must have been visually
// still before a positive-delta chunk is sync-pinned straight to the new bottom
// (an in-frame correction, no spring) instead of handed to the spring.
//
// Mechanism the pin defeats: the spring's tick advances on rAF, and per the HTML
// "update the rendering" order rAF callbacks fire BEFORE ResizeObserver callbacks
// within a frame — so a spring started from REST to chase a content growth lands
// its first catch-up one frame late, and that onset frame paints with the bottom
// detached by ~the growth chunk. Measured in bug-report-20260630T195825Z:
// `distanceFromBottom` 0→33..85px for a frame, then eased back over ~390ms — the
// "goes down ½–1 line then presents properly" the user reported. Mid-stream the
// spring's own continuous motion masks that identical onset; at settle the screen
// is still, so the one frame reads as a flicker. The two are the SAME controller
// event — the ONLY observable difference is whether scrollTop was moving. So the
// discriminator is temporal, not kinematic: the spring stays sentinel-alive at
// rest through a turn (springToken / velocity can't tell "parked" from
// "gliding"), but the rendered scrollTop can. Past this idle window the spring is
// parked → pin in-frame (no detach, and nothing mid-glide to snap); within it the
// spring is actively following → leave it be (its motion masks the onset).
// Instant pins count as motion, so a burst pins its first chunk once and the next
// fast chunk re-engages the spring; a lone straggler just pins. ~3 frames at
// 60Hz; this is the primary tuning knob — lower catches quicker settle stragglers
// at the cost of snapping more borderline chunks, higher preserves more spring
// glide. See docs/architecture/settle-flicker-analysis.md.
const SETTLE_IDLE_MS = 50;
// SUPERSEDED by SETTLE_IDLE_MS. The "bound" this named — pin scrollTop to
// committed − 8px whenever a slow-starting spring would detach further — keyed on
// the spring's kinematic state (velocity / springToken) and so fired on ordinary
// mid-stream growth too, snapping the smooth follow (reverted 2026-06-30, see the
// analysis doc). Retained only because the not-yet-reworked settle-flicker tests
// still import it; delete both together once the settled-idle pin is confirmed
// in-app.
export const MAX_STREAM_DETACH_PX = 8;
// Velocity (px per 60fps frame) at or below which the spring KEEPS its
// upward follow momentum when it catches up to the bottom mid-stream,
// instead of zeroing it. Content height grows in line-sized quanta during
// streaming — a wrap moves the bottom ~20px, but the word-by-word reveals
// between wraps don't change height — so the spring repeatedly reaches the
// bottom and idles in the gaps. Carrying a gentle follow velocity across
// those gaps lets the next line continue the existing motion rather than
// re-accelerating from a dead stop: the difference between one continuous
// glide and a string of slow-start lurches (the reported jank).
//
// The ceiling keeps this from reintroducing the big→small snap that the
// diff===0 zeroing originally fixed. A velocity left over from a LARGE
// motion — a 200–400px jump, or an external instant-pin
// (notifyContentMaybeGrew) that froze velocity mid-chase at ~8–28 — would,
// if carried into a small ~3–10px growth, cross the target on the first
// frame and snap instead of gliding. Those remnants sit well above this
// ceiling and are still shed; only genuine line-follow momentum
// (~1–2 px/frame) is kept. The snap threshold is exact: a carried velocity
// crosses a growth of size D on the first frame when
// velocity > D · (mass − stiffness) / damping — i.e. D · 1.714… for
// DEFAULT_SPRING — so a sub-line ~3px growth tolerates carry up to ~5; 4
// leaves margin while still exceeding single/double-line follow speed. If
// DEFAULT_SPRING is retuned that margin shifts; the "stuck spring"
// snap-guard tests pin the upper bound (their remnant velocities ~8/14/28
// all exceed it, so they zero exactly as before).
const SPRING_CARRY_VELOCITY_CEILING = 4;
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
//
// CRITICAL: the shortened window is only sound once the SURFACE has
// stopped moving. `quietContextSignal` reports svelte-streamdown's async
// state — it is blind to virtua's estimate→measure cascade, which mounts
// rows at ESTIMATED_ROW_SIZE and grows scrollHeight over a sequence of
// contentRO fires spaced WIDER than SETTLED_QUIET_MS. If we shorten the
// window while that cascade is in flight, the timer fires in the gap
// between two corrections and reveals a still-growing surface — the
// "lands right, flickers, lands right again" idle-thread regression. So
// the shortened window is gated on geometry stability
// (`quietWindowForGeometry`): a large contentRO height delta keeps us on
// the conservative QUIET_MS window (which each cascade fire resets, so it
// only closes once the cascade goes quiet — geometry-driven, not a
// cascade-duration guess); only a small delta admits the shortcut.
// Granularity: the gate keys on the AGGREGATE contentRO height delta, so a
// lone row measuring within the epsilon of its estimate and correcting a
// frame ahead of a later large step could still admit the shortcut early.
// Accepted as an unlikely residual — per-row tracking would need the
// second observer the scroll contract forbids, and a real cascade fire
// measures many rows at once, so the aggregate step dwarfs any single
// in-band row.
const SETTLED_QUIET_MS = 16;
// A contentRO height delta at or below this counts as "the surface has
// effectively settled" — small enough that revealing on SETTLED_QUIET_MS
// cannot show a perceptible bottom-shift. Anything larger is treated as
// virtua's estimate→measure cascade still in flight. Absolute px, not
// viewport-relative: the threshold is human-perceptible scroll
// displacement, which does not scale with viewport height. virtua mounts
// rows at ESTIMATED_ROW_SIZE (56px, see MessageTimeline.svelte), so each
// per-row correction is |measured − 56| — tens-to-hundreds of px for any
// real multi-line chat row, comfortably clear of this floor.
const WARMUP_SETTLE_EPSILON_PX = 8;

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
   * but honors animationMode and structural-append marks: active chat turns
   * and just-appended command/tool row batches spring-chase instead of
   * sync-pinning.
   */
  notifyLiveContentMaybeGrew(): void;
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
  let stickStateDevHook: (() => Record<string, unknown>) | undefined;

  let targetAnimationFrame: number | null = null;
  let targetAnimationResolve: ((result: 'completed' | 'cancelled') => void) | null = null;
  let restoreTargetScrollBehavior: (() => void) | null = null;
  let resizeDifference = 0;
  let resizeClearTimer: ReturnType<typeof setTimeout> | null = null;
  let ignoreScrollToTop = -1;
  let pendingProgrammaticScrollEventTokens: { top: number; expiresAt: number; remaining: number }[] = [];
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
  // Motion clock for the settled-idle onset-detach pin (see SETTLE_IDLE_MS).
  // `lastScrollMotionAt` stamps the last controller write that actually moved
  // scrollTop; `lastWrittenScrollTop` is the value it moved to, so the next write
  // can tell a real move from a no-op re-pin at rest. The spring stays
  // sentinel-alive at rest during a turn, so kinematic state can't distinguish a
  // parked spring from a gliding one — the rendered position can.
  // NEGATIVE_INFINITY = no motion yet, treated as fully idle so the first chunk of
  // a turn pins cleanly.
  let lastScrollMotionAt = Number.NEGATIVE_INFINITY;
  let lastWrittenScrollTop = -1;

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
  let arrivalReadbackAcceptedTarget: number | null = null;
  let springStopRequested = false;
  let structuralAppendSpringUntil = 0;
  let springStartedFromStructuralAppend = false;
  // The target when the sentinel first entered after a chase. When the
  // sentinel tick sees diff > 0 but target === sentinelEntryTarget, the
  // content oscillated and returned to the same height — snap instantly.
  // -1 means not in sentinel. Only set on the FIRST sentinel entry
  // after a chase (not on re-entry), so the value reflects the target
  // at the moment the spring settled. Cleared on cancelSpring() and
  // when the spring exits sentinel with a different target.
  let sentinelEntryTarget = -1;

  // ===== Deadband-stable committed bottom target =====
  // virtua re-measures its explicit totalSize on a 150 ms scroll-end debounce
  // (node_modules/virtua/lib/core/index.js, the `J` scroller); the value lands
  // a few sub-pixels apart depending on scrollTop's sub-pixel phase
  // (fractional-DPR / WSLg compositing), so the RAW bottom (`targetScrollTop()`)
  // oscillates ±2-3 px at rest. The two steady-state-at-bottom writers — the
  // spring-tick chase and the contentRO sync-pin — reference THIS committed
  // value instead of raw so virtua's sub-pixel noise never moves what they aim
  // for, and neither re-arms virtua's timer (any controller write that MOVES
  // scrollTop fires a scroll event virtua's own listener sees — `ignoreScrollToTop`
  // does not reach it). `committed` snaps to raw only when raw crosses the
  // deadband (real line growth / shrink), giving a clean jitter-vs-growth
  // discriminator that the retain-window bump also keys on. Updated ONLY in the
  // contentRO delivery (the single site that knows content deltas); snapped
  // exact on forceStick; -1 = uninitialized (read falls back to raw). The
  // spring CHASES committed (it glides over frames, so chasing raw would track
  // the jitter even inside the retain window); the sync-pin's SKIP decision
  // keys on committed (deadband-stable) but it still WRITES raw for a
  // pixel-exact one-frame snap. Event-driven readers (overshoot guard, virtua
  // anchor redirect) stay on RAW: scrollTop legitimately sits between committed
  // and raw after a browser max-scroll clamp, and comparing against committed
  // there would fire spurious snaps.
  let committedBottomTarget = -1;

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
  // Magnitude of the most recent contentRO height change, shared by both
  // quiet-timer arming sites (`bumpQuietTimer`, `notifyQuietContextSignalChanged`)
  // so the shortened window stays geometry-gated. Reset to +Infinity each
  // warm-up cycle: geometry is assumed to be moving until a small delta is
  // actually observed, so the first fire (which has no baseline) and any
  // large correction both hold us on the conservative window.
  let lastContentHeightDelta = Number.POSITIVE_INFINITY;

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
    lastContentHeightDelta = Number.POSITIVE_INFINITY;
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

  // Pick the quiet window for an arm. The shortened SETTLED_QUIET_MS is
  // only sound once the surface has stopped moving in large steps; while
  // virtua's estimate→measure cascade is still growing scrollHeight we use
  // the conservative QUIET_MS window, which each cascade fire resets so it
  // closes only after the cascade goes quiet. See WARMUP_SETTLE_EPSILON_PX.
  function quietWindowForGeometry(): number {
    return lastContentHeightDelta <= WARMUP_SETTLE_EPSILON_PX
      ? SETTLED_QUIET_MS
      : QUIET_MS;
  }

  // `heightDelta` is the contentRO height change driving this bump
  // (undefined on the first fire — no baseline yet). It keeps the shortened
  // window geometry-gated: until we have observed the surface hold still,
  // a thread's mount cascade cannot trip an early reveal.
  function bumpQuietTimer(heightDelta?: number): void {
    if (warm) return;
    hasFirstContentRO = true;
    // First fire (undefined) has no baseline → treat as still-moving. A
    // height delta of exactly 0 is a spurious / width-only / padding-var
    // reflow (this runs before the contentRO's own `delta === 0` early-out)
    // and carries no new height information — keep the prior magnitude so a
    // reflow interleaved between two large cascade steps cannot masquerade
    // as "settled" and trip the shortened window. That interleave is the
    // cold-boot residual: cascade steps are far apart and font/layout
    // reflows fire in the gaps.
    if (heightDelta === undefined) {
      lastContentHeightDelta = Number.POSITIVE_INFINITY;
    } else if (heightDelta !== 0) {
      lastContentHeightDelta = Math.abs(heightDelta);
    }
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
    quietTimer = setTimeout(() => markWarm('quiet'), quietWindowForGeometry());
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
    // Geometry-gated like bumpQuietTimer: if the surface was still moving in
    // large steps at the last contentRO, the settle signal flipping does not
    // license an early reveal — the cascade outlasts the settle signal.
    quietTimer = setTimeout(() => markWarm('quiet'), quietWindowForGeometry());
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

  // Read-only accessor for the deadband-stable committed bottom (see the
  // `committedBottomTarget` state block). Returns committed once contentRO has
  // established it, else the raw bottom. The spring tick uses this so it chases
  // a value virtua's sub-pixel re-measure can't move; once committed is set it
  // also skips the `targetScrollTop()` forced-layout read entirely. This accessor
  // never mutates committed. During a chase the contentRO delivery is the only
  // site that moves it (by the deadband rule); forceStick re-baselines it to the
  // exact bottom on an explicit stick and detach clears it to -1, but those are
  // lifecycle events, not per-frame. So a spring rAF (which fires before the paired
  // ResizeObserver within a frame) can't consume a growth's deadband-crossing
  // before contentRO sees it and bumps the retain window.
  function committedTargetScrollTop(): number {
    return committedBottomTarget < 0 ? targetScrollTop() : committedBottomTarget;
  }

  function isWithinArrivalDistance(current: number, target: number): boolean {
    return Math.abs(current - target) <= ARRIVAL_DISTANCE_PX;
  }

  function scrollTopIsAtTarget(target: number): boolean {
    return !scrollEl || isWithinArrivalDistance(scrollEl.scrollTop, target);
  }

  function scrollTargetsMatch(a: number, b: number): boolean {
    return isWithinArrivalDistance(a, b);
  }

  function acceptedReadbackMatchesTarget(target: number): boolean {
    return arrivalReadbackAcceptedTarget !== null
      && scrollTargetsMatch(arrivalReadbackAcceptedTarget, target)
      && scrollTopIsAtTarget(target);
  }

  function shouldWriteExactArrivalTarget(target: number): boolean {
    if (!scrollEl) return false;
    if (scrollEl.scrollTop === target) return false;
    if (!scrollTopIsAtTarget(target)) return true;
    return !acceptedReadbackMatchesTarget(target);
  }

  function recordArrivalReadbackAcceptance(target: number): void {
    if (scrollEl && scrollEl.scrollTop !== target && scrollTopIsAtTarget(target)) {
      arrivalReadbackAcceptedTarget = target;
      return;
    }
    arrivalReadbackAcceptedTarget = null;
  }

  function writeExactArrivalTarget(caller: string, target: number): void {
    writeCaller = caller;
    writeScrollTop(target);
    recordArrivalReadbackAcceptance(target);
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
    clearProgrammaticScrollState();
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
    // `scrollTop === ignoreScrollToTop` check matches. One post-write read, reused
    // for the motion clock (see SETTLE_IDLE_MS) — stamp only when the write
    // actually moved the viewport, so a no-op re-pin at rest doesn't read as
    // motion and defeat the settled-idle detector.
    const postWriteTop = scrollEl.scrollTop;
    if (postWriteTop !== lastWrittenScrollTop) {
      lastScrollMotionAt = nowMs();
      lastWrittenScrollTop = postWriteTop;
    }
    ignoreScrollToTop = postWriteTop;
    recordProgrammaticScrollEventToken(postWriteTop);
    lastObservedScrollTopForRestick = postWriteTop;
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

  function recordProgrammaticScrollEventToken(top: number): void {
    const now = nowMs();
    pendingProgrammaticScrollEventTokens.push({
      top,
      expiresAt: now + PROGRAMMATIC_SCROLL_EVENT_TOKEN_TTL_MS,
      remaining: PROGRAMMATIC_SCROLL_EVENT_DUPLICATE_BUDGET,
    });
    if (pendingProgrammaticScrollEventTokens.length > MAX_PROGRAMMATIC_SCROLL_EVENT_TOKENS) {
      pendingProgrammaticScrollEventTokens.splice(
        0,
        pendingProgrammaticScrollEventTokens.length - MAX_PROGRAMMATIC_SCROLL_EVENT_TOKENS,
      );
    }
  }

  function consumeProgrammaticScrollEventToken(top: number): boolean {
    if (pendingProgrammaticScrollEventTokens.length === 0) return false;
    const now = nowMs();
    let consumed = false;
    let writeIndex = 0;
    for (const token of pendingProgrammaticScrollEventTokens) {
      if (token.expiresAt < now) continue;
      if (!consumed && token.top === top) {
        consumed = true;
        token.remaining -= 1;
        if (token.remaining > 0) {
          pendingProgrammaticScrollEventTokens[writeIndex] = token;
          writeIndex += 1;
        }
        continue;
      }
      pendingProgrammaticScrollEventTokens[writeIndex] = token;
      writeIndex += 1;
    }
    pendingProgrammaticScrollEventTokens.length = writeIndex;
    return consumed;
  }

  function clearProgrammaticScrollState(): void {
    pendingProgrammaticScrollEventTokens = [];
    ignoreScrollToTop = -1;
    structuralAppendSpringUntil = 0;
    springStartedFromStructuralAppend = false;
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
    arrivalReadbackAcceptedTarget = null;
    springStartedFromStructuralAppend = false;
    // Reset the target-change timestamp so a stale value can't trick a
    // fresh chase into thinking it's within the retain window right out
    // of the gate (matches the historical 80LoC-spring cleanup semantics).
    lastTargetChangedAt = 0;
    sentinelEntryTarget = -1;
  }

  // Settle the spring at a target it has already reached, shared by both
  // oscillation-recovery sites: the spring-tick path (the spring caught up
  // to a target that had returned to the sentinel-entry value) and the
  // synchronous contentRO path (an above-viewport remeasure regrow). The
  // body is load-bearing and must stay identical across both — zero
  // velocity/accumulated so the arrival check stays settled, and consume
  // `sentinelEntryTarget` so the OTHER site's snap no-ops for this same
  // oscillation. `springToken` is intentionally left untouched: the spring
  // stays sentinel-alive so the external-write gate stays engaged. Extracted
  // so the two sites can't drift — the same reason `springGateOpen()` is shared.
  //
  // FOOTGUN — this is a one-shot CLAMP RECOVERY, not an oscillation source. If
  // you ever catch it firing every frame in a sustained ±N px limit cycle (text
  // visibly "vibrating"/flickering, idle or while streaming), the bug is NOT
  // here: some other code is driving a per-frame content-height oscillation that
  // keeps re-arming the snap. The classic cause is a forced synchronous layout
  // read (getBoundingClientRect / offsetHeight) in a ResizeObserver or Svelte
  // `use:` action hot path — see timelineRowGeometry.ts `applyParams`, where
  // exactly this once happened. Do NOT "fix" the vibration by adding a
  // stop-after-N break here: this snap exists to rescue scrollTop from a browser
  // max-scroll clamp, and a break would instead STRAND it there — the
  // post-width-reflow floating-message bug it recovers from. Fix the driver.
  function snapOscillationToBottom(caller: string, top: number): void {
    writeCaller = caller;
    writeScrollTop(top);
    velocity = 0;
    accumulated = 0;
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
      && (options.animationMode?.() === 'spring' || structuralAppendSpringUntil > nowMs());
  }

  function startSpringIfNeeded(): void {
    if (springToken !== 0) return;
    if (!springGateOpen()) return;
    springStartedFromStructuralAppend =
      options.animationMode?.() !== 'spring'
      && structuralAppendSpringUntil > nowMs();
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

      // Frame-rate independent spring integration. One full step matches
      // the tuned 60Hz recurrence; higher-refresh frames integrate a
      // fractional step and still write every rAF, so 120Hz displays do not
      // see every other frame held. Long gaps are capped to a bounded burst
      // so a blocked frame cannot pay the entire stall in one write.
      const dtFrames = lastTickAt === null ? 1 : (now - lastTickAt) / SIXTY_FPS_INTERVAL_MS;
      lastTickAt = now;
      const integrationFrames = Math.min(Math.max(dtFrames, 0), SPRING_MAX_CATCHUP_STEPS);

      // Cache per-tick. Chase the deadband-stable COMMITTED bottom, not raw:
      // raw oscillates ±2-3 px under virtua's scroll-end re-measure, and a
      // spring chases over many frames, so a raw target would make the spring
      // track that jitter (the per-line streaming vibration) even while content
      // is briefly settling inside the retain window. committed only moves on
      // real growth/shrink. Once committed is established this also avoids the
      // `targetScrollTop()` forced-layout read (`scrollHeight`/`clientHeight`).
      const target = committedTargetScrollTop();
      const current = scrollEl.scrollTop;
      if (
        arrivalReadbackAcceptedTarget !== null
        && !scrollTargetsMatch(arrivalReadbackAcceptedTarget, target)
      ) {
        arrivalReadbackAcceptedTarget = null;
      }

      // Whether the consumer still wants spring follow, and whether a
      // target change landed recently enough that more content is probably
      // still arriving. Hoisted above the diff branch so the caught-up
      // branch can decide whether to KEEP momentum for the next growth or
      // shed it and settle; the arrival check below reuses both.
      const wantsStreamingSpringNow = options.animationMode?.() === 'spring';
      const wantsSpringNow = wantsStreamingSpringNow || springStartedFromStructuralAppend;
      const withinTargetChangeRetainWindow =
        wantsSpringNow && now - lastTargetChangedAt < RETAIN_ANIMATION_DURATION_MS;

      // Deadband settle (spring counterpart of the contentRO idle sync-pin
      // deadband). Only engages AFTER the spring has arrived once and is holding
      // in the sentinel (`sentinelEntryTarget >= 0`): that is the only state in
      // which the residual gap is virtua's sub-pixel re-measure plus the
      // browser's max-scroll clamp (the browser drops scrollTop a few px below
      // the bottom on each totalSize shrink, and restoring it the rest of the
      // way is a real scrollTop move that re-arms virtua's 150 ms timer — the
      // per-line streaming vibration). The INITIAL chase runs with
      // `sentinelEntryTarget === -1`, so a long chase that outlasts the retain
      // window still lands pixel-exact rather than freezing within the deadband.
      // Also requires the retain window to have lapsed (no real growth in
      // flight) — while it is fresh the spring chases committed pixel-precise.
      const settledWithinDeadband =
        sentinelEntryTarget >= 0
        && !withinTargetChangeRetainWindow
        && Math.abs(current - target) <= IDLE_SYNC_PIN_DEADBAND_PX;

      if (current !== target && !acceptedReadbackMatchesTarget(target) && !settledWithinDeadband) {
        // Content oscillation guard: if the sentinel was idle
        // (sentinelEntryTarget set) and the target returned to the
        // sentinel entry value, the content layer oscillated in
        // height (-N then +N from async Streamdown typesetting /
        // virtua row remount). The browser auto-clamped scrollTop
        // during the low point (native engine operation that
        // bypasses the property-descriptor gate), stranding scrollTop
        // below the restored target. Snap back instantly — a spring
        // chase for zero net content change is a visible artifact.
        if (sentinelEntryTarget >= 0 && scrollTargetsMatch(target, sentinelEntryTarget)) {
          snapOscillationToBottom('spring.oscillationSnap', target);
        } else {
          arrivalReadbackAcceptedTarget = null;
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
            writeCaller = crossedTarget ? 'spring.overshoot' : 'spring.tick';
            writeScrollTop(clamped);
            if (clamped === target) {
              recordArrivalReadbackAcceptance(target);
            }
            if (scrollEl.scrollTop !== current) accumulated = 0;
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
        // KEEP a gentle upward follow velocity across the catch-up instead
        // of zeroing it — that is what turns a line-by-line stream into one
        // glide rather than a slow-start per line. See
        // SPRING_CARRY_VELOCITY_CEILING for why the ceiling is load-bearing.
        // Shed velocity otherwise:
        //   - outside the retain window → streaming paused; the arrival
        //     check below needs |velocity| < 0.5 to settle the spring (or
        //     hand it to the sentinel), else it ticks at 60fps forever;
        //   - above the ceiling → a large-motion remnant that would snap a
        //     small follow-up growth (the big→small case the zeroing fixed);
        //   - downward (velocity <= 0) → carry is scoped to growth-follow;
        //     a shrink-follow remnant carried into a resumed growth would
        //     nudge the viewport the wrong way for a frame. This half is
        //     defensive: it guards a virtua applyJump overshoot a real browser
        //     can transiently produce. A descent large enough to build an
        //     observable negative remnant is bounded out of normal operation —
        //     `committed` tracks the reachable max within the committed deadband,
        //     so a genuine down-chase spans <= IDLE_SYNC_PIN_DEADBAND_PX
        //     (|velocity| < 1); any larger drop clamps `current` to the new max
        //     in one step. Hence no isolated unit test (see the carry describe
        //     block in the spec for the full reachability argument).
        accumulated = 0;
        const carryMomentum =
          withinTargetChangeRetainWindow
          && velocity > 0
          && velocity <= SPRING_CARRY_VELOCITY_CEILING;
        if (!carryMomentum) velocity = 0;
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
      // `settledWithinDeadband` counts as arrived: the `current !== target`
      // write block above was skipped (no chase, no oscillation snap), so without this the tick would
      // re-rAF every frame doing nothing. It already implies the retain window
      // lapsed, so the `!withinTargetChangeRetainWindow` guard below still holds.
      const arrived =
        (scrollTopIsAtTarget(target) || settledWithinDeadband)
        && Math.abs(velocity) < ARRIVAL_VELOCITY_THRESHOLD;
      if (arrived && !withinTargetChangeRetainWindow) {
        if (wantsStreamingSpringNow) {
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
          // Snap pixel-perfect on sentinel entry only when the browser readback
          // is outside the accepted arrival band. Some engines reject the exact
          // max scrollTop by one CSS pixel; repeatedly writing that rejected
          // target is pure jank and creates needless ResizeObserver pressure.
          // Zeroing velocity/accumulated keeps the arrival check stable across
          // sentinel ticks.
          //
          // Suppress the exact-arrival write while settled within the deadband:
          // scrollTop is then ≤ deadband below committed because the browser
          // clamped it during a sub-pixel shrink, and writing the exact target
          // would restore it the rest of the way on the next grow phase — a real
          // move that re-arms virtua's timer (the per-line streaming vibration).
          // The spring holds at the clamped position, sentinel-alive, until real
          // growth bumps the retain window and resumes the chase.
          if (!settledWithinDeadband && shouldWriteExactArrivalTarget(target)) {
            writeExactArrivalTarget('spring.arrive', target);
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
        // pixel-perfect rather than 0.5px above the bottom, unless the browser
        // already accepted a value inside the arrival band. Skipped when settled
        // within the deadband (same clamp-restore re-arm reason as the
        // sentinel-alive path above); cancelSpring hands off to the idle
        // sync-pin, whose own deadband holds the resting position.
        if (!settledWithinDeadband && shouldWriteExactArrivalTarget(target)) {
          writeExactArrivalTarget('spring.arrive', target);
        }
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
      // once they're done. The height delta keeps the shortened settle
      // window gated on geometry stability (undefined on the first fire,
      // which has no baseline — see bumpQuietTimer).
      bumpQuietTimer(prev === undefined ? undefined : nextHeight - prev);

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
      // Cache the bottom target once per RO delivery. `targetScrollTop()`
      // reads `scrollHeight` + `clientHeight` (forced layout), and neither
      // changes across this synchronous callback — the only writes here are to
      // `scrollTop`, which don't affect them — so the overshoot guard, the
      // oscillation snap, and the delta-branch pins can all reuse one read.
      // Mirrors the spring tick's per-frame `const target` discipline.
      const target = targetScrollTop();
      const overshootMagnitude = Math.max(0, scrollEl.scrollTop - target);
      const overshoot = overshootMagnitude > ARRIVAL_DISTANCE_PX;
      // Update the deadband-stable committed bottom (see `committedBottomTarget`).
      // This is the ONLY site in contentRO that moves committed. The rule is
      // ASYMMETRIC, and deliberately so:
      //   - DOWN (target < committed): always track. `target` is the browser's
      //     reachable max (scrollHeight - clientHeight, no offset — see
      //     targetScrollTop). committed must NEVER sit above it, or the spring
      //     chases a value the browser clamps away, never arrives, and ticks
      //     (writing scrollTop) every frame forever — a silent permanent spin
      //     when a sub-deadband shrink strands committed high before the spring
      //     has entered the sentinel. Clamping down here keeps committed
      //     reachable so normal arrival settles it.
      //   - UP (target > committed): snap only past the deadband (real line
      //     growth). A sub-deadband positive delta is virtua's scroll-end
      //     re-measure jitter; chasing it is the per-line streaming vibration.
      // `committedMoved` also gates the retain-window bump below — sub-deadband
      // jitter must NOT refresh the retain window, or the spring's deadband-
      // settle path is never reachable (it keys on the window having lapsed).
      // A downward clamp DOES bump retain mid-chase (the negative-delta branch),
      // but that is benign: committed is then reachable, so the spring arrives
      // and settles regardless; at idle (springToken === 0) that retain-bump site
      // is never reached and the idle sync-pin deadband still suppresses the
      // oscillation. Computed from the same `target` read so no second forced
      // layout.
      const committedMoved =
        committedBottomTarget < 0
        || target < committedBottomTarget
        || target - committedBottomTarget > IDLE_SYNC_PIN_DEADBAND_PX;
      if (committedMoved) committedBottomTarget = target;
      const committed = committedBottomTarget;
      // Idle settle-vibration guard (see IDLE_SYNC_PIN_DEADBAND_PX). When no
      // spring is in flight and we're not mid-reflow, a sync-pin that would only
      // move scrollTop a few px to the bottom is suppressed: that re-pin is the
      // energy that sustains virtua's scroll-end re-measure limit cycle. Distance
      // is measured to COMMITTED (deadband-stable) so the skip decision doesn't
      // itself flicker on/off with raw's jitter. Gated to the idle path only —
      // spring chases (springToken !== 0) and width reflows own their own writers
      // and must still pin.
      const idleSyncPinWithinDeadband =
        springToken === 0
        && !widthReflowActive
        && Math.abs(scrollEl.scrollTop - committed) <= IDLE_SYNC_PIN_DEADBAND_PX;
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
        idleSyncPinWithinDeadband,
        committed: scrollEl ? Math.round(committed) : null,
        committedMoved,
        isAtBottomState,
        escapedFromLockState,
        pauseDepth,
        isNearBottomState,
        prevWidth: prevWidth === undefined ? null : roundCssPx(prevWidth),
        nextWidth: roundCssPx(nextWidth),
        widthDelta: prevWidth === undefined ? null : roundCssPx(nextWidth - prevWidth),
        widthChanged,
        widthReflowActive,
        structuralAppendSpringPending: structuralAppendSpringUntil > nowMs(),
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        target: scrollEl ? Math.round(target) : null,
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
        writeScrollTop(target);
      }

      // ===== Synchronous sentinel oscillation recovery =====
      // A row ABOVE the viewport that transiently shrinks-then-regrows —
      // virtua remounting/remeasuring a replaced element, e.g. an image
      // user-message row scrolled out of the live window — momentarily drops
      // virtua's total size. Because virtua sizes its container explicitly
      // (`contain: size` + `height: <totalSize>px`), that drop is the
      // contentEl height we observe here. While pinned at the exact bottom,
      // the browser SYNCHRONOUSLY clamps scrollTop down during the dip (a
      // native operation that bypasses the property-descriptor write gate).
      // When the row regrows, total returns to the pre-dip value but
      // scrollTop is stranded below the restored bottom.
      //
      // The spring tick's `spring.oscillationSnap` already recovers this, but
      // it runs in a rAF — and per the HTML "update the rendering" order, rAF
      // callbacks fire BEFORE ResizeObserver callbacks within a frame, so a
      // snap reacting to THIS regrow RO delivery always lands one frame late.
      // That stranded frame paints as a one-frame jump
      // (bug-report-20260615T182227Z: codex thread, above-viewport image row
      // remeasure, ~37px upward jolt). Recovering synchronously here — in the
      // same RO delivery as the regrow, before paint — closes the gap.
      //
      // Gated identically to the spring-tick snap: only while a spring is
      // sentinel-idle (`springToken !== 0 && sentinelEntryTarget >= 0`) and
      // the new target has returned to exactly the sentinel-entry value, so
      // genuine new growth (target beyond the entry) still spring-chases and
      // active chases are untouched. Comparison is against COMMITTED (the
      // coordinate `sentinelEntryTarget` is now recorded in) and the stranded
      // check is gated on the DEADBAND, not 1px: virtua's sub-pixel re-measure
      // strands scrollTop ≤ deadband below committed every shrink, and snapping
      // that back is itself a re-arming write (the per-line streaming
      // vibration). Only a stranding LARGER than the deadband — a real
      // above-viewport regrow clamp (the ~37px image-row case) — recovers here;
      // sub-deadband jitter is left for the spring's deadband-settle to hold.
      // The shared snap (`snapOscillationToBottom`) consumes
      // `sentinelEntryTarget`, making the later spring-tick snap a no-op for
      // this same oscillation.
      const sentinelOscillationStranded =
        springToken !== 0
        && sentinelEntryTarget >= 0
        && isAtBottomState
        && !escapedFromLockState
        && pauseDepth === 0
        && scrollTargetsMatch(committed, sentinelEntryTarget)
        && Math.abs(scrollEl.scrollTop - sentinelEntryTarget) > IDLE_SYNC_PIN_DEADBAND_PX;

      if (sentinelOscillationStranded) {
        snapOscillationToBottom('contentRO.oscillationSnap', committed);
      } else if (delta > 0) {
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
            // Only a committed move (real line growth) refreshes the retain
            // window and (re)starts the chase. A sub-deadband positive delta is
            // virtua's scroll-end re-measure jitter: bumping here would keep the
            // retain window perpetually fresh, and the spring tick's deadband-
            // settle path (which requires the window to have lapsed) could never
            // be reached. An in-flight spring stays sentinel-alive and settles
            // via its own deadband; a dormant one is correctly left dormant.
            if (committedMoved) {
              lastTargetChangedAt = nowMs();
              // INVESTIGATING (2026-06-30): the flicker is a regression — a
              // known-good state existed with no flicker AND a working spring.
              // The settled-idle pin tried here replaced the spring with an
              // instant pin (wrong — user wants the spring). Reverted to plain
              // spring while the actual regressing change is found.
              startSpringIfNeeded();
            }
          } else if (!idleSyncPinWithinDeadband) {
            writeCaller = 'contentRO.positiveDelta';
            writeScrollTop(target);
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
            // Idle settle-vibration guard: when already within the deadband
            // of the bottom, the sync-pin only chases virtua's sub-pixel
            // scroll-end re-measure noise and re-arms its 150ms timer — the
            // sustaining energy of the limit cycle. Skip it. isAtBottomState
            // is already set above, so we stay logically pinned. Width reflows
            // never match (idleSyncPinWithinDeadband requires !widthReflow) and
            // still pin.
            if (!idleSyncPinWithinDeadband) {
              writeCaller = widthReflowActive
                ? 'contentRO.negativeDeltaReflow'
                : 'contentRO.negativeDelta';
              writeScrollTop(target);
            }
          } else if (committedMoved) {
            // Spring is the single writer mid-chase; sync write is
            // suppressed above. A real shrink (committed moved down) is a
            // target change, so bump the retain timestamp — otherwise a small
            // negative correction between chunks could let
            // `withinTargetChangeRetainWindow` lapse and the spring would
            // arrive-and-stop while a follow-up chunk was on its way. A
            // sub-deadband negative delta is virtua's re-measure jitter and
            // must NOT bump, or the spring's deadband-settle path is never
            // reached (same linchpin as the positive branch above).
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
    clearProgrammaticScrollState();
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
    const exactTagged = scrollTopAtEvent === tag;
    const tokenTagged = consumeProgrammaticScrollEventToken(scrollTopAtEvent);
    const controllerTagged = exactTagged || tokenTagged;
    const tagged = controllerTagged || externalTagged;
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
    const exactBottom = targetScrollTop();
    // Re-baseline committed to the exact bottom. forceStick is an explicit "we
    // are at the bottom NOW" event (scroll-to-bottom chip, thread-switch
    // restore), so it must land pixel-exact with no deadband short-rest, and it
    // resets the committed reference the steady-state writers chase afterward.
    committedBottomTarget = exactBottom;
    writeScrollTop(exactBottom);
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
      structuralAppendSpringPending: structuralAppendSpringUntil > nowMs(),
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      target: scrollEl ? Math.round(targetScrollTop()) : null,
    }));
    if (!gate.canPin) return;

    const target = targetScrollTop();
    if (scrollEl && scrollTopIsAtTarget(target)) {
      if (shouldWriteExactArrivalTarget(target)) {
        writeExactArrivalTarget('notifyLiveContentMaybeGrew.arrive', target);
      }
      refreshIsNearBottom();
      return;
    }
    if (willSpring && scrollEl) {
      const current = scrollEl.scrollTop;
      if (target - current > ARRIVAL_DISTANCE_PX) {
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

  function markStructuralContentPending(): void {
    structuralAppendSpringUntil = nowMs() + STRUCTURAL_APPEND_SPRING_WINDOW_MS;
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
        // Genuinely bottom-locked (warm, isAtBottomState, not escaped, not
        // paused) AND the DOM is already pinned to true bottom — the
        // controller's contentRO/spring pin already landed there this
        // cascade. virtua's `$fixScrollJump` is now requesting an
        // anchor-preserving offset BELOW true bottom: its `delta` only
        // compensates above-viewport remeasures, not the at/below-fold row
        // growth that pushed the bottom down. Letting it land paints one
        // frame a few hundred px short of bottom, then the next controller
        // pin snaps back — the cold-thread-switch flicker (correct →
        // up-jump → correct). Redirect the write to true bottom instead of
        // dropping it. Dropping is what the branch above documents as the
        // revert-to-top / right→wrong→right regression: a swallowed write
        // fires no "scroll" event, and virtua re-syncs its internal offset
        // model from the DOM through that listener (virtua core: the jump
        // path writes the DOM and relies on the resulting scroll event to
        // feed the model), so suppression diverges virtua's model from the DOM.
        // Redirecting keeps the DOM at the bottom the controller already
        // pinned, so virtua's DOM-derived model — last synced by that landed
        // pin — stays correct and the stale-anchor frame is never painted.
        // Narrow by construction: it fires ONLY when the DOM is already at
        // bottom (`domAlreadyPinned`) and virtua tries to move meaningfully
        // away from it, so virtua's legitimate above-viewport compensation
        // (DOM not yet pinned, or the user scrolled up — already passed
        // through above) is untouched, and an in-flight spring chase (DOM
        // intentionally below target → not `domAlreadyPinned`) falls through
        // to the spring-protection branches unchanged. The redirect target
        // is `targetScrollTop()`, the exact value the controller's own pin
        // writes, so the two writers can never disagree on the bottom.
        const bottomTarget = targetScrollTop();
        const currentScrollTop = origGet.call(el) as number;
        const domAlreadyPinned =
          bottomTarget - currentScrollTop <= AUTO_FOLLOW_BOTTOM_EPSILON_PX;
        const requestedMovesAwayFromBottom =
          bottomTarget - value > AUTO_FOLLOW_BOTTOM_EPSILON_PX;
        if (domAlreadyPinned && requestedMovesAwayFromBottom) {
          if (isUiRenderTraceEnabled()) trace('scroll.virtuaAnchorRedirect', () => ({
            requested: Math.round(value),
            bottomTarget: Math.round(bottomTarget),
            current: Math.round(currentScrollTop),
            springToken,
            scrollHeight: Math.round(scrollEl?.scrollHeight ?? 0),
            clientHeight: Math.round(scrollEl?.clientHeight ?? 0),
          }));
          origSet.call(el, bottomTarget);
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
        // Plain animationMode === 'instant': the controller's contentRO
        // would respond to this growth with a synchronous sync-pin, not a
        // spring chase. virtua's `$fixScrollJump` and the controller's pin
        // would BOTH write the same target (the new bottom), so let virtua's
        // write land in the same paint. Structural-append springs are the
        // exception: they intentionally animate command/tool row batches even
        // though the prose latch is instant, so keep the spring as the single
        // writer while that chase is in flight.
        if (options.animationMode?.() === 'instant' && !springStartedFromStructuralAppend) {
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
        // Large measurement-correction carve-out. The suppression below
        // exists to keep the spring the single writer for virtua's SMALL
        // (1–2 line) `$fixScrollJump` anchor compensations during a chase.
        // But a fresh-mount estimate→measure pass or a late async-typesetting
        // reflow (shiki/katex/mermaid) lands as ONE jump larger than the
        // viewport. Suppressing virtua's instant correction there leaves the
        // spring to chase the entire delta — the visible multi-hundred-px
        // "spring scroll" on a thread switch into an actively-streaming thread
        // (bug-report-20260622T041049Z: a single +2276px / ~1.7-viewport
        // virtua write suppressed, then a ~1s 2300px spring chase). A jump
        // that large is a layout correction, not streamed content: let
        // virtua's write land in the same paint (invisible) and the spring
        // resolves from the corrected position on its next tick (current ==
        // target → arrives → cancels). Symmetric with the positive contentRO
        // pin and the negative overshoot snap — incremental content springs,
        // bulk corrections snap. Viewport-relative so it self-scales and
        // cannot fire on real per-chunk streaming growth.
        //
        // Safety invariant: this branch is reached ONLY by non-controller
        // writes (virtua `$fixScrollJump`, browser auto-anchor). Every
        // streamed-content follow write — the contentRO positive pin and
        // every spring tick — is controller-owned and already passed through
        // at the `controllerOwnsScrollTopWrite` guard at the top of this
        // setter. So the threshold discriminates among virtua's anchor
        // corrections; it never sees streamed content and cannot snap it.
        // `currentScrollTop` is the same DOM read captured above for the
        // anchor-redirect check; no scrollTop write lands between there and
        // here (every interceding branch returns), so it is still current.
        const requestedScrollJump = Math.abs(value - currentScrollTop);
        if (requestedScrollJump > el.clientHeight) {
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
          current: Math.round(currentScrollTop),
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
    lastScrollMotionAt = Number.NEGATIVE_INFINITY;
    lastWrittenScrollTop = -1;
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
    const hook = () => {
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
    stickStateDevHook = hook;
    window.__stickState = hook;
  }

  function uninstallStickStateDevHook(): void {
    if (typeof window === 'undefined' || !stickStateDevHook) return;
    if (window.__stickState === stickStateDevHook) {
      delete window.__stickState;
    }
    stickStateDevHook = undefined;
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
    uninstallStickStateDevHook();
    if (resizeClearTimer) {
      clearTimeout(resizeClearTimer);
      resizeClearTimer = null;
    }
    if (externalScrollClearTimer) {
      clearTimeout(externalScrollClearTimer);
      externalScrollClearTimer = null;
    }
    externalScrollIgnoreUntil = 0;
    clearProgrammaticScrollState();
    clearRecentDownIntent();
    clearScrollbarDragSession();
    cancelTargetAnimation();
    cancelSpring();
    springStopRequested = false;
    lastTargetChangedAt = 0;
    // Re-baseline the committed bottom on detach so a new scrollEl / thread
    // doesn't inherit a stale value; the first contentRO delivery (or the
    // restore forceStick) re-establishes it. -1 = read falls back to raw.
    committedBottomTarget = -1;
    clearWarmupTimers();
    warm = false;
    resizeDifference = 0;
    resizeCorrelatedUntaggedScrollBudget = 0;
    previousHeight = undefined;
    previousWidth = undefined;
    contentReflowSettleUntil = 0;
    touchStartY = null;
    lastObservedScrollTopForRestick = -1;
    lastScrollMotionAt = Number.NEGATIVE_INFINITY;
    lastWrittenScrollTop = -1;
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
    markStructuralContentPending,
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
