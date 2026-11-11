// Factory for the PaneHost drag/drop controller. Owns:
//   - pane-reorder drag state (sourcePaneId being moved within the host)
//   - the drop slot and its preview (ghost rectangle) for the two drags that
//     place a pane from the sidebar: a thread row and a tools-row button.
//     Both resolve their slot through one `resolvePaneDropTarget`.
//   - duplicate-pane highlight when dragging a thread already mounted
//   - edge-driven auto-scroll while a drag hovers the host edges
// Pulled out of PaneHost.svelte so the host file stays readable and the
// drag protocol becomes unit-testable through a single public surface.
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
  toolPaneIdFor,
  type PaneLayoutItem,
} from '../../stores/paneLayout.svelte';
import { isToolAtSlot, openTool } from '../../stores/toolPanes.svelte';
import {
  endThreadRowDrag,
  endToolPaneDrag,
  PANE_REORDER_DRAG_MIME,
  projectedPaneDropWidth,
  threadDragPayloadForEvent,
  toolDragPayloadForEvent,
  THREAD_ROW_DRAG_MIME,
  TOOL_PANE_DRAG_MIME,
  type PaneDropTarget,
} from '../../utils/threadDragPayload';
import { edgeAutoScrollVelocity } from './edgeAutoScroll';

const PANE_SELECTOR = '[data-pane-id]';
const PANE_GAP_SELECTOR = '[data-pane-gap-index]';

export interface PaneThreadDragOptions {
  getHostEl(): HTMLElement | undefined;
  getLayoutItems(): PaneLayoutItem[];
  getMinPaneWidth(): number;
  getScrollLeft(): number;
  setScrollLeft(value: number): void;
  getScrollClientWidth(): number;
  getPaneOffsetLeft(paneId: string): number;
  getPaneMeasuredWidth(paneId: string): number;
}

interface SlotNeighbours {
  before: string | null;
  after: string | null;
}

export function createPaneThreadDrag(options: PaneThreadDragOptions) {
  let draggingPaneId = $state<string | null>(null);
  let duplicateDropPaneId = $state<string | null>(null);
  let dropTarget = $state<PaneDropTarget | null>(null);
  let dropPreviewLeft = $state(0);
  let dropPreviewWidth = $state(0);

  // The panes either side of `dropTarget` when it was resolved: a drop that
  // resolves no slot of its own falls back to it only while they stand.
  let dropAnchor: SlotNeighbours | null = null;

  let autoScrollFrame: number | null = null;
  let autoScrollVelocity = 0;

  function neighboursOf(insertIndex: number): SlotNeighbours {
    const items = options.getLayoutItems();
    return { before: items[insertIndex - 1]?.paneId ?? null, after: items[insertIndex]?.paneId ?? null };
  }

  function setDropTarget(target: PaneDropTarget | null): void {
    dropTarget = target;
    dropAnchor = target ? neighboursOf(target.insertIndex) : null;
  }

  // A drop resolves its slot from its own event against the layout as it is
  // now; the last dragover's slot answers only when the event resolves none
  // and the panes it was computed between are still its neighbours.
  function dropSlot(event: DragEvent): PaneDropTarget | null {
    const resolved = resolvePaneDropTarget(event);
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

  function lastPaneElement(): HTMLElement | null {
    const host = options.getHostEl();
    const paneEls = host?.querySelectorAll<HTMLElement>(PANE_SELECTOR);
    return paneEls?.item(paneEls.length - 1) ?? null;
  }

  function resolvePaneDropTarget(event: DragEvent): PaneDropTarget | null {
    const layoutItems = options.getLayoutItems();
    if (layoutItems.length === 0) return { kind: 'empty', insertIndex: 0 };
    const target = event.target as HTMLElement | null;
    const gap = target?.closest<HTMLElement>(PANE_GAP_SELECTOR);
    if (gap?.dataset.paneGapIndex) {
      const insertIndex = Number(gap.dataset.paneGapIndex);
      if (!Number.isInteger(insertIndex)) return null;
      // The end-handle wrapper carries the gap index past the last pane
      // (== length). That is the append slot: normalize it to `end` so the
      // preview positions after the last pane instead of falling through to
      // the `gap` branch, where layoutItems[length] is undefined and the
      // ghost would snap to the strip's left edge.
      if (insertIndex >= layoutItems.length) {
        return { kind: 'end', insertIndex: layoutItems.length };
      }
      // The gap between a source pane and one of its companions is not a
      // valid slot (companions glue to their source): snap to the block's
      // right edge so the preview and the landing agree.
      if (insertIndex > 0 && insertIndex < layoutItems.length) {
        const { start, end } = paneBlockRangeAt(layoutItems, insertIndex);
        if (start < insertIndex) {
          return end + 1 >= layoutItems.length
            ? { kind: 'end', insertIndex: layoutItems.length }
            : { kind: 'gap', insertIndex: end + 1 };
        }
      }
      return { kind: 'gap', insertIndex };
    }

    const paneEl = target?.closest<HTMLElement>(PANE_SELECTOR);
    if (paneEl?.dataset.paneId) {
      const paneId = paneEl.dataset.paneId;
      const index = layoutItems.findIndex((item) => item.paneId === paneId);
      if (index < 0) return null;
      const rect = paneEl.getBoundingClientRect();
      const after = event.clientX > rect.left + rect.width / 2;
      // Companions glue to their source, so the slots around ANY pane in
      // a block are the block's edges — hovering a companion's left half
      // targets the slot before its source pane.
      const { start, end } = paneBlockRangeAt(layoutItems, index);
      if (after) {
        return { kind: 'pane-right', insertIndex: end + 1, paneId: layoutItems[end].paneId };
      }
      return { kind: 'pane-left', insertIndex: start, paneId: layoutItems[start].paneId };
    }

    const lastPane = lastPaneElement();
    if (lastPane && event.clientX >= lastPane.getBoundingClientRect().right) {
      return { kind: 'end', insertIndex: layoutItems.length };
    }
    return null;
  }

  // `movedWidth` is the measured width of a pane the drop moves; a new pane
  // takes the projected width.
  function publishDropPreview(target: PaneDropTarget, movedWidth?: number): void {
    const layoutItems = options.getLayoutItems();
    const host = options.getHostEl();
    const scrollLeft = options.getScrollLeft();
    const scrollClientWidth = options.getScrollClientWidth();
    const width = movedWidth || projectedPaneDropWidth(
      layoutItems,
      scrollClientWidth || host?.clientWidth || 0,
      options.getMinPaneWidth(),
    );
    dropPreviewWidth = width;
    if (target.kind === 'empty') {
      dropPreviewLeft = scrollLeft;
      return;
    }
    if (target.kind === 'end') {
      const lastPaneId = layoutItems.at(-1)?.paneId;
      const lastWidth = lastPaneId ? options.getPaneMeasuredWidth(lastPaneId) : 0;
      dropPreviewLeft = lastPaneId
        ? options.getPaneOffsetLeft(lastPaneId) + lastWidth
        : scrollLeft;
      return;
    }
    const targetPaneId = target.paneId ?? layoutItems[target.insertIndex]?.paneId;
    if (!targetPaneId) {
      dropPreviewLeft = scrollLeft;
      return;
    }
    const targetPaneLeft = options.getPaneOffsetLeft(targetPaneId);
    if (target.kind === 'pane-right') {
      dropPreviewLeft =
        targetPaneLeft + options.getPaneMeasuredWidth(targetPaneId) - width;
    } else {
      dropPreviewLeft = targetPaneLeft;
    }
  }

  function updateThreadDropTarget(event: DragEvent): void {
    const threadId = threadDragPayloadForEvent(event)?.threadId;
    if (!threadId) {
      setDropTarget(null);
      duplicateDropPaneId = null;
      return;
    }
    const existing = findPaneShowingThread(threadId);
    duplicateDropPaneId = existing?.paneId ?? null;
    if (existing) {
      setDropTarget(null);
      return;
    }

    const target = resolvePaneDropTarget(event);
    setDropTarget(target);
    if (target) publishDropPreview(target);
  }

  // An open tool moves rather than opening twice: its own slot shows no
  // preview, and any other shows the pane at its own width.
  function updateToolDropTarget(event: DragEvent): void {
    duplicateDropPaneId = null;
    const kind = toolDragPayloadForEvent(event)?.kind;
    const target = kind ? resolvePaneDropTarget(event) : null;
    setDropTarget(target && kind && !isToolAtSlot(kind, target.insertIndex) ? target : null);
    if (!dropTarget || !kind) return;
    const paneId = toolPaneIdFor(kind);
    const open = options.getLayoutItems().some((item) => item.paneId === paneId);
    publishDropPreview(dropTarget, open ? options.getPaneMeasuredWidth(paneId) : undefined);
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
      options.setScrollLeft(el.scrollLeft);
      autoScrollFrame = window.requestAnimationFrame(tick);
    };
    autoScrollFrame = window.requestAnimationFrame(tick);
  }

  function clearDropState(): void {
    duplicateDropPaneId = null;
    setDropTarget(null);
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
    }
  }

  function onPaneDrop(event: DragEvent, targetPaneId: string): void {
    if (isThreadDrag(event)) {
      event.stopPropagation();
      void handleThreadDrop(event);
      return;
    }
    if (isToolDrag(event)) {
      event.stopPropagation();
      handleToolDrop(event);
      return;
    }
    event.preventDefault();
    const sourcePaneId = draggingPaneId ?? event.dataTransfer?.getData(PANE_REORDER_DRAG_MIME);
    draggingPaneId = null;
    if (!sourcePaneId || sourcePaneId === targetPaneId) return;
    const layoutItems = options.getLayoutItems();
    const targetIndex = layoutItems.findIndex((item) => item.paneId === targetPaneId);
    const sourceIndex = layoutItems.findIndex((item) => item.paneId === sourcePaneId);
    if (targetIndex < 0) return;
    const target = event.currentTarget as HTMLElement;
    const rect = target.getBoundingClientRect();
    const after = event.clientX > rect.left + rect.width / 2;
    const targetInsertIndex = after ? targetIndex + 1 : targetIndex;
    const adjustedInsertIndex =
      sourceIndex >= 0 && sourceIndex < targetInsertIndex
        ? targetInsertIndex - 1
        : targetInsertIndex;
    movePaneLayoutItemToIndex(sourcePaneId, adjustedInsertIndex);
  }

  function onPaneDragEnd(): void {
    draggingPaneId = null;
    clearDropState();
  }

  function onHostDrop(event: DragEvent): void {
    if (isThreadDrag(event)) void handleThreadDrop(event);
    else if (isToolDrag(event)) handleToolDrop(event);
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
    get dropPreviewLeft() {
      return dropPreviewLeft;
    },
    get dropPreviewWidth() {
      return dropPreviewWidth;
    },
    onPaneDragStart,
    onHostDragOver,
    onPaneDrop,
    onPaneDragEnd,
    onHostDrop,
    onHostDragLeave,
    destroy,
  };
}
