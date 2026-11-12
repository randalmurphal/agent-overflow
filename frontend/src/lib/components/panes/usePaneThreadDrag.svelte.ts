// Factory for the PaneHost drag/drop controller. Owns:
//   - the drop slot and its indicator for the three drags that place a pane
//     in the strip: a sidebar thread row, a tools-row button and a pane
//     dragged by its title. All three resolve their slot through one
//     `resolveDrop`, so what the indicator shows is where the drop lands.
//   - duplicate-pane highlight when dragging a thread already mounted
//   - edge-driven auto-scroll while a drag hovers the host edges
// Pulled out of PaneHost.svelte so the host file stays readable and the
// drag protocol becomes unit-testable through a single public surface.
//
// A slot is an edge of a layout block (a pane with its companions): the
// pointer's x picks the block it is over and the nearer of its two edges,
// whatever element it is over. The indicator is a bar on that seam plus
// the half that chose it.
//
// While one of these drags is over the strip it is `shielding`: PaneHost
// covers the panes with an element outside the strip's scroller, so the
// drag is never over pane content. The browser scrolls a scrollable
// element a drag is held near the edge of, and a pane's own drop handlers
// would see the drag; neither can happen through the shield.
//
// The DataTransfer answers nothing during dragover, so the payloads come
// from `threadDragPayloadForEvent` / `toolDragPayloadForEvent`, which fall
// back to the source's in-flight record; every drop clears that record.

import {
  findPaneShowingThread,
  focusPane,
  openThreadIdInNewPane,
  revealPane,
} from '../../stores/panes.svelte';
import {
  movePaneLayoutItemToIndex,
  paneBlockRangeAt,
  type PaneLayoutItem,
} from '../../stores/paneLayout.svelte';
import { isToolAtSlot, openTool } from '../../stores/toolPanes.svelte';
import {
  endThreadRowDrag,
  endToolPaneDrag,
  PANE_REORDER_DRAG_MIME,
  threadDragPayloadForEvent,
  toolDragPayloadForEvent,
  THREAD_ROW_DRAG_MIME,
  TOOL_PANE_DRAG_MIME,
  type PaneDropTarget,
} from '../../utils/threadDragPayload';
import { edgeAutoScrollVelocity } from './edgeAutoScroll';

/** Width of the seam bar. */
export const PANE_DROP_SEAM_PX = 4;

export interface PaneThreadDragOptions {
  /** The strip's root: the scroller and the shield are inside it. */
  getStripEl(): HTMLElement | undefined;
  /** The scroller whose children are the pane sections. */
  getHostEl(): HTMLElement | undefined;
  getLayoutItems(): PaneLayoutItem[];
}

/** Where the indicator paints, in the strip's scroll-content pixels. */
export interface PaneDropIndicator {
  seamLeft: number;
  /** The half of the block under the pointer; `seam` is the side its bar is on. */
  lit: { left: number; width: number; seam: 'left' | 'right' } | null;
}

/** A horizontal run in viewport pixels. */
interface Extent {
  left: number;
  right: number;
}

interface ResolvedDrop {
  target: PaneDropTarget;
  /** Viewport x of the seam; null for the empty strip, which has none. */
  seam: number | null;
  lit: Extent | null;
}

export function createPaneThreadDrag(options: PaneThreadDragOptions) {
  let draggingPaneId = $state<string | null>(null);
  let duplicateDropPaneId = $state<string | null>(null);
  let dropTarget = $state<PaneDropTarget | null>(null);
  let dropIndicator = $state.raw<PaneDropIndicator | null>(null);
  let shielding = $state(false);

  let autoScrollFrame: number | null = null;
  let autoScrollVelocity = 0;

  function sameIndicator(a: PaneDropIndicator | null, b: PaneDropIndicator | null): boolean {
    if (!a || !b) return a === b;
    if (a.seamLeft !== b.seamLeft || !a.lit !== !b.lit) return false;
    return !a.lit || !b.lit || (a.lit.left === b.lit.left && a.lit.width === b.lit.width && a.lit.seam === b.lit.seam);
  }

  // The bar is held between the strip's left edge and its last pane's
  // right edge, and the lit half keeps its measured edges unrounded, so the
  // first and last seams stay whole and neither adds scroll range.
  function indicatorFor(resolved: ResolvedDrop): PaneDropIndicator | null {
    const host = options.getHostEl();
    const lastPaneId = options.getLayoutItems().at(-1)?.paneId;
    const end = lastPaneId ? paneExtent(lastPaneId) : null;
    if (!host || !end || resolved.seam === null) return null;
    const origin = host.getBoundingClientRect().left + host.clientLeft - host.scrollLeft;
    const maxLeft = Math.max(0, end.right - origin - PANE_DROP_SEAM_PX);
    const lit = resolved.lit;
    return {
      seamLeft: Math.min(maxLeft, Math.max(0, resolved.seam - origin - PANE_DROP_SEAM_PX / 2)),
      lit: lit && {
        left: lit.left - origin,
        width: lit.right - lit.left,
        seam: resolved.seam <= lit.left ? 'left' : 'right',
      },
    };
  }

  function showDrop(resolved: ResolvedDrop | null): void {
    const target = resolved?.target ?? null;
    if (target?.kind !== dropTarget?.kind || target?.insertIndex !== dropTarget?.insertIndex) dropTarget = target;
    const indicator = resolved ? indicatorFor(resolved) : null;
    if (!sameIndicator(indicator, dropIndicator)) dropIndicator = indicator;
  }

  // A drop resolves its slot from its own event against the layout as it
  // is now, never from the last dragover: the pointer can move, and the
  // layout change, without one.
  function dropSlot(event: DragEvent): PaneDropTarget | null {
    return resolveDrop(event)?.target ?? null;
  }

  // Every sidebar drag ends here, wherever it ends: a dragend at its source
  // (a drop, Escape, a drop outside the window) or a drop that bubbles to
  // the document. A source that unmounted mid-drag gets no dragend; leaving
  // the strip clears the preview then, and the next pointer event over the
  // strip (onShieldPointer) ends the drag.
  function onDocumentDragEnd(): void {
    draggingPaneId = null;
    clearDropState();
    endThreadRowDrag();
    endToolPaneDrag();
  }
  document.addEventListener('dragend', onDocumentDragEnd, true);
  document.addEventListener('drop', onDocumentDragEnd);

  function isThreadDrag(event: DragEvent): boolean {
    return event.dataTransfer?.types.includes(THREAD_ROW_DRAG_MIME) ?? false;
  }

  function isPaneDrag(event: DragEvent): boolean {
    return event.dataTransfer?.types.includes(PANE_REORDER_DRAG_MIME) ?? false;
  }

  function isToolDrag(event: DragEvent): boolean {
    return event.dataTransfer?.types.includes(TOOL_PANE_DRAG_MIME) ?? false;
  }

  function paneExtent(paneId: string): Extent | null {
    const host = options.getHostEl();
    if (!host) return null;
    for (const child of host.children) {
      if (child instanceof HTMLElement && child.dataset.paneId === paneId) {
        const rect = child.getBoundingClientRect();
        return { left: rect.left, right: rect.right };
      }
    }
    return null;
  }

  // Companions glue to their source, so the slots around any pane of a
  // block are the block's edges: the pointer's half of the block picks one.
  function resolveDrop(event: DragEvent): ResolvedDrop | null {
    const items = options.getLayoutItems();
    if (items.length === 0) return { target: { kind: 'empty', insertIndex: 0 }, seam: null, lit: null };
    const x = event.clientX;
    for (let start = 0; start < items.length; ) {
      const { end } = paneBlockRangeAt(items, start);
      const first = paneExtent(items[start].paneId);
      const last = paneExtent(items[end].paneId);
      if (!first || !last) return null;
      if (x < last.right) {
        const middle = (first.left + last.right) / 2;
        return x > middle
          ? { target: { kind: 'pane-right', insertIndex: end + 1 }, seam: last.right, lit: { left: middle, right: last.right } }
          : { target: { kind: 'pane-left', insertIndex: start }, seam: first.left, lit: { left: first.left, right: middle } };
      }
      if (end === items.length - 1) return { target: { kind: 'end', insertIndex: items.length }, seam: last.right, lit: null };
      start = end + 1;
    }
    return null;
  }

  function updateThreadDropTarget(event: DragEvent): void {
    const threadId = threadDragPayloadForEvent(event)?.threadId;
    const existing = threadId ? findPaneShowingThread(threadId) : null;
    duplicateDropPaneId = existing?.paneId ?? null;
    showDrop(threadId && !existing ? resolveDrop(event) : null);
  }

  // An open tool moves rather than opening twice: its own slot shows nothing.
  function updateToolDropTarget(event: DragEvent): void {
    duplicateDropPaneId = null;
    const kind = toolDragPayloadForEvent(event)?.kind;
    const resolved = kind ? resolveDrop(event) : null;
    showDrop(resolved && kind && !isToolAtSlot(kind, resolved.target.insertIndex) ? resolved : null);
  }

  // A pane moves with its block, so the block's own two edges are where it
  // already is.
  function reorderMoves(paneId: string, insertIndex: number): boolean {
    const items = options.getLayoutItems();
    const index = items.findIndex((item) => item.paneId === paneId);
    if (index < 0) return false;
    const { start, end } = paneBlockRangeAt(items, index);
    return insertIndex < start || insertIndex > end + 1;
  }

  function updateReorderDropTarget(event: DragEvent, paneId: string): void {
    duplicateDropPaneId = null;
    const resolved = resolveDrop(event);
    showDrop(resolved && reorderMoves(paneId, resolved.target.insertIndex) ? resolved : null);
  }

  function stopDragAutoScroll(): void {
    autoScrollVelocity = 0;
    if (autoScrollFrame === null) return;
    window.cancelAnimationFrame(autoScrollFrame);
    autoScrollFrame = null;
  }

  function updateDragAutoScroll(clientX: number): void {
    const host = options.getHostEl();
    if (!host) return;
    autoScrollVelocity = edgeAutoScrollVelocity(host.getBoundingClientRect(), clientX);
    if (autoScrollVelocity === 0) {
      stopDragAutoScroll();
      return;
    }
    if (autoScrollFrame !== null) return;
    const tick = () => {
      const el = options.getHostEl();
      if (!el || autoScrollVelocity === 0) {
        autoScrollFrame = null;
        return;
      }
      const maxScrollLeft = Math.max(0, el.scrollWidth - el.clientWidth);
      const nextScrollLeft = Math.max(0, Math.min(maxScrollLeft, el.scrollLeft + autoScrollVelocity));
      if (nextScrollLeft === el.scrollLeft) {
        stopDragAutoScroll();
        return;
      }
      el.scrollLeft = nextScrollLeft;
      autoScrollFrame = window.requestAnimationFrame(tick);
    };
    autoScrollFrame = window.requestAnimationFrame(tick);
  }

  function clearDropState(): void {
    duplicateDropPaneId = null;
    shielding = false;
    showDrop(null);
    stopDragAutoScroll();
  }

  async function handleThreadDrop(event: DragEvent): Promise<void> {
    event.preventDefault();
    const threadId = threadDragPayloadForEvent(event)?.threadId;
    const target = dropSlot(event);
    clearDropState();
    // The sidebar's in-flight record: no dragend is guaranteed when the
    // source row unmounted mid-drag, so every drop target clears it.
    endThreadRowDrag();
    if (!threadId) return;
    const existing = findPaneShowingThread(threadId);
    if (existing) {
      // Dropping a thread that's already open is navigation intent:
      // focus AND reveal the pane showing it.
      focusPane(existing.paneId);
      revealPane(existing.paneId);
      return;
    }
    if (!target) return;
    await openThreadIdInNewPane(threadId, target.insertIndex);
  }

  function handleToolDrop(event: DragEvent): void {
    event.preventDefault();
    const kind = toolDragPayloadForEvent(event)?.kind;
    const target = dropSlot(event);
    clearDropState();
    endToolPaneDrag();
    if (!kind || !target) return;
    openTool(kind, { insertIndex: target.insertIndex });
  }

  function onPaneDragStart(event: DragEvent, paneId: string): void {
    draggingPaneId = paneId;
    event.dataTransfer?.setData(PANE_REORDER_DRAG_MIME, paneId);
    if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move';
  }

  function onHostDragOver(event: DragEvent): void {
    if (isThreadDrag(event)) {
      event.preventDefault();
      shielding = true;
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'copy';
      updateThreadDropTarget(event);
      updateDragAutoScroll(event.clientX);
      return;
    }
    if (isToolDrag(event)) {
      event.preventDefault();
      shielding = true;
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
      updateToolDropTarget(event);
      updateDragAutoScroll(event.clientX);
      return;
    }
    // The drag's own type as well as the pane the strip saw start: a pane
    // drag that ended unheard must not claim the next drag.
    if (draggingPaneId && isPaneDrag(event)) {
      event.preventDefault();
      shielding = true;
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
      updateReorderDropTarget(event, draggingPaneId);
      updateDragAutoScroll(event.clientX);
    }
  }

  function handleReorderDrop(event: DragEvent, paneId: string): void {
    event.preventDefault();
    const target = dropSlot(event);
    draggingPaneId = null;
    clearDropState();
    if (!target || !reorderMoves(paneId, target.insertIndex)) return;
    // The index is one of the layout without the moved pane; its
    // companions follow it there.
    const from = options.getLayoutItems().findIndex((item) => item.paneId === paneId);
    movePaneLayoutItemToIndex(paneId, from < target.insertIndex ? target.insertIndex - 1 : target.insertIndex);
  }

  function onPaneDragEnd(): void {
    draggingPaneId = null;
    clearDropState();
  }

  function onHostDrop(event: DragEvent): void {
    if (isThreadDrag(event)) void handleThreadDrop(event);
    else if (isToolDrag(event)) handleToolDrop(event);
    else if (isPaneDrag(event)) {
      const paneId = draggingPaneId ?? event.dataTransfer?.getData(PANE_REORDER_DRAG_MIME);
      if (paneId) handleReorderDrop(event, paneId);
    }
    // Any other drag (dragged text, a file) is not the strip's: it goes to
    // whatever inside a pane takes it.
  }

  // A dragleave bubbles from every element the pointer leaves, the pane
  // content the shield has just covered included. The drag left the strip
  // when the pointer is outside the strip's box; `relatedTarget` cannot
  // say, since WebKit leaves it null.
  function onHostDragLeave(event: DragEvent): void {
    const box = options.getStripEl()?.getBoundingClientRect();
    if (box && event.clientX >= box.left && event.clientX < box.right && event.clientY >= box.top && event.clientY < box.bottom) return;
    clearDropState();
  }

  // The shield covers the strip, so a drag that leaves it left the strip,
  // for the window or for a surface above the strip. The drag goes on.
  function onShieldDragLeave(): void {
    clearDropState();
  }

  // A pointer event reaches the shield only when no drag is in flight: the
  // drag ended with no dragend or drop the document heard, as when its
  // source unmounted mid-drag.
  function onShieldPointer(): void {
    onDocumentDragEnd();
  }

  function destroy(): void {
    stopDragAutoScroll();
    document.removeEventListener('dragend', onDocumentDragEnd, true);
    document.removeEventListener('drop', onDocumentDragEnd);
  }

  return {
    get draggingPaneId() {
      return draggingPaneId;
    },
    get duplicateDropPaneId() {
      return duplicateDropPaneId;
    },
    get dropTarget() {
      return dropTarget;
    },
    get dropIndicator() {
      return dropIndicator;
    },
    get shielding() {
      return shielding;
    },
    onPaneDragStart,
    onHostDragOver,
    onPaneDragEnd,
    onHostDrop,
    onHostDragLeave,
    onShieldDragLeave,
    onShieldPointer,
    destroy,
  };
}
