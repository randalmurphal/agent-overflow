// Drag-and-drop state for manual project reordering.
//
// Uses HTML5 DnD over a custom pointer-based handler because:
//   - The browser already runs the drag cursor, modifier-key handling,
//     escape-cancel, and drop-target detection — duplicating those is
//     a non-trivial pile of edge cases.
//   - Auto-animate (Phase F) handles the post-drop reflow, so we don't
//     need a moving-during-drag preview; a thin drop indicator above /
//     below the hovered row is enough.
//   - Total surface is ~80 LOC, fits the codebase's "minimal code" bar.
//
// State lives at module scope so ProjectsSection (which owns the list)
// and ProjectItem (which owns each row's handle + drag events) share
// one source of truth without prop-drilling.
//
// Lifecycle: beginDrag → over (one or more) → drop or cancel. Cancel
// fires on Escape (HTML5 native) which the browser surfaces as a
// dragend without a preceding drop — so dropTargetId is cleared on
// dragend regardless of outcome.

let draggingId: string | null = $state(null);
let dropTargetId: string | null = $state(null);
let dropPosition: 'before' | 'after' | null = $state(null);

export function getDraggingProjectId(): string | null {
  return draggingId;
}

export function getDropTargetProjectId(): string | null {
  return dropTargetId;
}

export function getDropPosition(): 'before' | 'after' | null {
  return dropPosition;
}

export function beginProjectDrag(id: string, event: DragEvent): void {
  if (!event.dataTransfer) return;
  draggingId = id;
  event.dataTransfer.effectAllowed = 'move';
  // dataTransfer.setData is required to make the drag operation valid
  // in some browsers; the payload itself isn't used (we read draggingId
  // from module state).
  event.dataTransfer.setData('text/plain', id);
}

/**
 * Update the drop target while a drag is in progress over a project
 * row. Computes "before" or "after" based on whether the cursor is in
 * the top or bottom half of the row's bounding rect.
 */
export function updateDropTarget(targetId: string, event: DragEvent, rowEl: HTMLElement): void {
  if (draggingId === null || draggingId === targetId) {
    dropTargetId = null;
    dropPosition = null;
    return;
  }
  event.preventDefault();
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
  const rect = rowEl.getBoundingClientRect();
  const mid = rect.top + rect.height / 2;
  dropTargetId = targetId;
  dropPosition = event.clientY < mid ? 'before' : 'after';
}

/**
 * Compute the new ordered id list given the drag state. Returns null
 * when there's nothing to commit (no target, dropping on self, etc.).
 */
export function computeReorderedIds(currentOrder: readonly string[]): string[] | null {
  if (draggingId === null || dropTargetId === null || dropPosition === null) return null;
  if (draggingId === dropTargetId) return null;
  const without = currentOrder.filter((id) => id !== draggingId);
  const targetIndex = without.indexOf(dropTargetId);
  if (targetIndex === -1) return null;
  const insertAt = dropPosition === 'before' ? targetIndex : targetIndex + 1;
  // No-op detection: if the dragged item is already at the resulting
  // slot in `currentOrder`, skip the round-trip.
  const next = [...without.slice(0, insertAt), draggingId, ...without.slice(insertAt)];
  for (let i = 0; i < currentOrder.length; i++) {
    if (currentOrder[i] !== next[i]) return next;
  }
  return null;
}

export function endProjectDrag(): void {
  draggingId = null;
  dropTargetId = null;
  dropPosition = null;
}
