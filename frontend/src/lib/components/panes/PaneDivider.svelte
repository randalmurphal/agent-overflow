<script lang="ts">
  import { onDestroy } from 'svelte';
  import { getMinPaneWidth } from '../../stores/paneDensity.svelte';
  import { getPaneWidth } from '../../stores/layoutMetrics.svelte';
  import {
    applyPaneBoundaryDrag,
    equalizePaneWidths,
    flushPaneLayoutPersistence,
    getPaneLayoutItems,
    minAnchorPaneLayoutWidths,
  } from '../../stores/paneLayout.svelte';
  import { getPane } from '../../stores/panes.svelte';
  import { MAX_PANE_WIDTH_PX, PANE_DIVIDER_WIDTH_PX } from '../../utils/paneWidths';
  import { edgeAutoScrollVelocity, type HorizontalEdges } from './edgeAutoScroll';

  // Hand-rolled gesture rather than utils/resizeGesture.svelte.ts: that
  // helper models a single clamped scalar, while this drag needs an
  // all-panes start snapshot, content-space deltas, and a per-frame
  // auto-scroll loop that keeps resizing while the pointer sits still.

  interface Props {
    leftPaneId: string;
    // Absent on the end handle at the strip's right edge: that drag
    // resizes the last pane with nothing on the far side to shift.
    rightPaneId?: string;
    // Current width of the pane left of the boundary, for aria-valuenow.
    // Passed in (rather than looked up) so this component does not re-scan
    // the layout on every resize frame.
    leftPaneWidthPx: number;
    getHostEl: () => HTMLElement | undefined;
    // Fired after a completed resize gesture. PaneHost republishes pane
    // offsets here: shifted panes keep their width, so their per-pane
    // ResizeObservers never fire and cached offsets would go stale.
    onDragEnd?: () => void;
  }

  let { leftPaneId, rightPaneId, leftPaneWidthPx, getHostEl, onDragEnd }: Props = $props();

  // A double-click that was really two fine-tune drags must not nuke
  // the layout the user just built.
  const DBLCLICK_SUPPRESS_TRAVEL_PX = 4;

  let dragging = $state(false);
  let startClientX = 0;
  let lastClientX = 0;
  // Scroll this drag itself performed via edge auto-scroll. Tracked
  // explicitly instead of diffing host.scrollLeft: shrinking a pane
  // shrinks scrollWidth, which makes the browser clamp scrollLeft down
  // — diffing would feed that clamp back into the delta and compound
  // the shrink every frame.
  let autoScrolledPx = 0;
  let hostRect: HorizontalEdges = { left: 0, right: 0, width: 0 };
  let startWidths: Map<string, number> = new Map();
  let overflowPx = 0;
  let zeroSum = false;
  let lastAppliedDelta: number | null = null;
  let frame: number | null = null;
  let releaseLease: (() => void) | null = null;
  let prevGestureTravelPx = 0;
  let gestureTravelPx = 0;

  function acquireAdjacentScrollLeases(): (() => void) | null {
    const releases = [
      getPane(leftPaneId)?.scrollController?.pauseAutoScroll() ?? null,
      rightPaneId ? getPane(rightPaneId)?.scrollController?.pauseAutoScroll() ?? null : null,
    ].filter((release): release is () => void => release !== null);
    if (releases.length === 0) return null;
    return () => {
      for (const release of releases) release();
    };
  }

  function applyLive(): void {
    const delta = lastClientX - startClientX + autoScrolledPx;
    if (delta === lastAppliedDelta) return;
    lastAppliedDelta = delta;
    applyPaneBoundaryDrag({
      leftPaneId,
      rightPaneId: rightPaneId ?? null,
      startWidths,
      deltaPx: delta,
      minPaneWidth: getMinPaneWidth(),
      overflowPx,
      zeroSum,
    });
  }

  function edgeAutoScrollStep(): void {
    const host = getHostEl();
    if (!host) return;
    const step = edgeAutoScrollVelocity(hostRect, lastClientX);
    if (step === 0) return;
    const before = host.scrollLeft;
    host.scrollLeft = before + step;
    autoScrolledPx += host.scrollLeft - before;
  }

  // Runs every frame while dragging: the auto-scroll must keep feeding
  // the resize while the pointer sits still at the window edge, so a
  // move-event-driven update is not enough.
  function frameLoop(): void {
    frame = null;
    if (!dragging) return;
    edgeAutoScrollStep();
    applyLive();
    frame = requestAnimationFrame(frameLoop);
  }

  function onPointerDown(event: PointerEvent): void {
    if (event.button !== 0 || dragging) return;
    event.preventDefault();
    window.getSelection()?.removeAllRanges();
    const host = getHostEl();
    dragging = true;
    zeroSum = event.altKey;
    startClientX = event.clientX;
    lastClientX = event.clientX;
    autoScrolledPx = 0;
    prevGestureTravelPx = gestureTravelPx;
    gestureTravelPx = 0;
    overflowPx = host ? Math.max(0, host.scrollWidth - host.clientWidth) : 0;
    // The host does not move or resize mid-drag; snapshot its edges
    // instead of a getBoundingClientRect per frame.
    const rect = host?.getBoundingClientRect();
    hostRect = rect
      ? { left: rect.left, right: rect.right, width: rect.width }
      : { left: 0, right: 0, width: 0 };
    lastAppliedDelta = null;
    startWidths = new Map();
    for (const item of getPaneLayoutItems()) {
      const measured = getPaneWidth(item.paneId);
      startWidths.set(item.paneId, measured > 0 ? measured : item.widthPx);
    }
    (event.currentTarget as HTMLElement).setPointerCapture(event.pointerId);
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
    releaseLease = acquireAdjacentScrollLeases();
    frame = requestAnimationFrame(frameLoop);
  }

  function onPointerMove(event: PointerEvent): void {
    if (!dragging) return;
    lastClientX = event.clientX;
    gestureTravelPx = Math.max(gestureTravelPx, Math.abs(event.clientX - startClientX));
  }

  function cancelDragArtifacts(): void {
    dragging = false;
    if (frame !== null) {
      cancelAnimationFrame(frame);
      frame = null;
    }
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
    releaseLease?.();
    releaseLease = null;
  }

  function endDrag(event?: PointerEvent): void {
    if (!dragging) return;
    cancelDragArtifacts();
    if (event) {
      const target = event.currentTarget as HTMLElement;
      if (target.hasPointerCapture?.(event.pointerId)) {
        target.releasePointerCapture(event.pointerId);
      }
    }
    applyLive();
    // The store gates this on its own data (skip while overflowing), so
    // no DOM measurement — the final width write may not have flushed.
    minAnchorPaneLayoutWidths(getMinPaneWidth());
    void flushPaneLayoutPersistence();
    onDragEnd?.();
  }

  function onDoubleClick(): void {
    if (Math.max(prevGestureTravelPx, gestureTravelPx) > DBLCLICK_SUPPRESS_TRAVEL_PX) return;
    equalizePaneWidths(getMinPaneWidth());
    void flushPaneLayoutPersistence();
    onDragEnd?.();
  }

  onDestroy(() => {
    if (dragging) cancelDragArtifacts();
  });
</script>

<!-- Must be rendered in a flex (or otherwise definite-height) parent:
     this root has no intrinsic height — its only child is absolutely
     positioned — so it relies on the parent's cross-axis stretch to
     fill height instead of collapsing to 0px. PaneHost wraps it in a
     `flex` gap div. -->
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  role="separator"
  aria-orientation="vertical"
  aria-label={rightPaneId ? 'Resize Panes' : 'Resize Last Pane'}
  aria-valuenow={Math.round(leftPaneWidthPx)}
  aria-valuemin={getMinPaneWidth()}
  aria-valuemax={MAX_PANE_WIDTH_PX}
  style={`width:${PANE_DIVIDER_WIDTH_PX}px`}
  class={[
    'relative z-20 shrink-0 cursor-col-resize select-none touch-none',
    'bg-border-subtle/30 hover:bg-accent/40 transition-colors',
    dragging ? 'bg-accent/60' : '',
  ].join(' ')}
  onpointerdown={onPointerDown}
  onpointermove={onPointerMove}
  onpointerup={endDrag}
  onpointercancel={endDrag}
  onlostpointercapture={endDrag}
  ondblclick={onDoubleClick}
  data-testid={rightPaneId ? 'pane-divider' : 'pane-end-handle'}
>
  <!-- Widened invisible hit area; the visible strip stays slim. -->
  <div class="absolute inset-y-0 -left-1 -right-1"></div>
</div>
