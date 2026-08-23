// Diagnostic instrumentation for MessageTimeline: render/state tracing,
// the dev-only memory-stats and pane-geometry probes, and the trace-flag
// gated row-resize / margin-divergence / reasoning-tail-jump oracles.
// Holds no runes itself — reactivity comes from the component's thin
// `$effect` bodies that call into these methods, so each method's
// internal reads (pane, contentEl, etc.) are tracked by whichever effect
// invokes it, same as if the effect body were written out here directly.

import type { ThreadPane } from '../../stores/thread.svelte';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { TimelineNode } from '../../utils/subagentGrouping';
import {
  recordTimelineRenderTrace,
  startTimelineRowResizeTrace,
  startRowMarginDivergenceTrace,
  startReasoningTailJumpTrace,
} from './messageTimelineTrace';
import {
  isUiOracleTraceEnabled,
  isUiRenderTraceEnabled,
  recordUiTrace,
} from '../../utils/uiRenderTrace';
import {
  countMountedTimelineMemoryNodes,
  installTimelineMemoryDiagnostics,
} from '../../utils/timelineMemoryDiagnostics';
import {
  installPaneGeometryProbe,
  type PaneGeometrySnapshot,
} from '../../utils/paneGeometryProbe';

export interface TimelineDiagnosticsOptions {
  getPane(): ThreadPane;
  stick: UseStickToBottomController;
  getScrollEl(): HTMLDivElement | undefined;
  getContentEl(): HTMLDivElement | undefined;
  getListRef(): TimelineVirtualizerHandle | undefined;
  getRevealedNodes(): TimelineNode[];
  getScrollSurfaceContentWidth(): number;
  getRestoredThreadId(): string | null;
}

export interface TimelineDiagnostics {
  recordRenderTrace(): void;
  memoryDiagnosticsSnapshotInstall(): () => void;
  captureTimelineGeometry(): PaneGeometrySnapshot;
  geometryProbeInstall(): () => void;
  recordListRefBindTrace(): void;
  rowResizeTraceInstall(): (() => void) | undefined;
  marginDivergenceTraceInstall(): (() => void) | undefined;
  reasoningTailJumpTraceInstall(): (() => void) | undefined;
}

export function createTimelineDiagnostics(
  options: TimelineDiagnosticsOptions,
): TimelineDiagnostics {
  // Diagnostic UI render trace — extracted to messageTimelineTrace.ts.
  // Production builds short-circuit at isUiRenderTraceEnabled() inside
  // the helper, so this $effect's only steady-state cost is the reactive
  // dep tracking.
  function recordRenderTrace(): void {
    const pane = options.getPane();
    const revealedNodes = options.getRevealedNodes();
    pane.threadId;
    pane.items.length;
    pane.timelineRevision;
    revealedNodes.length;
    recordTimelineRenderTrace(pane, revealedNodes, options.getScrollEl(), options.getListRef());
  }

  function memoryDiagnosticsSnapshotInstall(): () => void {
    const pane = options.getPane();
    return installTimelineMemoryDiagnostics(pane.paneId, () => ({
      threadId: pane.threadId || null,
      itemWindowItems: pane.items.length,
      revealedNodes: options.getRevealedNodes().length,
      oldestLoadedCursor: pane.oldestLoadedCursor,
      newestLoadedCursor: pane.newestLoadedCursor,
      oldestLoadedTurnIndex: pane.oldestLoadedTurnIndex,
      newestLoadedTurnIndex: pane.newestLoadedTurnIndex,
      hasMoreHistory: pane.hasMoreHistory,
      hasMoreNewer: pane.hasMoreNewer,
      loadingOlder: pane.loadingOlder,
      loadingNewer: pane.loadingNewer,
      ...countMountedTimelineMemoryNodes(options.getScrollEl()),
      paneState: pane.debugMemoryStats(),
    }));
  }

  // Dev-only per-pane scroll-geometry probe for the width-reflow strand
  // (last message left floating high after a pane widens, never self-correcting).
  // Reports the engine's per-row slot size vs the real DOM row height, so a
  // Ctrl+Shift+B capture at a stable strand names the mechanism. Reads THIS
  // pane's controller + refs directly, so it is immune to __stickState's
  // last-writer-wins. See utils/paneGeometryProbe.ts.
  function captureTimelineGeometry(): PaneGeometrySnapshot {
    const pane = options.getPane();
    const stick = options.stick;
    const snapshot: PaneGeometrySnapshot = {
      paneId: pane.paneId,
      threadId: pane.threadId || null,
      isAtBottom: stick.isAtBottom,
      isSticky: stick.isSticky,
      escapedFromLock: stick.escapedFromLock,
      isWarm: stick.isWarm,
      scrollTop: null,
      scrollHeight: null,
      clientHeight: null,
      clientWidth: null,
      distanceFromBottom: null,
      scrollSurfaceContentWidth: options.getScrollSurfaceContentWidth(),
      itemsLength: pane.items.length,
      engineTotalSize: null,
      bottomRenderedIndex: null,
      rows: [],
    };
    try {
      const surface = options.getScrollEl();
      if (surface) {
        snapshot.scrollTop = Math.round(surface.scrollTop);
        snapshot.scrollHeight = Math.round(surface.scrollHeight);
        snapshot.clientHeight = Math.round(surface.clientHeight);
        snapshot.clientWidth = Math.round(surface.clientWidth);
        snapshot.distanceFromBottom = Math.round(
          surface.scrollHeight - surface.scrollTop - surface.clientHeight,
        );
      }

      const list = options.getListRef();
      if (list) {
        snapshot.engineTotalSize = Math.round(list.getTotalSize());
      }

      const contentEl = options.getContentEl();
      if (contentEl) {
        const itemCount = options.getRevealedNodes().length;
        const wrappers = contentEl.querySelectorAll<HTMLElement>('[data-row-index]');
        let bottomIndex = -1;
        for (const wrapper of wrappers) {
          const index = Number(wrapper.dataset.rowIndex);
          if (!Number.isInteger(index)) continue;
          const wrapperHeight = wrapper.offsetHeight;
          // The engine's slot for this index: measured size, or the
          // estimate the row is currently placed at.
          const slotSize =
            list && index >= 0 && index < itemCount ? Math.round(list.sizeAt(index)) : null;
          snapshot.rows.push({
            index,
            wrapperHeight,
            slotSize,
            slotVsWrapper: slotSize === null ? null : slotSize - wrapperHeight,
          });
          if (index > bottomIndex) bottomIndex = index;
        }
        snapshot.rows.sort((a, b) => a.index - b.index);
        snapshot.bottomRenderedIndex = bottomIndex >= 0 ? bottomIndex : null;
      }
    } catch (err) {
      snapshot.error = String(err);
    }
    return snapshot;
  }

  function geometryProbeInstall(): () => void {
    return installPaneGeometryProbe(options.getPane().paneId, captureTimelineGeometry);
  }

  // Trace virtualizer remount transitions: listRef goes undefined →
  // defined when the {#key pane.threadId} block remounts the
  // virtualizer. A stale scrollTop from the outgoing thread can be
  // visible here until the restore effect lands.
  //
  // Emits on TRANSITIONS only (listRef identity / threadId /
  // restoredThreadId). The method's reactive deps come from whichever
  // $effect calls it, and reading revealedNodes unconditionally made
  // that effect re-run on every streamed projection pass — ~100
  // identical records/minute that diluted the record's meaning and
  // crowded the trace ring (bug-report-20260818T163100Z: 5% of the
  // whole capture). The early return also drops revealedNodes from the
  // effect's dep set on non-transition runs, so the effect itself
  // stops waking per pass.
  // `listRef` is held through a WeakRef: this dedup cache outlives the
  // handle it compares against, and a strong ref here pinned the outgoing
  // virtualizer's whole detached row plane between binds (2026-08-22 heap
  // snapshot — `lastListRefBind` was a top retainer of detached timeline
  // DOM). A collected handle simply fails the identity compare and the
  // transition records again, which is the correct answer anyway: the
  // handle being gone means it was a different virtualizer.
  let lastListRefBind: {
    listRef: WeakRef<TimelineVirtualizerHandle> | null;
    threadId: string | null;
    restoredThreadId: string | null;
  } | null = null;
  function recordListRefBindTrace(): void {
    if (!isUiRenderTraceEnabled()) return;
    const listRef = options.getListRef();
    const threadId = options.getPane().threadId;
    const restoredThreadId = options.getRestoredThreadId();
    // WeakRef refuses non-object targets, and test seams hand this method
    // null/primitive stand-ins; those dedup as "unbound" (worst case a
    // duplicate transition record, never a crash or a pin).
    const refable = typeof listRef === 'object' && listRef !== null;
    const last = lastListRefBind;
    if (
      last
      && (last.listRef !== null) === refable
      && (last.listRef === null || last.listRef.deref() === listRef)
      && last.threadId === threadId
      && last.restoredThreadId === restoredThreadId
    ) {
      return;
    }
    lastListRefBind = {
      listRef: refable ? new WeakRef(listRef) : null,
      threadId,
      restoredThreadId,
    };
    const scrollEl = options.getScrollEl();
    recordUiTrace('timeline.listRef.bind', {
      bound: listRef !== undefined,
      threadId,
      restoredThreadId,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      groupedNodesLength: options.getRevealedNodes().length,
    });
  }

  // Per-row resize tracker. Diagnostic-only — gated on the trace flag, so
  // production builds skip it entirely. The helper observes mounted
  // [data-row-index] wrappers with ResizeObserver and keeps its
  // MutationObserver scoped to row add/remove discovery, so streaming text
  // mutations do not trigger trace-side layout measurements.
  function rowResizeTraceInstall(): (() => void) | undefined {
    const contentEl = options.getContentEl();
    if (!isUiOracleTraceEnabled() || !contentEl) return undefined;
    return startTimelineRowResizeTrace(contentEl);
  }

  // Settle-flicker regression oracle. Observes the `contain: layout` VirtualRow
  // wrappers AND their inner [data-row-index] rows, and emits
  // `timeline.margin.diverge` only when a frame moves the wrapper by a
  // different amount than the row — the escaped-margin signature the
  // `[data-row-geometry-content] { display: flow-root }` fix eliminated. With
  // the fix in place it stays silent; any emission flags a new wrapper chain
  // that re-opened the collapse-out. Same trace-flag gate, so production skips
  // it entirely.
  function marginDivergenceTraceInstall(): (() => void) | undefined {
    const contentEl = options.getContentEl();
    if (!isUiOracleTraceEnabled() || !contentEl) return undefined;
    return startRowMarginDivergenceTrace(contentEl);
  }

  // Streaming-thinking flicker regression oracle. Tracks each reasoning-tail
  // body and emits `timeline.reasoning.tailJump` only when a frame re-wraps it
  // (width change) with no text delta yet leaves the newest line below the
  // 3-line window — the stale imperative-pin signature the TailClampedText
  // flex bottom-anchor eliminated. Silent with the fix; an emission flags a
  // regression (or, on the pre-fix build, confirms the trigger fires live).
  // Same trace-flag gate, so production skips it entirely.
  function reasoningTailJumpTraceInstall(): (() => void) | undefined {
    const contentEl = options.getContentEl();
    if (!isUiOracleTraceEnabled() || !contentEl) return undefined;
    return startReasoningTailJumpTrace(contentEl);
  }

  return {
    recordRenderTrace,
    memoryDiagnosticsSnapshotInstall,
    captureTimelineGeometry,
    geometryProbeInstall,
    recordListRefBindTrace,
    rowResizeTraceInstall,
    marginDivergenceTraceInstall,
    reasoningTailJumpTraceInstall,
  };
}
