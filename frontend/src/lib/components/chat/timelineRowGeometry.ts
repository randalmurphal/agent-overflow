import type { Action } from 'svelte/action';
import {
  normalizeTimelineRowGeometryKey,
  type TimelineRowGeometryKey,
} from '../../stores/threadRowUiState.svelte';

export const ROW_GEOMETRY_CONTENT_ATTR = 'data-row-geometry-content';

// Bound a stale reservation if the old measured height no longer matches
// the remounted row. 750ms covers the normal image/markdown remount settle
// path without leaving a long-lived blank gap for legitimate shrink cases.
const ROW_GEOMETRY_STALE_RESERVATION_RELEASE_MS = 750;

export interface TimelineRowGeometryReservationParams extends TimelineRowGeometryKey {}

export interface TimelineRowGeometryCache {
  cachedTimelineRowHeight(key: TimelineRowGeometryKey): number | undefined;
  rememberTimelineRowHeight(key: TimelineRowGeometryKey, height: number): void;
}

// Diagnostic tap on every reservation state transition, for the ui-render
// trace (Ctrl+Shift+B captures). The hook is optional and every emit site uses
// `trace?.({...})`, so a build without a hook never evaluates the event
// argument — which is why MessageTimeline passes `undefined` (not a no-op
// hook) when the trace surface is not compiled in. That presence contract
// matters because some taps are hot: `skip-settled` fires for the streaming
// tail row on every streamed beat (its signature embeds updatedAt/summary
// length, so the applyParams value-equal fast path does not absorb it) and
// `hold` fires per ResizeObserver delivery while a remounted row settles.
export type TimelineRowGeometryTraceAction =
  | 'reserve'             // cold-mount floor written (cache hit)
  | 'skip-settled'        // settled-height gate blocked a re-floor
  | 'skip-no-cache'       // cold-mount path missed the cache; any held floor released
  | 'hold'                // measured below the floor; reservation held
  | 'settle'              // first committed measured height this mount (gate closes)
  | 'release-measured'    // floor released after measuring at/above it
  | 'release-stale'       // 750ms backstop released a floor that never re-grew
  | 'release-null-params' // params failed normalization; reservation dropped
  | 'rebind'              // content element swapped under a living action (gate re-opens)
  | 'destroy';            // action destroyed (row unmounted)

export interface TimelineRowGeometryTraceEvent {
  action: TimelineRowGeometryTraceAction;
  // Global per-action-instance sequence: a repeated key under a NEW mountSeq is
  // a virtua remount; the same mountSeq cycling reserve/release is churn on one
  // living row. This is the discriminator the ring buffer's contentRO deltas
  // cannot provide.
  mountSeq: number;
  key: string;
  itemId?: string;
  signature?: string;
  width?: number;
  cachedHeight?: number;
  reservedHeight?: number;
  measuredHeight?: number;
  releasedHeight?: number;
  settled?: boolean;
}

export type TimelineRowGeometryTraceHook = (event: TimelineRowGeometryTraceEvent) => void;

let nextRowGeometryMountSeq = 1;

interface RowReservationState {
  row: HTMLElement;
  content: HTMLElement | null;
  params: TimelineRowGeometryReservationParams | null;
  initialMinHeight: string;
  reservedHeight: number;
  lastMeasuredHeight: number;
  lastMeasuredWidth: number;
  releaseTimer: ReturnType<typeof setTimeout> | null;
  mountSeq: number;
  // Per-mount latch, set at the rememberMeasuredHeight chokepoint once this
  // content element has committed a height (normal settle, no-reservation
  // measure, or stale-timer release — that last path may commit below the
  // floor). Gates applyParams so an already-settled, still-visible row is never
  // re-floored (the settle "twitch"; full rationale at the gate). Reset in
  // bindContentElement when a different content element is bound.
  hasSettledHeight: boolean;
}

export function createTimelineRowGeometryReservation(
  cache: TimelineRowGeometryCache,
  trace?: TimelineRowGeometryTraceHook,
): Action<HTMLElement, TimelineRowGeometryReservationParams> {
  const statesByContent = new WeakMap<Element, RowReservationState>();
  let observer: ResizeObserver | null = null;

  function ensureObserver(): ResizeObserver | null {
    if (observer || typeof ResizeObserver === 'undefined') return observer;
    observer = new ResizeObserver((entries) => {
      for (const entry of entries) {
        const state = statesByContent.get(entry.target);
        if (!state) continue;
        // Height stays fractional end-to-end: the cache stores the exact
        // measured value so a cold-mount floor equals the row's natural
        // height and releases with zero residue. Rounding here used to
        // stack +0.x px per remounted row into a visible totalSize pulse
        // (docs/architecture/settle-flicker-analysis.md). Width is only a
        // cache KEY — it stays rounded to match the integer widths used by
        // reserve-time lookups.
        handleMeasuredHeight(
          state,
          entry.contentRect.height,
          Math.round(entry.contentRect.width),
        );
      }
    });
    return observer;
  }

  return (row: HTMLElement, initialParams: TimelineRowGeometryReservationParams) => {
    const state: RowReservationState = {
      row,
      content: null,
      params: null,
      initialMinHeight: row.style.minHeight,
      reservedHeight: 0,
      lastMeasuredHeight: 0,
      lastMeasuredWidth: 0,
      releaseTimer: null,
      mountSeq: nextRowGeometryMountSeq++,
      hasSettledHeight: false,
    };

    bindContentElement(state);
    applyParams(state, initialParams);

    return {
      update(nextParams: TimelineRowGeometryReservationParams) {
        bindContentElement(state);
        applyParams(state, nextParams);
      },
      destroy() {
        trace?.({
          action: 'destroy',
          mountSeq: state.mountSeq,
          key: state.params?.key ?? '',
          releasedHeight: state.reservedHeight,
          settled: state.hasSettledHeight,
        });
        clearReservationTimer(state);
        releaseReservation(state, false);
        if (state.content) {
          observer?.unobserve(state.content);
          statesByContent.delete(state.content);
        }
        state.content = null;
      },
    };
  };

  function bindContentElement(state: RowReservationState): void {
    const nextContent = directRowGeometryContent(state.row);
    if (nextContent === state.content) return;

    if (state.content) {
      trace?.({
        action: 'rebind',
        mountSeq: state.mountSeq,
        key: state.params?.key ?? '',
        settled: state.hasSettledHeight,
      });
      observer?.unobserve(state.content);
      statesByContent.delete(state.content);
    }

    state.content = nextContent;
    // A newly bound content element is a fresh mount surface — allow it one
    // cold-mount floor before the settled-height gate re-engages. On the initial
    // bind this is a redundant no-op (the constructor already inited false); its
    // only load-bearing trigger is a content-element swap under a living action,
    // which today's unconditional content div never produces (a true virtua
    // remount is a fresh action instance, not this reset). Kept for a future
    // conditional content wrapper.
    state.hasSettledHeight = false;
    if (!nextContent) return;
    statesByContent.set(nextContent, state);
    ensureObserver()?.observe(nextContent);
  }

  // KEEP THIS FREE OF SYNCHRONOUS LAYOUT READS. applyParams runs on every
  // action update — every reactive param change, which during streaming is
  // many times per second. An earlier strand fix read the row's own width
  // here with a synchronous getBoundingClientRect(); the forced reflow it
  // triggered each update drove a per-frame content-height oscillation, and
  // useStickToBottom's oscillation-snap recovery limit-cycled on it — a
  // sustained ±~16px scroll cycle that showed as the timeline text
  // "vibrating"/flickering, both idle and while streaming. Everything this
  // reservation needs about width/height arrives ASYNCHRONOUSLY via the
  // ResizeObserver (handleMeasuredHeight's contentRect). Read from there —
  // never with a sync layout query (getBoundingClientRect, offsetHeight/Width,
  // getComputedStyle on a laid-out element, scrollHeight) in this path.
  function applyParams(
    state: RowReservationState,
    nextParams: TimelineRowGeometryReservationParams,
  ): void {
    const current = state.params;
    // Fast path: timelineRowGeometryKey already emits a clean, integer-width,
    // de-duplicated key, so on an unchanged re-render nextParams is value-equal
    // to the stored (normalized) params. Compare the raw params first to skip
    // normalizeTimelineRowGeometryKey's allocation (two trims, a Set, and a
    // spread) — this action's `update` runs on every reactive param change,
    // many times per second during streaming.
    if (current && sameReservationParams(current, nextParams)) return;

    const normalized = normalizeTimelineRowGeometryKey(nextParams);
    if (!normalized) {
      trace?.({
        action: 'release-null-params',
        mountSeq: state.mountSeq,
        key: state.params?.key ?? '',
        releasedHeight: state.reservedHeight,
      });
      state.params = null;
      clearReservationTimer(state);
      releaseReservation(state, false);
      return;
    }

    // Fallback: raw params differed only in fields normalization touches
    // (whitespace, duplicate ownerItemIds, width rounding) yet still resolve to
    // the stored params — nothing to re-reserve.
    if (current && sameReservationParams(current, normalized)) return;

    state.params = normalized;
    // Captured for the trace taps below; the reset wipes it before they fire.
    const measuredBeforeReset = state.lastMeasuredHeight;
    state.lastMeasuredHeight = 0;
    state.lastMeasuredWidth = 0;
    clearReservationTimer(state);

    // Cold-mount floor ONLY. Once this row has committed a real measured height
    // this mount (hasSettledHeight), its natural size is known and must never be
    // overridden by the stale integer cached height: re-flooring an
    // already-settled, still-visible row is the settle "twitch". The shell
    // signature recomputes on each timelineRevision; while pinned to the bottom
    // the rendered window sits in that churn zone, so without this gate the
    // visible rows re-reserved on each wave (~9 waves/turn), each write nudging a
    // fractional-height row by the integer-vs-fractional delta — a 2-6px
    // content-box flutter (confirmed against a trace capture). The floor's only
    // legitimate job is bridging virtua's 56px estimate / async-render collapse
    // on a FRESH mount (c5c79d5a), before the first measure while hasSettledHeight
    // is false. Gate placed AFTER state.params advances above so a still-growing
    // settled row caches its new height under the current signature; hoisting it
    // above the normalize would cache under a stale signature — the cold-mount
    // miss this floor exists to prevent.
    if (state.hasSettledHeight) {
      trace?.({
        action: 'skip-settled',
        mountSeq: state.mountSeq,
        key: normalized.key,
        itemId: normalized.ownerItemIds[0],
        signature: normalized.signature,
        width: normalized.width,
        measuredHeight: measuredBeforeReset,
      });
      return;
    }

    const cachedHeight = cache.cachedTimelineRowHeight(normalized);
    if (!cachedHeight) {
      trace?.({
        action: 'skip-no-cache',
        mountSeq: state.mountSeq,
        key: normalized.key,
        itemId: normalized.ownerItemIds[0],
        signature: normalized.signature,
        width: normalized.width,
        releasedHeight: state.reservedHeight,
        measuredHeight: measuredBeforeReset,
      });
      releaseReservation(state, false);
      return;
    }

    trace?.({
      action: 'reserve',
      mountSeq: state.mountSeq,
      key: normalized.key,
      itemId: normalized.ownerItemIds[0],
      signature: normalized.signature,
      width: normalized.width,
      cachedHeight,
      reservedHeight: state.reservedHeight,
      measuredHeight: measuredBeforeReset,
    });
    state.reservedHeight = cachedHeight;
    state.row.style.minHeight = `${cachedHeight}px`;
    state.releaseTimer = setTimeout(() => {
      state.releaseTimer = null;
      trace?.({
        action: 'release-stale',
        mountSeq: state.mountSeq,
        key: state.params?.key ?? '',
        releasedHeight: state.reservedHeight,
        measuredHeight: state.lastMeasuredHeight,
      });
      releaseReservation(state, true);
    }, ROW_GEOMETRY_STALE_RESERVATION_RELEASE_MS);
  }

  function handleMeasuredHeight(
    state: RowReservationState,
    height: number,
    measuredWidth: number,
  ): void {
    const params = state.params;
    if (!params || height <= 0) return;
    state.lastMeasuredHeight = height;
    if (measuredWidth > 0) state.lastMeasuredWidth = measuredWidth;

    // Hold the reservation while the remounted row is still settling shorter
    // than what we reserved (image / markdown reflow). The applyParams timer
    // is the backstop if it never grows back. Do NOT mark the row settled here:
    // it has not yet reached its natural height, so the cold-mount floor must
    // stay eligible to re-arm.
    if (state.reservedHeight > 0 && height < state.reservedHeight) {
      trace?.({
        action: 'hold',
        mountSeq: state.mountSeq,
        key: params.key,
        measuredHeight: height,
        reservedHeight: state.reservedHeight,
      });
      return;
    }

    rememberMeasuredHeight(state);
    if (state.reservedHeight > 0) {
      trace?.({
        action: 'release-measured',
        mountSeq: state.mountSeq,
        key: params.key,
        measuredHeight: height,
        releasedHeight: state.reservedHeight,
      });
      releaseReservation(state, false);
    }
  }

  // Cache the height under the width the ResizeObserver actually measured it
  // at, NOT params.width. params.width is the surface width threaded through
  // props; it lags by a frame during a column-width reflow, so keying off it
  // caches a tall narrow-layout height under the new wide width — and the next
  // remount at the wide width reserves that too-tall height and strands the
  // timeline above the composer. contentRect.width is atomic with the height
  // just reported, so it is always the width this height is valid for. At a
  // steady width it equals params.width (zero horizontal padding between the
  // scroll surface's content box and data-row-geometry-content), so this only
  // diverges across a reflow.
  function rememberMeasuredHeight(state: RowReservationState): void {
    if (!state.params || state.lastMeasuredHeight <= 0) return;
    const width = state.lastMeasuredWidth > 0 ? state.lastMeasuredWidth : state.params.width;
    cache.rememberTimelineRowHeight({ ...state.params, width }, state.lastMeasuredHeight);
    // Trace only the gate-closing transition; steady-state growth measures are
    // already visible as timeline.row.resize events.
    if (!state.hasSettledHeight) {
      trace?.({
        action: 'settle',
        mountSeq: state.mountSeq,
        key: state.params.key,
        signature: state.params.signature,
        width,
        measuredHeight: state.lastMeasuredHeight,
        reservedHeight: state.reservedHeight,
      });
    }
    // Committing a real measured height means the row has rendered its natural
    // size this mount; the cold-mount floor gate (applyParams) now stays shut so
    // later signature churn can't re-floor a still-visible row. Single
    // chokepoint: covers normal settle, the no-reservation measure, and the
    // stale-timer release (releaseReservation → rememberMeasuredHeight).
    state.hasSettledHeight = true;
  }

  function releaseReservation(state: RowReservationState, rememberLastMeasured: boolean): void {
    if (state.reservedHeight === 0) return;
    state.row.style.minHeight = state.initialMinHeight;
    state.reservedHeight = 0;
    clearReservationTimer(state);

    if (rememberLastMeasured) {
      rememberMeasuredHeight(state);
    }
  }

  function clearReservationTimer(state: RowReservationState): void {
    if (!state.releaseTimer) return;
    clearTimeout(state.releaseTimer);
    state.releaseTimer = null;
  }
}

// Observe a scroll surface's CONTENT-box width and report each integer change
// through `onWidth`. Returns a cleanup that disconnects the observer.
//
// Content-box (ResizeObserver `contentRect.width`) is the ONLY width the
// row-geometry cache may key on: the reserve path threads this surface width
// into every row's geometry key, while the remember path keys off each row's
// own `contentRect.width` (see rememberMeasuredHeight and the
// data-row-geometry-content comment in MessageTimeline), and the two must
// agree. NEVER seed from getBoundingClientRect() / clientWidth here: those are
// border-box — they include the `scrollbar-gutter: stable both-edges`
// reservation, disagree with `contentRect` by the gutter width, and a second
// disagreeing source turns the width signal into a self-sustaining oscillation
// that re-renders every visible row forever (idle CPU/heap-churn incident
// 2026-06-26). One box, one source, asynchronous only — the same rule the
// applyParams comment above enforces for the per-row reservation path.
export function observeScrollSurfaceContentWidth(
  surface: Element,
  onWidth: (width: number) => void,
): () => void {
  if (typeof ResizeObserver === 'undefined') return () => {};
  const observer = new ResizeObserver((entries) => {
    // `=== undefined` is load-bearing: it narrows `number | undefined` to
    // `number` for Math.round (Number.isFinite is not a TS type predicate).
    const measured = entries[0]?.contentRect.width;
    if (measured === undefined || !Number.isFinite(measured)) return;
    onWidth(Math.max(0, Math.round(measured)));
  });
  observer.observe(surface);
  return () => observer.disconnect();
}

export function directRowGeometryContent(row: HTMLElement): HTMLElement | null {
  for (const child of row.children) {
    if (
      child instanceof HTMLElement
      && child.hasAttribute(ROW_GEOMETRY_CONTENT_ATTR)
    ) {
      return child;
    }
  }
  return null;
}

function sameOwnerItemIds(a: readonly string[], b: readonly string[]): boolean {
  if (a.length !== b.length) return false;
  for (let index = 0; index < a.length; index += 1) {
    if (a[index] !== b[index]) return false;
  }
  return true;
}

function sameReservationParams(
  a: TimelineRowGeometryReservationParams,
  b: TimelineRowGeometryReservationParams,
): boolean {
  return (
    a.key === b.key
    && a.signature === b.signature
    && a.width === b.width
    && sameOwnerItemIds(a.ownerItemIds, b.ownerItemIds)
  );
}

