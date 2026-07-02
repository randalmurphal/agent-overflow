// Content-geometry observation pipeline for the sticky-bottom
// controller (useStickToBottom): the single ResizeObserver on
// contentEl, the warm-up (quiescence) gate over its deliveries, and the
// resize-classification state that lets the intent machine tell
// layout-induced scroll events from user gestures.
//
// Each RO delivery is gathered here, decided by the pure resolver
// (scroll/resolver.ts), and applied here — through the controller's
// writeScrollTop chokepoint and spring instance, both reached via the
// host interface — so everything about "a content delivery" reads in
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
import type { ScrollWriteCaller } from './types';
import { isUiRenderTraceEnabled } from '../uiRenderTrace';

// ResizeObserver width jitter below half a CSS pixel is usually rounding
// noise. Wider changes mean the content column reflowed; any paired height
// delta is layout correction, not new live transcript content.
const CONTENT_REFLOW_WIDTH_EPSILON_PX = 0.5;
// Width and height can arrive in separate ResizeObserver deliveries. Keep
// the layout-correction classification alive briefly so a width-only fire
// followed by renderer height settle still sync-pins.
const CONTENT_REFLOW_SETTLE_WINDOW_MS = 250;

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

function roundCssPx(value: number): number {
  return Math.round(value * 100) / 100;
}

// Everything the pipeline needs from the controller: the observed
// elements, the reactive flags it drives or traces, the geometry reads,
// the resolver snapshot, and the two effect surfaces a delivery may
// touch (the writeScrollTop chokepoint and the spring instance).
export interface ContentObserverHost {
  getScrollEl(): HTMLElement | undefined;
  getContentEl(): HTMLElement | undefined;
  /** Normalized per-fire animation mode (anything but 'spring' is 'instant'). */
  animationMode(): 'spring' | 'instant';
  /** Consumer's typesetting-settled signal (options.quietContextSignal). */
  quietContextSignal?: () => boolean;
  /** Reactive warm flag (controller-owned $state; consumers read isWarm). */
  warm(): boolean;
  setWarm(next: boolean): void;
  isAtBottom(): boolean;
  setIsAtBottom(next: boolean): void;
  escaped(): boolean;
  pauseDepth(): number;
  isNearBottom(): boolean;
  targetScrollTop(): number;
  refreshIsNearBottom(): number;
  writeScrollTop(caller: ScrollWriteCaller, value: number): void;
  resolverStateSnapshot(): ResolverState;
  prefersReducedMotion(): boolean;
  spring: SpringChase;
}

export interface ContentObserver {
  /** Start observing the host's current contentEl (no-op without RO support). */
  observeContent(): void;
  /** Disconnect the RO and reset all classification + warm-up state (warm → false). */
  detach(): void;
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
  /** True while the width-reflow settle window is open (virtua compensation input). */
  widthReflowActive(): boolean;
  /**
   * Stamp a synthetic RO-correlation before an out-of-content instant
   * pin (composer/host geometry), so the resulting scroll event is
   * classified as layout, not user input.
   */
  stampSyntheticResizeCorrelation(): void;
}

export function createContentObserver(host: ContentObserverHost): ContentObserver {
  let contentRO: ResizeObserver | undefined;
  let previousHeight: number | undefined;
  let previousWidth: number | undefined;
  let contentReflowSettleUntil = 0;
  let resizeDifference = 0;
  let resizeClearTimer: ReturnType<typeof setTimeout> | null = null;
  let resizeCorrelatedUntaggedScrollBudget = 0;

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
    if (host.warm()) return;
    host.setWarm(true);
    clearWarmupTimers();
    if (isUiRenderTraceEnabled()) trace(`scroll.warmup.${reason}`, () => ({
      isAtBottomState: host.isAtBottom(),
      escapedFromLockState: host.escaped(),
      pauseDepth: host.pauseDepth(),
    }));
  }

  function beginWarmup(): void {
    clearWarmupTimers();
    host.setWarm(false);
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

  function skipWarmup(): void {
    if (!host.warm()) {
      host.setWarm(true);
      clearWarmupTimers();
    }
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
    if (host.warm()) return;
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
    if (!host.quietContextSignal) {
      quietTimer = setTimeout(() => markWarm('quiet'), QUIET_MS);
      return;
    }
    const settled = host.quietContextSignal();
    if (!settled) {
      quietTimer = null;
      return;
    }
    quietTimer = setTimeout(() => markWarm('quiet'), quietWindowForGeometry());
  }

  function notifyQuietContextSignalChanged(): void {
    const settled = host.quietContextSignal?.() ?? false;
    const haveTimer = quietTimer !== null;
    const warm = host.warm();
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

  function observeContent(): void {
    const contentEl = host.getContentEl();
    if (!contentEl) return;
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver((entries) => {
      const entry = entries[0];
      const scrollEl = host.getScrollEl();
      if (!entry || !host.getContentEl() || !scrollEl) return;
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
        const decision = resolveContentDelivery(host.resolverStateSnapshot(), {
          kind: 'first',
          target: host.targetScrollTop(),
        });
        if (isUiRenderTraceEnabled()) trace('scroll.contentRO.firstFire', () => ({
          nextHeight: Math.round(nextHeight),
          willPin: decision.write !== null,
          isAtBottomState: host.isAtBottom(),
          escapedFromLockState: host.escaped(),
          scrollTop: Math.round(scrollEl.scrollTop),
          scrollHeight: Math.round(scrollEl.scrollHeight),
          clientHeight: Math.round(scrollEl.clientHeight),
        }));
        if (decision.write) {
          host.writeScrollTop(decision.write.caller, decision.write.value);
        }
        host.refreshIsNearBottom();
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
          scrollTop: Math.round(scrollEl.scrollTop),
          scrollHeight: Math.round(scrollEl.scrollHeight),
          clientHeight: Math.round(scrollEl.clientHeight),
        }));
        return;
      }
      resizeDifference = delta;
      // A browser scroll clamp (scrollHeight shrinking below
      // scrollTop + clientHeight, e.g. from virtua's row-height
      // corrections mutating styles in its own RO callbacks) emits one
      // untagged scroll event correlated with this content resize. The
      // timer/rAF `resizeDifference` guard catches the normal ordering;
      // this one-event budget covers the clear racing ahead of the
      // scroll handler. Pending user intent still wins. (virtua's own
      // compensation writes used to be the main untagged producer here;
      // those are now routed through the controller and token-tagged.)
      resizeCorrelatedUntaggedScrollBudget = 1;

      // Refresh the geometric near-bottom flag BEFORE resolving so the
      // decision and its trace see the same post-resize geometry.
      host.refreshIsNearBottom();
      // Cache the bottom target once per RO delivery. `targetScrollTop()`
      // reads `scrollHeight` + `clientHeight` (forced layout), and neither
      // changes across this synchronous callback — the only writes here
      // are to `scrollTop`, which don't affect them — so one read serves
      // the whole decision. Mirrors the spring tick's per-frame
      // `const target` discipline.
      const target = host.targetScrollTop();
      const scrollTopAtDelivery = scrollEl.scrollTop;
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
        animationMode: host.animationMode(),
        structuralAppendPending: host.spring.structuralAppendPending(),
        prefersReducedMotion: host.prefersReducedMotion(),
      };
      const decision = resolveContentDelivery(host.resolverStateSnapshot(), observation);
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
        isAtBottomState: host.isAtBottom(),
        escapedFromLockState: host.escaped(),
        pauseDepth: host.pauseDepth(),
        isNearBottomState: host.isNearBottom(),
        prevWidth: prevWidth === undefined ? null : roundCssPx(prevWidth),
        nextWidth: roundCssPx(nextWidth),
        widthDelta: prevWidth === undefined ? null : roundCssPx(nextWidth - prevWidth),
        widthChanged,
        widthReflowActive,
        structuralAppendSpringPending: observation.structuralAppendPending,
        scrollTop: Math.round(scrollTopAtDelivery),
        scrollHeight: Math.round(scrollEl.scrollHeight),
        clientHeight: Math.round(scrollEl.clientHeight),
        target: Math.round(target),
      }));

      // Apply the decision. Order preserved from the legacy inline
      // logic: intent flip first (the write's trace payload reads it),
      // then the single write, then spring bookkeeping.
      if (decision.setIsAtBottom) host.setIsAtBottom(true);
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
        host.spring.snapOscillationToBottom(decision.write.caller, decision.write.value);
      } else if (decision.write) {
        host.writeScrollTop(decision.write.caller, decision.write.value);
      }
      if (decision.startSpring) {
        host.spring.markTargetChanged();
        host.spring.start();
      } else if (decision.bumpTargetChanged) {
        // Spring is the single writer mid-chase and the sync write was
        // suppressed, but the target moved — without the bump the retain
        // window could lapse between chunks and the spring would
        // arrive-and-stop while a follow-up chunk was on its way.
        host.spring.markTargetChanged();
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

  function detach(): void {
    contentRO?.disconnect();
    contentRO = undefined;
    if (resizeClearTimer) {
      clearTimeout(resizeClearTimer);
      resizeClearTimer = null;
    }
    clearWarmupTimers();
    host.setWarm(false);
    resizeDifference = 0;
    resizeCorrelatedUntaggedScrollBudget = 0;
    previousHeight = undefined;
    previousWidth = undefined;
    contentReflowSettleUntil = 0;
  }

  return {
    observeContent,
    detach,
    beginWarmup,
    skipWarmup,
    notifyQuietContextSignalChanged,
    sampleResizeCorrelation: () => {
      const correlated = resizeDifference !== 0 || resizeCorrelatedUntaggedScrollBudget > 0;
      if (resizeCorrelatedUntaggedScrollBudget > 0) resizeCorrelatedUntaggedScrollBudget -= 1;
      return correlated;
    },
    resizeDifferenceNow: () => resizeDifference,
    widthReflowActive: () => contentReflowSettleUntil > nowMs(),
    stampSyntheticResizeCorrelation: () => {
      // Stamp resizeDifference BEFORE the caller writes scrollTop so the
      // resulting scroll event is treated as RO-correlated, not
      // user-driven. Without this, a textarea-shrink could cause the
      // scroll handler's re-stick path to flip isAtBottom in a way that
      // surprises the user.
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
    },
  };
}
