// Content-geometry observation pipeline for the sticky-bottom
// controller (useStickToBottom): the content-geometry deliveries, the
// warm-up (quiescence) gate over them, and the resize-classification
// state that lets the intent machine tell layout-induced scroll events
// from user gestures.
//
// Two sources feed one pipeline (`processSample`):
//
// - ResizeObserver on contentEl (the default) — surfaces without a
//   virtualizer (discussion's ChannelView) observe their content element
//   directly.
// - Engine-sourced samples (`externalContentGeometry` option) — chat's
//   TimelineVirtualizer already knows every content-height change
//   synchronously (its spacer height IS the content height), so it
//   delivers `ContentGeometrySample`s post-flush via the controller's
//   `deliverContentGeometry` instead of the controller re-observing the
//   same element: same observation shape, one frame earlier, no
//   duplicate layout read on the streaming hot path. Engine samples also
//   carry per-row settle evidence that lets the warm gate reveal a
//   priors-hit revisit immediately (see the settled fast-path below).
//
// Each delivery is gathered here, decided by the pure resolver
// (scroll/resolver.ts), and applied here — through the controller's
// writeScrollTop chokepoint and spring instance, both reached via the
// deps interface — so everything about "a content delivery" reads in
// one place. The reactive `warm` flag stays controller-owned (consumers
// subscribe to `isWarm`); this module drives it through accessors.

import {
  RESIZE_CLEAR_PADDING_MS,
} from './intent';
import {
  resolveContentDelivery,
  type ContentDeltaObservation,
  type ResolverState,
} from './resolver';
import type { SpringChase } from './spring';
import { nowMs } from './time';
import { trace } from './trace';
import type { ScrollWriteCaller, WarmReason } from './types';
import { reportFrontendDiagnostic } from '../frontendErrorCapture';
import { isUiRenderTraceEnabled } from '../uiRenderTrace';
import type { ContentGeometrySample } from '../virtual/types';

// ResizeObserver width jitter at or below ~1 CSS px is measurement noise,
// not a reflow: fractional-DPR displays report single-device-pixel wobble
// (1 device px at 125%/150% scaling ≈ 0.8/0.67 CSS px), and fractional
// flex distribution can shift a pane's content box by similar amounts
// without the column meaningfully rewrapping. Every change this window
// exists for — pane resize, sidebar toggle, Mermaid useMaxWidth — moves
// width by tens of pixels, so nothing legitimate lives below this
// threshold. Misclassifying sub-threshold noise as a reflow is the
// expensive failure: it opens the settle window below and converts live
// streaming follow into 250ms of instant stepped pins (was 0.5px, which
// fractional-DPR wobble cleared).
const CONTENT_REFLOW_WIDTH_EPSILON_PX = 2;
// Width and height can arrive in separate ResizeObserver deliveries. Keep
// the layout-correction classification alive briefly so a width-only fire
// followed by renderer height settle still sync-pins.
const CONTENT_REFLOW_SETTLE_WINDOW_MS = 250;
// Same shape for the pinned-remeasure classification: a DISPLACED
// engine.anchorRedirect (the controller recovering a pinned viewport from
// a large post-warm remeasure burst — bug-report-20260822T020840Z) proves
// a measurement-correction wave is in flight, and the wave's trailing
// height deltas arrive over the following frames. Fixed window,
// deliberately NOT refreshed by the deltas it classifies: a self-
// extending window would convert a streaming turn that starts inside it
// into indefinite sync-pins.
const PINNED_REMEASURE_SETTLE_WINDOW_MS = 250;
// Cold-load settle: from warm-up arm (attach / restore forceStick /
// armWarmup at slice application) the pane is inside a cold load, and any
// height growth is the estimate→measure cascade or the window sync
// reconciling the painted replica — coordinate corrections to content the
// reader considers already-there, never the bottom advancing. Those must
// keep `scrollTop == bottom` true pre-paint (sync-pin), not animate: the
// 2026-08-22 boot trace glided 8.5kpx of measurement growth for ~2s and
// then got snapped by an unrelated bottom-held transaction. The warm gate
// alone cannot cover this — it opens on ~100ms of RO quiet while cascade
// bursts and the sync page land seconds later.
//
// The window ends the moment genuinely new content announces itself —
// a live-content stamp or an armed structural append observed at
// delivery time — because from then on glides own the pane. The time
// cap is only the failsafe for a pane that never streams; it is
// deliberately generous, because the cost of it running long is an
// instant (correct-position) pin where a glide was tolerable, while the
// cost of it running short is the boot-time animated crawl this exists
// to kill.
const COLD_LOAD_SETTLE_MAX_MS = 8000;

// ===== Warm-up (quiescence) gate =====
// After attach() or forceStick(), the controller stays in sync-pin mode
// until contentRO has been quiet for QUIET_MS or the FAILSAFE_MS
// deadline trips. This defends against the mount-time spring-chase
// regression: the virtualizer's per-row ResizeObserver and the markdown
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
// where shiki/mermaid worker startup + virtualizer first-paint can
// genuinely take >1s, while still letting the spring engage for the
// bulk of a typical multi-second response.
const QUIET_MS = 100;
const FAILSAFE_MS = 2500;
// Shortened quiet window used when the consumer's `quietContextSignal`
// is truthy at bump time — that signal means "I have first-hand evidence
// the visible async typesetting (the markdown renderer's
// shiki / katex / mermaid) is done." We still want ONE frame of
// contentRO silence after the last event so the post-typesetting layout
// has time to settle, but the conservative 100ms wait that defends
// against late typesetting is no longer needed. 16ms ≈ one rAF tick.
//
// CRITICAL: the shortened window is only sound once the SURFACE has
// stopped moving. `quietContextSignal` reports the renderer's async
// state — it is blind to the engine's estimate→measure cascade, which mounts
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
// the engine's estimate→measure cascade still in flight. Absolute px, not
// viewport-relative: the threshold is human-perceptible scroll
// displacement, which does not scale with viewport height. The engine
// places unmeasured rows at floor-biased kind estimates
// (ROW_KIND_ESTIMATE_PX / ESTIMATED_ROW_SIZE, see
// components/chat/timelineSizePriors.svelte.ts),
// so each per-row correction is |measured − floor| — tens-to-hundreds of
// px for any real multi-line chat row, comfortably clear of this floor.
const WARMUP_SETTLE_EPSILON_PX = 8;

function roundCssPx(value: number): number {
  return Math.round(value * 100) / 100;
}

// Everything the pipeline needs from the controller: the observed
// elements, the reactive flags it drives or traces, the geometry reads,
// the resolver snapshot, and the two effect surfaces a delivery may
// touch (the writeScrollTop chokepoint and the spring instance).
export interface ContentObserverDeps {
  getScrollEl(): HTMLElement | undefined;
  getContentEl(): HTMLElement | undefined;
  /** True when the consumer delivers ContentGeometrySamples itself
   * (`externalContentGeometry` option) — attach() then creates no RO. */
  hasExternalGeometrySource(): boolean;
  /** Normalized per-fire animation mode (anything but 'spring' is 'instant'). */
  /** Live content still arriving — liveness only, never physics. */
  liveContentActive(): boolean;
  /**
   * Consumer's typesetting-settled signal, read live off the options
   * object per call (like the sibling options — a consumer that assigns
   * `options.quietContextSignal` after construction is honored). "Option
   * absent" and "signal falsy" are distinct states for the quiet-window
   * gate: absent always arms the conservative window, falsy arms nothing.
   */
  getQuietContextSignal(): (() => boolean) | undefined;
  /** Reactive warm flag (controller-owned $state; consumers read isWarm). */
  warm(): boolean;
  setWarm(next: boolean): void;
  /** Reactive warm-reason flag (controller-owned $state; consumers read
   * warmReason). Reporting only — see `WarmReason`. */
  setWarmReason(reason: WarmReason): void;
  /** Intent flag: "we want to be glued to the bottom" (isAtBottomState). */
  isAtBottom(): boolean;
  setIsAtBottom(next: boolean): void;
  escaped(): boolean;
  pauseDepth(): number;
  isNearBottom(): boolean;
  targetScrollTop(): number;
  refreshIsNearBottom(): number;
  /**
   * Read-free bottom geometry for an authoritative virtualizer sample.
   * Computes target = content height - content-box viewport height and
   * pairs it with cached scrollTop. Null when that cached position cannot
   * be trusted. The caller then takes the real-read path.
   */
  contentGeometryForSample(
    height: number,
    viewportHeight: number,
  ): { target: number; scrollTop: number } | null;
  /** Publish the exact bottom target carried by an external geometry sample. */
  cacheExternalBottomTarget(height: number, viewportHeight: number): void;
  writeScrollTop(caller: ScrollWriteCaller, value: number): void;
  resolverStateSnapshot(): ResolverState;
  /** OS prefers-reduced-motion OR the app's low-power setting. */
  prefersReducedMotion(): boolean;
  /** Secondary consumers run after the controller has applied this sample. */
  contentGeometryProcessed(scrollable: boolean): void;
  /** Narrowed to what a delivery decision may drive on the spring. */
  spring: Pick<SpringChase, 'structuralAppendPending' | 'snapOscillationToBottom' | 'markTargetChanged' | 'start'>;
}

export interface ContentObserver {
  /** Start observing the controller's current contentEl (no-op without RO
   * support, and no-op under an external geometry source). */
  attach(): void;
  /** Disconnect the RO and reset all classification + warm-up state (warm → false). */
  detach(): void;
  /** External-source entry: one engine-sourced content-geometry sample
   * (TimelineVirtualizer post-flush) into the same pipeline an RO
   * delivery takes. Requires the option AND an attached scroller —
   * both breaches are loud (see deliverSample). */
  deliverSample(sample: ContentGeometrySample): void;
  /** Reset the warm-up gate: sync-pin mode until quiet/failsafe fires again. */
  beginWarmup(): void;
  /** Force warm without waiting for quiescence (already-settled surfaces). */
  skipWarmup(): void;
  /** Re-evaluate the quiet window when the consumer's settle signal flips. */
  notifyQuietContextSignalChanged(): void;
  /**
   * One-shot sample for the intent machine's scroll classification:
   * "is this scroll event correlated with a content resize?" Reads the
   * classification flag and consumes one unit of the untagged-scroll
   * budget.
   */
  sampleResizeCorrelation(): boolean;
  /** Raw resizeDifference read for trace payloads only. */
  resizeDifferenceNow(): number;
  /**
   * Open the pinned-remeasure settle window: the controller just wrote a
   * DISPLACED engine.anchorRedirect (a large remeasure burst threw a
   * pinned viewport off the bottom), so height deltas for the next
   * PINNED_REMEASURE_SETTLE_WINDOW_MS classify as layout correction and
   * sync-pin instead of gliding.
   */
  openPinnedRemeasureSettleWindow(): void;
  /**
   * Stamp a synthetic RO-correlation before an out-of-content instant
   * pin (composer/host geometry), so the resulting scroll event is
   * classified as layout, not user input.
   */
  stampSyntheticResizeCorrelation(): void;
}

export function createContentObserver(deps: ContentObserverDeps): ContentObserver {
  let contentRO: ResizeObserver | undefined;
  let previousHeight: number | undefined;
  let previousWidth: number | undefined;
  let previousViewportHeight: number | undefined;
  let contentReflowSettleUntil = 0;
  let pinnedRemeasureSettleUntil = 0;
  // 0 = inactive. Armed by beginWarmup (every cold edge arms the warm
  // gate), cleared by skipWarmup, detach, and the first delivery that
  // observes live content or an armed structural append.
  let coldLoadSettleUntil = 0;
  let resizeDifference = 0;
  let resizeClearTimer: ReturnType<typeof setTimeout> | null = null;
  let resizeCorrelatedUntaggedScrollBudget = 0;

  let quietTimer: ReturnType<typeof setTimeout> | null = null;
  let failsafeTimer: ReturnType<typeof setTimeout> | null = null;
  let hasFirstContentRO = false;
  // Engine-source settle evidence from the most recent sample: the mount
  // window is fully measured AND every first measurement landed within
  // WARMUP_SETTLE_EPSILON_PX of its estimate (a priors-hit revisit). Lets
  // the warm gate reveal without waiting out a quiet window — and lets
  // `notifyQuietContextSignalChanged` do the same when the typesetting
  // signal is what arrives last. RO-sourced deliveries never set it.
  let lastSettleEvidence = false;
  // Magnitude of the most recent contentRO height change, shared by both
  // quiet-timer arming sites (`bumpQuietTimer`, `notifyQuietContextSignalChanged`)
  // so the shortened window stays geometry-gated. Reset to +Infinity each
  // warm-up cycle: geometry is assumed to be moving until a small delta is
  // actually observed, so the first fire (which has no baseline) and any
  // large correction both hold us on the conservative window.
  let lastContentHeightDelta = Number.POSITIVE_INFINITY;

  // Shared by processSample and stampSyntheticResizeCorrelation: clear
  // resizeDifference (and the untagged-scroll budget) one timer + one rAF
  // after the write, but only if no newer stamp replaced the sentinel in
  // the meantime. The delay lets the scroll event fired by the paired
  // layout change observe resizeDifference !== 0 and classify as layout.
  function scheduleResizeDifferenceClear(sentinel: number): void {
    if (resizeClearTimer) clearTimeout(resizeClearTimer);
    resizeClearTimer = setTimeout(() => {
      requestAnimationFrame(() => {
        if (resizeDifference === sentinel) {
          resizeDifference = 0;
          resizeCorrelatedUntaggedScrollBudget = 0;
        }
      });
    }, RESIZE_CLEAR_PADDING_MS);
  }

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

  function markWarm(reason: 'quiet' | 'failsafe' | 'settled'): void {
    if (deps.warm()) return;
    deps.setWarm(true);
    deps.setWarmReason(reason);
    clearWarmupTimers();
    if (isUiRenderTraceEnabled()) trace(`scroll.warmup.${reason}`, () => ({
      isAtBottomState: deps.isAtBottom(),
      escapedFromLockState: deps.escaped(),
      pauseDepth: deps.pauseDepth(),
    }));
  }

  function beginWarmup(): void {
    clearWarmupTimers();
    deps.setWarm(false);
    deps.setWarmReason(null);
    hasFirstContentRO = false;
    lastContentHeightDelta = Number.POSITIVE_INFINITY;
    lastSettleEvidence = false;
    // ONLY arm the failsafe. The quiet timer is armed by `bumpQuietTimer`
    // on the FIRST content-geometry delivery — gating the "quiet" signal
    // on actual geometry evidence is what keeps an arm made against a
    // surface that has not reported yet from opening at QUIET_MS with no
    // cascade to have waited out. The failsafe still bounds the worst
    // case (slow shiki / mermaid / KaTeX typesetting that keeps
    // deliveries firing for > FAILSAFE_MS).
    //
    // That gate covers "no geometry source yet", NOT "no content yet".
    // Under an external geometry source an EMPTY mount window still
    // delivers a (zero-height) sample, which is indistinguishable here
    // from a real fire — so a chat pane armed at the switch edge while
    // its slice fetch is in flight legitimately warms ~QUIET_MS later,
    // against nothing. Re-closing the gate for the rows that arrive
    // afterwards is the pane data layer's job, not a heuristic here:
    // see `PaneScrollController.armWarmup` (stores/threadPaneShared.ts),
    // which the initial-slice application calls synchronously with the
    // item mutation.
    failsafeTimer = setTimeout(() => markWarm('failsafe'), FAILSAFE_MS);
    // Every warm-up arm is a cold edge (attach, restore forceStick, the
    // slice application's armWarmup): open the cold-load settle window so
    // post-warm cascade/sync growth sync-pins. See COLD_LOAD_SETTLE_MAX_MS.
    coldLoadSettleUntil = nowMs() + COLD_LOAD_SETTLE_MAX_MS;
    // DELIBERATELY not reset here: previousHeight/previousWidth (the
    // resize-correlation baseline). A re-arm on a surface that stays
    // attached (same-thread reload, retry) keeps the last observed
    // geometry as its baseline, so the first delivery after the re-arm
    // computes a real delta instead of a first-observation zero. Only
    // detach clears them — the element is gone, so the baseline is too.
    // Flagged 2026-08-29 (lifecycle-edge sweep): reviewed, ambiguity is
    // real but both readers tolerate a stale baseline for one delivery.
  }

  function skipWarmup(): void {
    if (!deps.warm()) {
      deps.setWarm(true);
      deps.setWarmReason('skip');
      clearWarmupTimers();
    }
    // Placeholder → materialized: no cascade to settle, and the first
    // growth is the optimistic send + response, which must glide.
    coldLoadSettleUntil = 0;
  }

  // Pick the quiet window for an arm. The shortened SETTLED_QUIET_MS is
  // only sound once the surface has stopped moving in large steps; while
  // the engine's estimate→measure cascade is still growing scrollHeight we use
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
    if (deps.warm()) return;
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
    const quietContextSignal = deps.getQuietContextSignal();
    if (!quietContextSignal) {
      quietTimer = setTimeout(() => markWarm('quiet'), QUIET_MS);
      return;
    }
    const settled = quietContextSignal();
    if (!settled) {
      quietTimer = null;
      return;
    }
    quietTimer = setTimeout(() => markWarm('quiet'), quietWindowForGeometry());
  }

  function notifyQuietContextSignalChanged(): void {
    const settled = deps.getQuietContextSignal()?.() ?? false;
    const haveTimer = quietTimer !== null;
    const warm = deps.warm();
    let outcome: 'armed' | 'rearmed' | 'disarmed' | 'noop_warm' | 'noop_no_ro' | 'noop_signal_falsy';
    if (warm) outcome = 'noop_warm';
    else if (!settled) outcome = haveTimer ? 'disarmed' : 'noop_signal_falsy';
    else if (!haveTimer && !hasFirstContentRO) outcome = 'noop_no_ro';
    else if (haveTimer) outcome = 'rearmed';
    else outcome = 'armed';
    if (isUiRenderTraceEnabled()) trace('scroll.warmup.signalChanged', () => ({
      outcome,
      settled,
      haveTimer,
      warm,
    }));
    if (outcome === 'disarmed') {
      // The signal was withdrawn while a quiet timer was armed. This
      // happens with presence-based signals (a ChatMarkdown mounted after
      // the timer armed): the armed window was licensed by "no late
      // typesetting wave is coming" and the mount revoked that license.
      // Only geometry-quiet-with-signal (or the failsafe) may open the
      // gate now.
      if (quietTimer) clearTimeout(quietTimer);
      quietTimer = null;
      return;
    }
    if (outcome === 'noop_warm' || outcome === 'noop_no_ro' || outcome === 'noop_signal_falsy') return;
    // Engine-source settle evidence supersedes the quiet wait: everything
    // mounted has measured within epsilon of its estimate (priors-hit
    // revisit), and the typesetting signal just confirmed no late wave is
    // coming — the two facts the quiet window exists to wait for.
    if (lastSettleEvidence) {
      markWarm('settled');
      return;
    }
    if (quietTimer) clearTimeout(quietTimer);
    // Geometry-gated like bumpQuietTimer: if the surface was still moving in
    // large steps at the last contentRO, the settle signal flipping does not
    // license an early reveal — the cascade outlasts the settle signal.
    quietTimer = setTimeout(() => markWarm('quiet'), quietWindowForGeometry());
  }

  // One content-geometry delivery through the pipeline, whichever source
  // produced it: (height, width) of the content, plus engine-source-only
  // per-row settle evidence. RO-sourced deliveries pass no `settle`.
  function processSample(
    nextHeight: number,
    nextWidth: number,
    settle?: Pick<
      ContentGeometrySample,
      'windowMeasured' | 'maxFirstMeasureCorrectionPx' | 'viewportHeight'
    >,
  ): void {
    const scrollEl = deps.getScrollEl();
    if (!scrollEl) return;
    if (settle?.viewportHeight !== undefined) {
      deps.cacheExternalBottomTarget(nextHeight, settle.viewportHeight);
    }
    // External virtualizer samples already carry the exact content-box
    // viewport, so their scrollability hint stays read-free. Native RO
    // deliveries run after layout and can read the real scroll range without
    // forcing another layout pass. Secondary consumers use this edge without
    // sampling hidden geometry on every streaming beat.
    const reportProcessedGeometry = (): void => {
      const viewportHeight = settle?.viewportHeight;
      deps.contentGeometryProcessed(
        viewportHeight !== undefined
          ? nextHeight - viewportHeight > 1
          : scrollEl.scrollHeight - scrollEl.clientHeight > 1,
      );
    };
    const prev = previousHeight;
    const prevWidth = previousWidth;
    previousHeight = nextHeight;
    previousWidth = nextWidth;
    // Engine-sourced deliveries carry the scroller's content-box height
    // from its RO entry. While it holds still, the viewport held still,
    // and the delta path below may answer from cached geometry instead
    // of forced-layout reads; any move (or an RO-sourced pipeline, which
    // never reports one) disqualifies this delivery from the cache.
    const nextViewportHeight = settle?.viewportHeight;
    const prevViewportHeight = previousViewportHeight;
    const viewportStable = nextViewportHeight !== undefined
      && nextViewportHeight === prevViewportHeight;
    const viewportChanged = nextViewportHeight !== undefined
      && prevViewportHeight !== undefined
      && nextViewportHeight !== prevViewportHeight;
    previousViewportHeight = nextViewportHeight;
    const widthChanged = prevWidth !== undefined
      && Math.abs(nextWidth - prevWidth) > CONTENT_REFLOW_WIDTH_EPSILON_PX;
    if (widthChanged) {
      contentReflowSettleUntil = nowMs() + CONTENT_REFLOW_SETTLE_WINDOW_MS;
    }

    // Every RO activity counts as "still settling" — reset the quiet
    // timer regardless of delta direction. The virtualizer's per-row
    // remeasurement, Streamdown's typesetting backfill, and
    // parseIncompleteMarkdown rebalance all fire multiple RO callbacks
    // in close succession during mount; we want warm to fire only
    // once they're done. The height delta keeps the shortened settle
    // window gated on geometry stability (undefined on the first fire,
    // which has no baseline — see bumpQuietTimer).
    bumpQuietTimer(prev === undefined ? undefined : nextHeight - prev);

    // Engine-source settled fast-path. The quiet window exists to wait out
    // two unknowns: unmeasured rows still correcting, and a late async-
    // typesetting wave. Settle evidence answers the first directly (every
    // mounted row measured, all within WARMUP_SETTLE_EPSILON_PX of their
    // estimates — the priors-hit revisit signature); the consumer's
    // typesetting signal answers the second. With both in hand there is
    // nothing left to wait for — reveal now instead of ~QUIET_MS later
    // (the zero-delta revisit never lowers lastContentHeightDelta, so the
    // geometry gate alone would hold the conservative window forever).
    // A cold mount's corrections are tens-to-hundreds of px, so it can
    // never qualify; late growth (mermaid, images) lands as a correction
    // and holds the gate exactly as before.
    if (settle !== undefined) {
      lastSettleEvidence =
        settle.windowMeasured &&
        settle.maxFirstMeasureCorrectionPx <= WARMUP_SETTLE_EPSILON_PX;
      if (lastSettleEvidence && !deps.warm()) {
        const quietContextSignal = deps.getQuietContextSignal();
        if (!quietContextSignal || quietContextSignal()) markWarm('settled');
      }
    }

    if (prev === undefined) {
      // First fire: snap to bottom synchronously so the initial paint
      // lands at the right place. Matches upstream's `initial` behavior
      // when isAtBottom starts true.
      const decision = resolveContentDelivery(deps.resolverStateSnapshot(), {
        kind: 'first',
        target: deps.targetScrollTop(),
      });
      if (isUiRenderTraceEnabled()) trace('scroll.contentRO.firstFire', () => ({
        nextHeight: Math.round(nextHeight),
        willPin: decision.write !== null,
        isAtBottomState: deps.isAtBottom(),
        escapedFromLockState: deps.escaped(),
        scrollTop: Math.round(scrollEl.scrollTop),
        scrollHeight: Math.round(scrollEl.scrollHeight),
        clientHeight: Math.round(scrollEl.clientHeight),
      }));
      if (decision.write) {
        deps.writeScrollTop(decision.write.caller, decision.write.value);
      }
      deps.refreshIsNearBottom();
      reportProcessedGeometry();
      return;
    }

    const delta = nextHeight - prev;
    const widthReflowActive = widthChanged || contentReflowSettleUntil > nowMs();
    // Genuinely new content retires the cold-load window for good: from
    // the first live stamp or armed structural append onward, growth is
    // the bottom advancing and glides own the pane. Checked per delivery
    // (not via a timer) so the delivery that carries the new content is
    // itself classified as content, not correction.
    if (
      coldLoadSettleUntil !== 0
      && (deps.liveContentActive() || deps.spring.structuralAppendPending())
    ) {
      coldLoadSettleUntil = 0;
    }
    const coldLoadSettleActive = coldLoadSettleUntil > nowMs();
    // One resolver input for both announcers of the same fact — "a
    // measurement-correction wave is in flight while pinned": the
    // displaced anchor-redirect's fixed window, and the cold-load settle
    // window. The raw signals are traced separately below.
    const pinnedRemeasureActive =
      pinnedRemeasureSettleUntil > nowMs() || coldLoadSettleActive;
    // Common cases include a virtualizer remeasuring a same-height row,
    // padding-bottom changes, or a CSS variable resolves to the same value.
    // No virtual content change means there is nothing to chase and no
    // scroll-event tagging is needed.
    if (delta === 0) {
      // A padding-only composer resize changes the content-box viewport
      // without changing virtual content height. Refresh cached scrollTop
      // now so the next stable read-free delivery cannot inherit a browser
      // clamp from the old viewport. The target itself is carried by the
      // next sample and needs no mutable offset to rebase.
      if (viewportChanged) deps.refreshIsNearBottom();
      if (widthChanged && isUiRenderTraceEnabled()) trace('scroll.contentRO.widthReflow', () => ({
        prevWidth: prevWidth === undefined ? null : roundCssPx(prevWidth),
        nextWidth: roundCssPx(nextWidth),
        widthDelta: prevWidth === undefined ? null : roundCssPx(nextWidth - prevWidth),
        widthReflowActive,
        settleWindowMs: CONTENT_REFLOW_SETTLE_WINDOW_MS,
        scrollTop: Math.round(scrollEl.scrollTop),
        scrollHeight: Math.round(scrollEl.scrollHeight),
        clientHeight: Math.round(scrollEl.clientHeight),
      }));
      reportProcessedGeometry();
      return;
    }
    resizeDifference = delta;
    // A browser scroll clamp (scrollHeight shrinking below
    // scrollTop + clientHeight, e.g. from a shrinking row measurement
    // reducing the virtualizer's spacer height) emits one
    // untagged scroll event correlated with this content resize. The
    // timer/rAF `resizeDifference` guard catches the normal ordering;
    // this one-event budget covers the clear racing ahead of the
    // scroll handler. Pending user intent still wins. (virtua's own
    // compensation writes used to be the main untagged producer here;
    // the engine's are routed through the controller and token-tagged.)
    resizeCorrelatedUntaggedScrollBudget = 1;

    // Geometry for the decision is read-free on the hot path. A
    // virtualizer delivery carries both content height and content-box
    // viewport height, whose difference is the DOM bottom target because
    // scroller padding is present on both sides of scrollHeight -
    // clientHeight. Cached scrollTop cannot have moved since the
    // controller's last real read. Scroll events, chokepoint writes, and
    // spring ticks all resync it. The forced-layout read this replaces
    // ran once per delivery inside the flush/RO window. Up to
    // four passes in a single 3-pane streaming frame, and the first
    // reader of a dirty frame pays the whole pending style recalc
    // (storm capture 2026-08-26). The cache answers only while the
    // viewport and wrap width held still. First fire, width reflow,
    // viewport resize, floored-short content, and RO-sourced pipelines
    // take the real reads below, which resync cached scrollTop. A shrink
    // the browser clamps ahead of us is seen here as overshoot and
    // resolved by the same overshoot-snap the real read produced; the
    // chokepoint's own fresh-read clamp still guards the actual write.
    const cachedGeometry = viewportStable && !widthChanged
      ? deps.contentGeometryForSample(nextHeight, nextViewportHeight)
      : null;
    let target: number;
    let scrollTopAtDelivery: number;
    if (cachedGeometry) {
      target = cachedGeometry.target;
      scrollTopAtDelivery = cachedGeometry.scrollTop;
    } else {
      // Refresh the geometric near-bottom flag BEFORE resolving so the
      // decision and its trace see the same post-resize geometry.
      deps.refreshIsNearBottom();
      // Cache the bottom target once per RO delivery. `targetScrollTop()`
      // reads `scrollHeight` + `clientHeight` (forced layout), and neither
      // changes across this synchronous callback — the only writes here
      // are to `scrollTop`, which don't affect them — so one read serves
      // the whole decision. Mirrors the spring tick's per-frame
      // `const target` discipline.
      target = deps.targetScrollTop();
      scrollTopAtDelivery = scrollEl.scrollTop;
    }
    // Every decision about this delivery — overshoot snap, stranded-
    // oscillation recovery, sync-pin vs spring, negative re-stick — is
    // made by the pure resolver (scroll/resolver.ts) over a sampled
    // snapshot; this callback only gathers observations and applies
    // effects. At most ONE scrollTop write leaves a delivery.
    const observation: ContentDeltaObservation = {
      kind: 'delta',
      delta,
      scrollTop: scrollTopAtDelivery,
      target,
      widthReflowActive,
      pinnedRemeasureActive,
      prefersReducedMotion: deps.prefersReducedMotion(),
    };
    const decision = resolveContentDelivery(deps.resolverStateSnapshot(), observation);
    if (isUiRenderTraceEnabled()) trace('scroll.contentRO', () => ({
      prev: Math.round(prev),
      next: Math.round(nextHeight),
      delta: Math.round(delta),
      overshootMagnitude: Math.round(Math.max(0, scrollTopAtDelivery - target)),
      distanceFromTarget: Math.round(Math.abs(scrollTopAtDelivery - target)),
      writeCaller: decision.write?.caller ?? null,
      startSpring: decision.startSpring,
      bumpTargetChanged: decision.bumpTargetChanged,
      oscillationRecovery: decision.oscillationRecovery,
      setIsAtBottom: decision.setIsAtBottom,
      isAtBottomState: deps.isAtBottom(),
      escapedFromLockState: deps.escaped(),
      pauseDepth: deps.pauseDepth(),
      isNearBottomState: deps.isNearBottom(),
      prevWidth: prevWidth === undefined ? null : roundCssPx(prevWidth),
      nextWidth: roundCssPx(nextWidth),
      widthDelta: prevWidth === undefined ? null : roundCssPx(nextWidth - prevWidth),
      widthChanged,
      widthReflowActive,
      pinnedRemeasureActive,
      coldLoadSettleActive,
      settleEvidence: settle === undefined ? null : lastSettleEvidence,
      liveContentActive: deps.liveContentActive(),
      structuralAppendSpringPending: deps.spring.structuralAppendPending(),
      scrollTop: Math.round(scrollTopAtDelivery),
      // Real DOM reads only on the read path: the cached path must stay
      // read-free even under UI_TRACE, or tracing re-adds the forced
      // layout it exists to diagnose. `target` carries the geometry.
      scrollHeight: cachedGeometry ? null : Math.round(scrollEl.scrollHeight),
      clientHeight: cachedGeometry ? null : Math.round(scrollEl.clientHeight),
      geometrySource: cachedGeometry ? 'cached' : 'read',
      target: Math.round(target),
    }));

    // Apply the decision. Order preserved from the legacy inline
    // logic: intent flip first (the write's trace payload reads it),
    // then the single write, then spring bookkeeping.
    if (decision.setIsAtBottom) deps.setIsAtBottom(true);
    if (decision.oscillationRecovery && decision.write) {
      // Route through the shared snap so this synchronous recovery and
      // the spring tick's rAF-side recovery cannot drift on the effect
      // body (see the FOOTGUN comment on spring.snapOscillationToBottom,
      // scroll/spring.ts). The
      // recovery runs here — same RO delivery as the regrow, before
      // paint — because rAF callbacks fire BEFORE ResizeObserver
      // callbacks within a frame, so the tick-side snap always lands
      // one frame late (bug-report-20260615T182227Z: ~37px one-frame
      // upward jolt on an above-viewport image row remeasure).
      deps.spring.snapOscillationToBottom(decision.write.caller, decision.write.value);
    } else if (decision.write) {
      deps.writeScrollTop(decision.write.caller, decision.write.value);
    }
    if (decision.startSpring) {
      deps.spring.markTargetChanged();
      deps.spring.start();
    } else if (decision.bumpTargetChanged) {
      // Spring is the single writer mid-chase and the sync write was
      // suppressed, but the target moved — without the bump the retain
      // window could lapse between chunks and the spring would
      // arrive-and-stop while a follow-up chunk was on its way.
      deps.spring.markTargetChanged();
    }

    scheduleResizeDifferenceClear(delta);
    reportProcessedGeometry();
  }

  function attach(): void {
    // Under an external geometry source the consumer's virtualizer
    // delivers samples itself (deliverSample) — observing contentEl too
    // would double-deliver every height change.
    if (deps.hasExternalGeometrySource()) return;
    const contentEl = deps.getContentEl();
    if (!contentEl) return;
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (!entry || !deps.getContentEl()) return;
      processSample(entry.contentRect.height, entry.contentRect.width);
    });
    ro.observe(contentEl);
    contentRO = ro;
  }

  function deliverSample(sample: ContentGeometrySample): void {
    // The option and this entry point are two halves of one contract:
    // without `externalContentGeometry`, attach() also observes contentEl,
    // and the two sources feed conflicting heights (contentEl wraps more
    // than the virtualizer's spacer) into one pipeline — oscillating
    // deltas and spurious resolver writes at idle. Fail loudly instead.
    if (!deps.hasExternalGeometrySource()) {
      throw new Error(
        'deliverContentGeometry requires the externalContentGeometry option — ' +
          'without it the controller also observes contentEl and the two sources conflict',
      );
    }
    // A sample delivered while DETACHED is LOST: processSample returns on
    // the missing scrollEl, and the source's own dedupe will not offer
    // the same tuple twice — which is exactly how a populated first mount
    // ended up at scrollTop=0 under a sticky-bottom claim. The source
    // must therefore subscribe after attach
    // (TimelineVirtualizerHandle.subscribeContentGeometry), and after
    // that wiring the production path cannot reach this branch at all;
    // it stays as the contract's tripwire.
    //
    // Loud in dev/test, silent-but-reported in production: a throw
    // crossing back into a Svelte effect aborts the update batch that
    // called it, which is a worse failure than one lost sample.
    if (!deps.getScrollEl()) {
      const message =
        'deliverContentGeometry called while the scroll controller is detached — ' +
        'the sample is lost; subscribe to the geometry source AFTER attach()';
      if (import.meta.env.DEV || import.meta.env.MODE === 'test') {
        throw new Error(message);
      }
      console.error(message);
      reportFrontendDiagnostic(message);
      return;
    }
    processSample(sample.height, sample.width, sample);
  }

  function detach(): void {
    contentRO?.disconnect();
    contentRO = undefined;
    if (resizeClearTimer) {
      clearTimeout(resizeClearTimer);
      resizeClearTimer = null;
    }
    clearWarmupTimers();
    deps.setWarm(false);
    deps.setWarmReason(null);
    resizeDifference = 0;
    resizeCorrelatedUntaggedScrollBudget = 0;
    previousHeight = undefined;
    previousWidth = undefined;
    previousViewportHeight = undefined;
    contentReflowSettleUntil = 0;
    pinnedRemeasureSettleUntil = 0;
    coldLoadSettleUntil = 0;
    lastSettleEvidence = false;
    // Also drop the warm-up baseline: a stale hasFirstContentRO would let
    // a post-detach notifyQuietContextSignalChanged arm a quiet timer and
    // flip warm on a detached controller.
    hasFirstContentRO = false;
    lastContentHeightDelta = Number.POSITIVE_INFINITY;
  }

  return {
    attach,
    detach,
    deliverSample,
    beginWarmup,
    skipWarmup,
    notifyQuietContextSignalChanged,
    sampleResizeCorrelation: () => {
      const correlated = resizeDifference !== 0 || resizeCorrelatedUntaggedScrollBudget > 0;
      if (resizeCorrelatedUntaggedScrollBudget > 0) resizeCorrelatedUntaggedScrollBudget -= 1;
      return correlated;
    },
    resizeDifferenceNow: () => resizeDifference,
    openPinnedRemeasureSettleWindow: () => {
      pinnedRemeasureSettleUntil = nowMs() + PINNED_REMEASURE_SETTLE_WINDOW_MS;
    },
    stampSyntheticResizeCorrelation: () => {
      // Stamp resizeDifference BEFORE the caller writes scrollTop so the
      // resulting scroll event is treated as RO-correlated, not
      // user-driven. Without this, a textarea-shrink could cause the
      // scroll handler's re-stick path to flip isAtBottom in a way that
      // surprises the user.
      resizeDifference = 1;
      scheduleResizeDifferenceClear(1);
    },
  };
}
