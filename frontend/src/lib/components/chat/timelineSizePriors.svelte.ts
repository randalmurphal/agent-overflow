// Per-thread row-size priors: persistence and thread-switch replay for
// MessageTimeline's windowing engine. Resolves the incoming thread's
// `RowEstimate` before the virtualizer remounts, and captures the
// outgoing thread's measured sizes on the same triggers as the scroll
// snapshot (see timelineRestore.svelte.ts's `saveScrollSnapshot`).

import { untrack } from 'svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import {
  createRowEstimate,
  getReplayableSizePriors,
  setThreadSizePriors,
  type SizePriorsKey,
} from '../../utils/virtual/priors';
import type { RowEstimate, TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { TimelineNode } from '../../utils/subagentGrouping';
import { timelineStructureSignature } from '../../utils/timelineStructureSignature';

// Flat fallback row estimate for the windowing engine. Real sizes come
// from the virtualizer's per-row ResizeObserver; estimates only place
// unmeasured rows before their first measurement lands (priors → kind
// table → this default; see utils/virtual/priors.ts). A floor like the
// kind table below, for the same asymmetry.
const ESTIMATED_ROW_SIZE = 40;
// Cold-thread (priors-miss) placement estimates keyed by rendered node
// kind — leaf rows use their item kind, structural rows their node
// kind (timelineRowEstimateKind below). Estimates never decide what a
// row renders, only where unmeasured rows sit until measured — and the
// two error directions cost differently. OVERSHOOT shrinks totalSize
// when the real measurement lands: the scrollbar dips, and while
// pinned at the exact bottom the browser synchronously clamps
// scrollTop down (the remount-collapse class
// remountReturn.browser.test.ts polices). UNDERSHOOT only grows
// totalSize, which the engine's remeasure-above compensation absorbs
// invisibly; its cost is a few extra transiently mounted rows on a
// cold thread switch. So these are FLOORS, not averages: the measured
// 1-line rendered height per kind (real-Chromium probe, default
// settings, 800px pane), derated ~20% so smaller font settings stay
// under. Warm switches replay exact priors and never touch this table.
const ROW_KIND_ESTIMATE_PX: Readonly<Record<string, number>> = {
  user_text: 72,
  assistant_text: 44,
  thinking: 30,
  tool_call: 20,
  tool_completion: 20,
  error: 42,
  notification: 24,
  api_retry: 24,
  read_group: 20,
  group: 36,
  wait_group: 36,
};

export interface TimelineSizePriorsOptions {
  getPane(): ThreadPane;
  getListRef(): TimelineVirtualizerHandle | undefined;
  getRevealedNodes(): TimelineNode[];
  getScrollSurfaceContentWidth(): number;
  getRestoredThreadId(): string | null;
}

export interface TimelineSizePriors {
  /** Reactive — the template binds `estimate={sizePriors.rowEstimate}`. */
  readonly rowEstimate: RowEstimate | undefined;
  maybePersistSizePriors(): void;
  resolveRowEstimateOnThreadEdge(threadId: string | null): void;
  captureOnWarmRisingEdge(warm: boolean): void;
}

export function createTimelineSizePriors(
  options: TimelineSizePriorsOptions,
): TimelineSizePriors {
  let rowEstimate = $state<RowEstimate | undefined>(undefined);
  let rowEstimateThreadId: string | null = null;

  // Total size at the last capture. The estimate→measure cascade only
  // moves this when rows actually measure, so it gates the capture: we
  // re-snap exactly when (and only when) the engine's geometry changed,
  // never per scroll frame. Reset on the threadId edge so the incoming
  // thread's first measured size is never mistaken for "unchanged"
  // against the outgoing thread's.
  let lastPersistedTotalSize = -1;
  let lastWarmForCapture = false;

  // The validity stamp for a measured-size snapshot: row height is keyed on
  // pane width (wrap point), the rendered node sequence + per-leaf content
  // (structureSig), and non-default expansion (taller rows). A snapshot only
  // replays when all three still match — otherwise every row falls back to
  // its kind estimate. (Display settings — fontSize, fonts,
  // collapseDiffPreviews — also affect height but are a deliberately-unkeyed
  // benign residual; see the header of utils/virtual/priors.ts.)
  // `scrollSurfaceContentWidth` persists across switches (MessageTimeline is
  // not keyed on threadId), so it carries the correct width into the next
  // mount. structureSig is computed from `revealedNodes` — the exact array
  // the virtualizer receives as `data` — so capture and the next mount's
  // lookup sign the same thing; it superseded an earlier version of this key
  // that read `pane.timelineRevision`, a monotonic counter that was never
  // restored on re-entry and so could never match on a revisit (the field
  // itself remains, as the timeline-derivation trigger).
  function currentSizePriorsKey(): SizePriorsKey {
    return {
      width: Math.round(options.getScrollSurfaceContentWidth()),
      structureSig: timelineStructureSignature(options.getRevealedNodes()),
      expansionSig: options.getPane().expansionSignature(),
    };
  }

  // Kind resolver for the estimate fallback. Reads live `revealedNodes`,
  // so it needs no remap across head splices (unlike the positional
  // priors snapshot, which the engine re-bases via RowEstimate.shiftBase).
  function timelineRowEstimateKind(index: number): string | undefined {
    const node = options.getRevealedNodes()[index];
    if (!node) return undefined;
    return node.kind === 'leaf' ? node.item.kind : node.kind;
  }

  // Capture the engine's current measured sizes for the active thread, but
  // only when the total size changed since the last capture — so a 60Hz
  // spring chase doesn't re-slice the size array every frame. The most recent
  // capture before a switch is what the return replays; mirroring the
  // scroll-snapshot strategy, we never capture in the switch effect.pre
  // because `pane` has already mutated to the incoming thread by then.
  //
  // Mid-stream cost is known and tolerated: on an actively-streaming thread the
  // size-gate passes once per geometry change (each append grows the total), so
  // takeSnapshot() + the O(N) structureSig rebuild in currentSizePriorsKey()
  // run ~5–20×/sec — bounded by the gate (never per-frame) and only while the
  // visible thread streams. Only the settle capture (isWarm rising) matters for
  // replay; the interim ones are overwritten. Deliberately NOT gated on
  // spring-chase state: that would risk dropping the settle capture on an
  // already-warm streaming thread (isWarm does not re-arm), regressing replay.
  function maybePersistSizePriors(): void {
    const pane = options.getPane();
    const threadId = pane.threadId || null;
    const listRef = options.getListRef();
    if (!threadId || !listRef || options.getRestoredThreadId() !== threadId) return;
    // O(1) read (the engine's prefix-sum total) — the cheap change-gate.
    // Skip the takeSnapshot() slice entirely when geometry hasn't moved
    // (60Hz spring).
    const totalSize = listRef.getTotalSize();
    if (totalSize === lastPersistedTotalSize) return;
    lastPersistedTotalSize = totalSize;
    setThreadSizePriors(threadId, {
      sizes: listRef.takeSnapshot(),
      ...currentSizePriorsKey(),
    });
  }

  // Resolve the row estimate for the INCOMING thread before the
  // {#key pane.threadId} block remounts the <TimelineVirtualizer>. The
  // virtualizer configures its engine once at construction, and
  // $effect.pre runs before DOM flush, so `rowEstimate` is settled by the
  // time the remount reads it. Gated on the threadId edge: mid-thread
  // revision/width churn must not recompute it (the mounted virtualizer
  // ignores a changed `estimate` anyway), and the same-thread revert flow
  // keeps threadId constant so it never remounts.
  function resolveRowEstimateOnThreadEdge(threadId: string | null): void {
    if (threadId === rowEstimateThreadId) return;
    rowEstimateThreadId = threadId;
    lastPersistedTotalSize = -1;
    rowEstimate = threadId
      ? untrack(() =>
          createRowEstimate({
            snapshot: getReplayableSizePriors(threadId, currentSizePriorsKey()),
            kindOf: timelineRowEstimateKind,
            kindHeights: ROW_KIND_ESTIMATE_PX,
            defaultSize: ESTIMATED_ROW_SIZE,
          }),
        )
      : undefined;
  }

  // Guarantee a post-settle capture. The scroll-driven captures
  // (handleTimelineScroll/ScrollEnd → saveScrollSnapshot) only store settled
  // sizes if the cascade's bottom-pin re-pins fire scroll events — which
  // an idle, bottom-pinned thread the user never scrolls cannot rely on, so the
  // only stored snapshot would be the pre-settle estimate and the NEXT visit
  // would replay it and still cascade. `stick.isWarm` is the controller's
  // existing "measurement cascade has settled" signal (QUIET_MS of geometry
  // stillness); on its rising edge the sizes are final. Capture is `untrack`ed
  // so this effect depends ONLY on isWarm — not on the geometry/content
  // maybePersistSizePriors reads — keeping it a settle-edge trigger, not a
  // content watcher. Size-gated downstream, so if a scroll capture already
  // stored the settled total this is a no-op; cascade-interim warm flickers
  // store interim sizes the final settle overwrites.
  function captureOnWarmRisingEdge(warm: boolean): void {
    const rising = warm && !lastWarmForCapture;
    lastWarmForCapture = warm;
    if (rising) untrack(() => maybePersistSizePriors());
  }

  return {
    get rowEstimate() {
      return rowEstimate;
    },
    maybePersistSizePriors,
    resolveRowEstimateOnThreadEdge,
    captureOnWarmRisingEdge,
  };
}
