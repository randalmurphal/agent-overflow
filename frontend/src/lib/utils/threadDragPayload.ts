export const THREAD_ROW_DRAG_MIME = 'application/x-agent-overflow-thread';
export const PANE_REORDER_DRAG_MIME = 'application/x-agent-overflow-pane';

export interface ThreadDragPayload {
  threadId: string;
  title: string;
  /**
   * The dragged thread's project. A group drop target refuses a thread
   * from another project, and the ungroup drop needs to know whose list
   * it landed in — neither can ask the store, because a drag can end in a
   * surface that never rendered the row.
   */
  projectId: string;
  /** The thread's current group, when it is in one. */
  groupId?: string;
}

export type ThreadDropKind = 'pane-left' | 'pane-right' | 'gap' | 'end' | 'empty';

export interface ThreadDropTarget {
  kind: ThreadDropKind;
  insertIndex: number;
  paneId?: string;
}

interface PaneWidthLike {
  widthPx: number;
}

export function encodeThreadDragPayload(payload: ThreadDragPayload): string {
  return JSON.stringify(payload);
}

export function decodeThreadDragPayload(raw: string): ThreadDragPayload | null {
  try {
    const parsed = JSON.parse(raw) as Partial<ThreadDragPayload>;
    if (!parsed.threadId || typeof parsed.threadId !== 'string') return null;
    return {
      threadId: parsed.threadId,
      title: typeof parsed.title === 'string' ? parsed.title : 'Untitled',
      // Both stay tolerant of an absent field: a payload minted by an
      // older client (or a pane drag that never needed them) must still
      // decode into a usable pane drop rather than null.
      projectId: typeof parsed.projectId === 'string' ? parsed.projectId : '',
      ...(typeof parsed.groupId === 'string' && parsed.groupId
        ? { groupId: parsed.groupId }
        : {}),
    };
  } catch {
    return null;
  }
}

/**
 * The drag in flight, recorded by the source row at `dragstart`.
 *
 * `DataTransfer.getData` is empty during `dragover` in every real browser —
 * the store is in protected mode until the drop — but a group drop target has
 * to decide DURING dragover whether it can accept this thread (project match,
 * not already a member) to set `dropEffect` and light the row. Both ends of
 * the drag live in this document, so the source simply leaves the payload
 * here and clears it on `dragend` — and every drop target clears it too,
 * because a source row that unmounts mid-drag (its project collapsed, a
 * search typed) fires no `dragend` at all.
 */
let inFlightThreadDrag: ThreadDragPayload | null = null;

export function beginThreadRowDrag(payload: ThreadDragPayload): void {
  inFlightThreadDrag = payload;
}

export function endThreadRowDrag(): void {
  inFlightThreadDrag = null;
}

/**
 * The payload behind a drag event: the DataTransfer when it will talk (drop),
 * the in-flight record when it won't (dragover). Null unless the event really
 * carries a thread row, so a stale record can never answer for a file drag.
 */
export function threadDragPayloadForEvent(event: DragEvent): ThreadDragPayload | null {
  const data = event.dataTransfer;
  if (!data) return null;
  if (!data.types.includes(THREAD_ROW_DRAG_MIME)) return null;
  const raw = data.getData(THREAD_ROW_DRAG_MIME);
  if (raw) return decodeThreadDragPayload(raw);
  return inFlightThreadDrag;
}

/**
 * A group row (or any row inside it) accepts a thread from its OWN project
 * that is not already a member. Cross-project is refused outright — a group
 * belongs to one project and the backend would reject the move anyway.
 */
export function canDropThreadInGroup(
  payload: ThreadDragPayload,
  projectId: string,
  groupId: string,
): boolean {
  return Boolean(projectId) && payload.projectId === projectId && payload.groupId !== groupId;
}

/**
 * The list background ungroups: only a thread that IS in a group, and only in
 * the list of its own project.
 */
export function canUngroupDroppedThread(
  payload: ThreadDragPayload,
  projectId: string,
): boolean {
  return Boolean(projectId) && payload.projectId === projectId && Boolean(payload.groupId);
}

export function projectedPaneDropWidth(
  items: PaneWidthLike[],
  paneRowWidth: number,
  minPaneWidth: number,
): number {
  if (items.length === 0) return Math.max(minPaneWidth, paneRowWidth);
  const total = items.reduce((sum, item) => sum + item.widthPx, 0);
  const average = total / items.length;
  if (!Number.isFinite(total) || total <= 0 || !Number.isFinite(average) || average <= 0) {
    return minPaneWidth;
  }
  // Fit mode stretches panes proportionally, so the new pane's share of
  // the row is average/(total+average); in overflow mode there is no
  // stretch and the pane lands at its base width (the average) verbatim.
  // The two regimes cross exactly where total+average === paneRowWidth,
  // so the rendered width is simply the larger of the two.
  const projected = (average / (total + average)) * paneRowWidth;
  return Math.max(minPaneWidth, Math.round(Math.max(projected, average)));
}
