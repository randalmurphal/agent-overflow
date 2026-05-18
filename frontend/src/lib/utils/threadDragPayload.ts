export const THREAD_ROW_DRAG_MIME = 'application/x-agent-overflow-thread';
export const PANE_REORDER_DRAG_MIME = 'application/x-agent-overflow-pane';

export interface ThreadDragPayload {
  threadId: string;
  title: string;
}

export type ThreadDropKind = 'pane-left' | 'pane-right' | 'gap' | 'end' | 'empty';

export interface ThreadDropTarget {
  kind: ThreadDropKind;
  insertIndex: number;
  paneId?: string;
}

interface PaneRatioLike {
  ratio: number;
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
    };
  } catch {
    return null;
  }
}

export function projectedPaneDropWidth(
  items: PaneRatioLike[],
  paneRowWidth: number,
  minPaneWidth: number,
): number {
  if (items.length === 0) return Math.max(minPaneWidth, paneRowWidth);
  const total = items.reduce((sum, item) => sum + item.ratio, 0);
  const average = total / items.length;
  if (!Number.isFinite(total) || total <= 0 || !Number.isFinite(average) || average <= 0) {
    return minPaneWidth;
  }
  const projected = (average / (total + average)) * paneRowWidth;
  return Math.max(minPaneWidth, Math.round(projected));
}
