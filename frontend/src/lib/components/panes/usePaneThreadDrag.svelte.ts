// Factory for the PaneHost drag/drop controller. Owns:
//   - pane-reorder drag state (sourcePaneId being moved within the host)
//   - thread-row drop preview (ghost rectangle showing the target slot)
//   - duplicate-pane highlight when dragging a thread already mounted
//   - edge-driven auto-scroll while a drag hovers the host edges
// Pulled out of PaneHost.svelte so the host file stays readable and the
// drag protocol becomes unit-testable through a single public surface.

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
import {
  decodeThreadDragPayload,
  endThreadRowDrag,
  PANE_REORDER_DRAG_MIME,
  projectedPaneDropWidth,
  THREAD_ROW_DRAG_MIME,
  type ThreadDropTarget,
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

export function createPaneThreadDrag(options: PaneThreadDragOptions) {
  let draggingPaneId = $state<string | null>(null);
  let draggedThreadId = $state<string | null>(null);
  let duplicateDropPaneId = $state<string | null>(null);
  let threadDropTarget = $state<ThreadDropTarget | null>(null);
  let threadDropPreviewLeft = $state(0);
  let threadDropPreviewWidth = $state(0);

  let cachedDragPayloadRaw: string | null = null;
  let cachedDragThreadId: string | null = null;
  let autoScrollFrame: number | null = null;
  let autoScrollVelocity = 0;

  function isThreadDrag(event: DragEvent): boolean {
    return event.dataTransfer?.types.includes(THREAD_ROW_DRAG_MIME) ?? false;
  }

  function threadPayloadFromEvent(event: DragEvent) {
    const raw = event.dataTransfer?.getData(THREAD_ROW_DRAG_MIME) ?? '';
    if (raw === cachedDragPayloadRaw) {
      return cachedDragThreadId ? { threadId: cachedDragThreadId, title: '' } : null;
    }
    cachedDragPayloadRaw = raw;
    const payload = decodeThreadDragPayload(raw);
    cachedDragThreadId = payload?.threadId ?? null;
    return payload;
  }

  function lastPaneElement(): HTMLElement | null {
    const host = options.getHostEl();
    const paneEls = host?.querySelectorAll<HTMLElement>(PANE_SELECTOR);
    return paneEls?.item(paneEls.length - 1) ?? null;
  }

  function resolveThreadDropTarget(event: DragEvent): ThreadDropTarget | null {
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

  function publishThreadDropPreview(target: ThreadDropTarget): void {
    const layoutItems = options.getLayoutItems();
    const host = options.getHostEl();
    const scrollLeft = options.getScrollLeft();
    const scrollClientWidth = options.getScrollClientWidth();
    const width = projectedPaneDropWidth(
      layoutItems,
      scrollClientWidth || host?.clientWidth || 0,
      options.getMinPaneWidth(),
    );
    threadDropPreviewWidth = width;
    if (target.kind === 'empty') {
      threadDropPreviewLeft = scrollLeft;
      return;
    }
    if (target.kind === 'end') {
      const lastPaneId = layoutItems.at(-1)?.paneId;
      const lastWidth = lastPaneId ? options.getPaneMeasuredWidth(lastPaneId) : 0;
      threadDropPreviewLeft = lastPaneId
        ? options.getPaneOffsetLeft(lastPaneId) + lastWidth
        : scrollLeft;
      return;
    }
    const targetPaneId = target.paneId ?? layoutItems[target.insertIndex]?.paneId;
    if (!targetPaneId) {
      threadDropPreviewLeft = scrollLeft;
      return;
    }
    const targetPaneLeft = options.getPaneOffsetLeft(targetPaneId);
    if (target.kind === 'pane-right') {
      threadDropPreviewLeft =
        targetPaneLeft + options.getPaneMeasuredWidth(targetPaneId) - width;
    } else {
      threadDropPreviewLeft = targetPaneLeft;
    }
  }

  function updateThreadDropTarget(event: DragEvent): void {
    const payload = threadPayloadFromEvent(event);
    draggedThreadId = payload?.threadId ?? draggedThreadId;
    const threadId = payload?.threadId ?? draggedThreadId;
    if (!threadId) {
      threadDropTarget = null;
      duplicateDropPaneId = null;
      return;
    }
    const existing = findPaneShowingThread(threadId);
    duplicateDropPaneId = existing?.paneId ?? null;
    if (existing) {
      threadDropTarget = null;
      return;
    }

    const target = resolveThreadDropTarget(event);
    threadDropTarget = target;
    if (target) publishThreadDropPreview(target);
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

  function clearThreadDragState(): void {
    draggedThreadId = null;
    duplicateDropPaneId = null;
    threadDropTarget = null;
    cachedDragPayloadRaw = null;
    cachedDragThreadId = null;
    stopDragAutoScroll();
  }

  async function handleThreadDrop(event: DragEvent): Promise<void> {
    event.preventDefault();
    const payload = threadPayloadFromEvent(event);
    const threadId = payload?.threadId ?? draggedThreadId;
    const target = threadDropTarget ?? resolveThreadDropTarget(event);
    clearThreadDragState();
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
    clearThreadDragState();
  }

  function onHostDrop(event: DragEvent): void {
    if (isThreadDrag(event)) void handleThreadDrop(event);
  }

  function onHostDragLeave(event: DragEvent): void {
    if (event.currentTarget === event.target) clearThreadDragState();
  }

  function destroy(): void {
    stopDragAutoScroll();
  }

  return {
    get draggingPaneId() {
      return draggingPaneId;
    },
    get draggedThreadId() {
      return draggedThreadId;
    },
    get duplicateDropPaneId() {
      return duplicateDropPaneId;
    },
    get threadDropTarget() {
      return threadDropTarget;
    },
    get threadDropPreviewLeft() {
      return threadDropPreviewLeft;
    },
    get threadDropPreviewWidth() {
      return threadDropPreviewWidth;
    },
    isThreadDrag,
    onPaneDragStart,
    onHostDragOver,
    onPaneDrop,
    onPaneDragEnd,
    onHostDrop,
    onHostDragLeave,
    destroy,
  };
}
