// Sidebar visual cursor — a $state thread-id that floats over the
// rendered sidebar without touching DOM focus. mod+j / mod+k step it
// up and down; mod+enter / mod+shift+enter activate the row.
//
// The cursor is a thread id (not an index) because the visible-row
// list re-derives constantly (filters, expand/collapse, status
// streaming) — an index would drift. The store reads the rendered
// DOM order via `getVisibleSidebarThreadIds()` to step; consumers
// (ThreadRow) read `getSidebarCursorThreadId()` reactively to draw
// the decoration.

import { getVisibleSidebarThreadIds } from './sidebarThreadOrder';

let cursorThreadId: string | null = $state(null);

export function getSidebarCursorThreadId(): string | null {
  return cursorThreadId;
}

export function clearSidebarCursor(): void {
  cursorThreadId = null;
}

/**
 * Move the cursor by `delta` positions through the rendered sidebar
 * order. Wraps at both ends. When the cursor has no current anchor
 * (cold start) or its anchor has fallen out of the visible set, the
 * fallback parameter chooses the initial landing — typically the
 * focused pane's thread id so the user sees a no-op first press
 * followed by motion.
 */
export function stepSidebarCursor(delta: -1 | 1, fallbackThreadId: string | null): void {
  const ids = getVisibleSidebarThreadIds();
  if (ids.length === 0) {
    cursorThreadId = null;
    return;
  }
  const currentIndex = cursorThreadId ? ids.indexOf(cursorThreadId) : -1;
  const cursorMissing = currentIndex === -1;
  let baseIndex = currentIndex;
  if (cursorMissing && fallbackThreadId) {
    baseIndex = ids.indexOf(fallbackThreadId);
  }
  let nextIndex: number;
  if (baseIndex === -1) {
    // No anchor at all — first press lands on row 0 going down, last
    // row going up.
    nextIndex = delta > 0 ? 0 : ids.length - 1;
  } else if (cursorMissing) {
    // Fallback supplied an anchor (cold start). Land on it without
    // stepping — user sees the cursor appear on their current thread,
    // next press moves.
    nextIndex = baseIndex;
  } else {
    nextIndex = (baseIndex + delta + ids.length) % ids.length;
  }
  cursorThreadId = ids[nextIndex];
}

/** Test seed — bypasses the DOM walk. */
export function setSidebarCursorForTest(threadId: string | null): void {
  cursorThreadId = threadId;
}

export function resetSidebarCursorStore(): void {
  cursorThreadId = null;
}
