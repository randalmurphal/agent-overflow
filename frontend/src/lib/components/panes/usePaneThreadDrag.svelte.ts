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
// pointer's half of the block it is over picks the nearer edge. The
// indicator is a bar on that seam plus the half that chose it.
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
  isCompanionKind,
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
  getHostEl(): HTMLElement | undefined;
  getLayoutItems(): PaneLayoutItem[];
}

/** Where the indicator paints, in the strip's scroll-content pixels. */
export interface PaneDropIndicator {
  seamLeft: number;
  /** The half of the block under the pointer; `seam` is the side its bar is on. */
  lit: { left: number; width: number; seam: 'left' | 'right' } | null;
}

interface SlotNeighbours {
  before: string | null;
  after: string | null;
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

  // The panes either side of `dropTarget` when it was resolved: a drop that
  // resolves no slot of its own falls back to it only while they stand.
  let dropAnchor: SlotNeighbours | null = null;

  let autoScrollFrame: number | null = null;
  let autoScrollVelocity = 0;

  function neighboursOf(insertIndex: number): SlotNeighbours {
    const items = options.getLayoutItems();
    return { before: items[insertIndex - 1]?.paneId ?? null, after: items[insertIndex]?.paneId ?? null };
  }

  function sameIndicator(a: PaneDropIndicator | null, b: PaneDropIndicator | null): boolean {
    if (!a || !b) return a === b;
    if (a.seamLeft !== b.seamLeft || !a.lit !== !b.lit) return false;
    return !a.lit || !b.lit || (a.lit.left === b.lit.left && a.lit.width === b.lit.width && a.lit.seam === b.lit.seam);
  }

  // The seam bar is clamped inside the strip's content so the first and
  // last seams stay whole and the bar never widens the scroll range.
  function indicatorFor(resolved: ResolvedDrop): PaneDropIndicator | null {
    const host = options.getHostEl();
    if (!host || resolved.seam === null) return null;
    const origin = host.getBoundingClientRect().left + host.clientLeft - host.scrollLeft;
    const maxLeft = Math.max(0, host.scrollWidth - PANE_DROP_SEAM_PX);
    const seamLeft = Math.min(maxLeft, Math.max(0, Math.round(resolved.seam - origin - PANE_DROP_SEAM_PX / 2)));
    const lit = resolved.lit;
    return {
      seamLeft,
      lit: lit && {
        left: Math.round(lit.left - origin),
        width: Math.round(lit.right - lit.left),
        seam: resolved.seam <= lit.left ? 'left' : 'right',
      },
    };
  }

  function showDrop(resolved: ResolvedDrop | null): void {
    const target = resolved?.target ?? null;
    if (target?.kind !== dropTarget?.kind || target?.insertIndex !== dropTarget?.insertIndex) dropTarget = target;
    dropAnchor = target ? neighboursOf(target.insertIndex) : null;
    const indicator = resolved ? indicatorFor(resolved) : null;
    if (!sameIndicator(indicator, dropIndicator)) dropIndicator = indicator;
  }

  // A drop resolves its slot from its own event against the layout as it is
  // now; the last dragover's slot answers only when the event resolves none
  // and the panes it was computed between are still its neighbours.
  function dropSlot(event: DragEvent): PaneDropTarget | null {
    const resolved = resolveDrop(event)?.target ?? null;
    if (resolved || !dropTarget || !dropAnchor) return resolved;
    const now = neighboursOf(dropTarget.insertIndex);
    return now.before === dropAnchor.before && now.after === dropAnchor.after ? dropTarget : null;
  }

  // Every sidebar drag ends here, wherever it ends: a dragend at its source
  // (a drop, Escape, a drop outside the window) or a drop that bubbles to
  // the document. A source that unmounted mid-drag gets no dragend; leaving
  // the strip (onHostDragLeave) still clears the preview then.
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

  function isToolDrag(event: DragEvent): boolean {
    return event.dataTransfer?.types.includes(TOOL_PANE_DRAG_MIME) ?? false;
  }

  // Pane sections and gap wrappers are the strip's own children; content
  // inside a pane can carry a pane id of its own (a header title).
  function stripChildOf(node: EventTarget | null): HTMLElement | null {
    const host = options.getHostEl();
    let el = node instanceof HTMLElement ? node : null;
    while (el && el.parentElement !== host) el = el.parentElement;
    return el;
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
  function besideBlock(items: PaneLayoutItem[], range: { start: number; end: number }, clientX: number): ResolvedDrop | null {
    const first = paneExtent(items[range.start].paneId);
    const last = paneExtent(items[range.end].paneId);
    if (!first || !last) return null;
    const middle = (first.left + last.right) / 2;
    if (clientX > middle) {
      return { target: { kind: 'pane-right', insertIndex: range.end + 1 }, seam: last.right, lit: { left: middle, right: last.right } };
    }
    return { target: { kind: 'pane-left', insertIndex: range.start }, seam: first.left, lit: { left: first.left, right: middle } };
  }

  function atEnd(items: PaneLayoutItem[]): ResolvedDrop | null {
    const last = paneExtent(items[items.length - 1].paneId);
    return last && { target: { kind: 'end', insertIndex: items.length }, seam: last.right, lit: null };
  }

  function resolveDrop(event: DragEvent): ResolvedDrop | null {
    const items = options.getLayoutItems();
    if (items.length === 0) return { target: { kind: 'empty', insertIndex: 0 }, seam: null, lit: null };
    const child = stripChildOf(event.target);
    if (child?.dataset.paneGapIndex) {
      const insertIndex = Number(child.dataset.paneGapIndex);
      if (!Number.isInteger(insertIndex) || insertIndex < 1) return null;
      // The end handle carries the gap index past the last pane.
      if (insertIndex >= items.length) return atEnd(items);
      // The gap between a source and its companion is not a slot.
      const range = paneBlockRangeAt(items, insertIndex);
      if (range.start < insertIndex) return besideBlock(items, range, event.clientX);
      const next = paneExtent(items[insertIndex].paneId);
      return next && { target: { kind: 'gap', insertIndex }, seam: next.left, lit: null };
    }
    if (child?.dataset.paneId) {
      const index = items.findIndex((item) => item.paneId === child.dataset.paneId);
      return index < 0 ? null : besideBlock(items, paneBlockRangeAt(items, index), event.clientX);
    }
    // The strip's own background, right of the last pane.
    const last = paneExtent(items[items.length - 1].paneId);
    return last && event.clientX >= last.right ? atEnd(items) : null;
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
    if (event.dataTransfer) {
      event.dataTransfer.setData('text/plain', paneId);
      event.dataTransfer.effectAllowed = 'move';
    }
  }

  function onHostDragOver(event: DragEvent): void {
    if (isThreadDrag(event)) {
      event.preventDefault();
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'copy';
      updateThreadDropTarget(event);
      updateDragAutoScroll(event.clientX);
      return;
    }
    if (isToolDrag(event)) {
      event.preventDefault();
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
      updateToolDropTarget(event);
      updateDragAutoScroll(event.clientX);
      return;
    }
    if (draggingPaneId) {
      event.preventDefault();
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
    else {
      const paneId = draggingPaneId ?? event.dataTransfer?.getData(PANE_REORDER_DRAG_MIME);
      if (paneId) handleReorderDrop(event, paneId);
      // Dragged text or a link is none of the strip's drags: a thread or
      // tool pane refuses it, a companion's own fields take it.
      else if (overLeadPane(event)) event.preventDefault();
    }
  }

  function overLeadPane(event: DragEvent): boolean {
    const paneId = stripChildOf(event.target)?.dataset.paneId;
    const item = paneId ? options.getLayoutItems().find((candidate) => candidate.paneId === paneId) : undefined;
    return item !== undefined && !isCompanionKind(item.kind);
  }

  // A dragleave bubbles from every child the pointer leaves; the drag left
  // the strip only when it enters nothing inside it. A cancel and a drop
  // outside the window leave with no related target.
  function onHostDragLeave(event: DragEvent): void {
    const next = event.relatedTarget;
    const host = options.getHostEl();
    if (next instanceof Node && host?.contains(next)) return;
    clearDropState();
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
    onPaneDragStart,
    onHostDragOver,
    onPaneDragEnd,
    onHostDrop,
    onHostDragLeave,
    destroy,
  };
}
