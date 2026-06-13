// 'thread' panes host a ChatView for a ThreadPane (the registry in
// panes.svelte.ts). 'take-control' panes host the read-only/attach terminal
// mirror of a claude-tui session's PTY; they are NOT ThreadPanes, are never
// persisted (buildSnapshot skips any layout item with no ThreadPane), and are
// always bound to a source thread pane via `sourcePaneId`. The take-control
// registry in takeControl.svelte.ts owns their runtime state and the
// open/close/cascade/switch-follow lifecycle.
export type PaneLayoutKind = 'thread' | 'take-control';

export interface PaneLayoutItem {
  id: string;
  paneId: string;
  kind: PaneLayoutKind;
  ratio: number;
  // Set only on 'take-control' items: the paneId of the source thread pane
  // this terminal mirror is paired to. Drives adjacency (it sits immediately
  // right of its source) and cascade close (source closes → this closes).
  sourcePaneId?: string;
}

export const DEFAULT_PANE_RATIO = 1;

interface PaneLayoutPersistenceHandlers {
  immediate: () => void;
  debounced: () => void;
  flush: () => Promise<void>;
}

interface PaneLayoutMutationOptions {
  persist?: boolean;
}

function defaultMainPaneLayoutItem(): PaneLayoutItem {
  return {
    id: 'main',
    paneId: 'main',
    kind: 'thread',
    ratio: DEFAULT_PANE_RATIO,
  };
}

let layoutItems: PaneLayoutItem[] = $state([]);
let persistenceHandlers: PaneLayoutPersistenceHandlers | null = null;

export function setPaneLayoutPersistenceHandlers(handlers: PaneLayoutPersistenceHandlers | null): void {
  persistenceHandlers = handlers;
}

function requestLayoutPersistence(debounced = false): void {
  if (!persistenceHandlers) return;
  if (debounced) {
    persistenceHandlers.debounced();
  } else {
    persistenceHandlers.immediate();
  }
}

export function flushPaneLayoutPersistence(): Promise<void> {
  return persistenceHandlers?.flush() ?? Promise.resolve();
}

function shouldPersist(options?: PaneLayoutMutationOptions): boolean {
  return options?.persist !== false;
}

function normalizeRatio(ratio: number): number {
  if (!Number.isFinite(ratio) || ratio <= 0) return DEFAULT_PANE_RATIO;
  return ratio;
}

function cloneLayoutItem(item: PaneLayoutItem): PaneLayoutItem {
  const cloned: PaneLayoutItem = {
    id: item.id,
    paneId: item.paneId,
    kind: item.kind,
    ratio: normalizeRatio(item.ratio),
  };
  if (item.sourcePaneId !== undefined) cloned.sourcePaneId = item.sourcePaneId;
  return cloned;
}

export function getPaneLayoutItems(): PaneLayoutItem[] {
  return layoutItems;
}

export function addPaneLayoutItem(
  item: PaneLayoutItem,
  insertIndex?: number,
  options?: PaneLayoutMutationOptions,
): void {
  if (layoutItems.some((existing) => existing.paneId === item.paneId)) return;
  const nextItem = cloneLayoutItem(item);
  const index = insertIndex === undefined
    ? layoutItems.length
    : Math.max(0, Math.min(layoutItems.length, insertIndex));
  layoutItems = [
    ...layoutItems.slice(0, index),
    nextItem,
    ...layoutItems.slice(index),
  ];
  if (shouldPersist(options)) requestLayoutPersistence();
}

export function removePaneLayoutItem(paneId: string, options?: PaneLayoutMutationOptions): void {
  const next = layoutItems.filter((item) => item.paneId !== paneId);
  if (next.length === layoutItems.length) return;
  layoutItems = next;
  if (shouldPersist(options)) requestLayoutPersistence();
}

export function movePaneLayoutItem(paneId: string, direction: -1 | 1): void {
  const index = layoutItems.findIndex((item) => item.paneId === paneId);
  if (index < 0) return;
  const nextIndex = index + direction;
  if (nextIndex < 0 || nextIndex >= layoutItems.length) return;
  const next = layoutItems.slice();
  const [item] = next.splice(index, 1);
  next.splice(nextIndex, 0, item);
  layoutItems = resnapTakeControlItems(next);
  requestLayoutPersistence();
}

export function movePaneLayoutItemToIndex(paneId: string, insertIndex: number): void {
  const index = layoutItems.findIndex((item) => item.paneId === paneId);
  if (index < 0) return;
  const next = layoutItems.slice();
  const [item] = next.splice(index, 1);
  const clamped = Math.max(0, Math.min(next.length, insertIndex));
  next.splice(clamped, 0, item);
  layoutItems = resnapTakeControlItems(next);
  requestLayoutPersistence();
}

// resnapTakeControlItems re-pins every 'take-control' pane immediately to the
// right of the thread pane it is paired to (sourcePaneId), preserving the
// relative order of thread panes. This is the structural guarantee behind the
// user-facing rule "it doesn't leave a dangling mf on the side": no reorder can
// separate a take-control pane from its source. A take-control item whose
// source is gone is dropped (the cascade-close path is the authoritative
// remover; this is only a defensive sweep so a transient orphan can't render).
function resnapTakeControlItems(items: PaneLayoutItem[]): PaneLayoutItem[] {
  const takeControlBySource = new Map<string, PaneLayoutItem>();
  const threadItems: PaneLayoutItem[] = [];
  for (const item of items) {
    if (item.kind === 'take-control' && item.sourcePaneId) {
      takeControlBySource.set(item.sourcePaneId, item);
    } else {
      threadItems.push(item);
    }
  }
  const result: PaneLayoutItem[] = [];
  for (const item of threadItems) {
    result.push(item);
    const paired = takeControlBySource.get(item.paneId);
    if (paired) result.push(paired);
  }
  return result;
}

export function resizeAdjacentPaneLayoutItems(
  leftPaneId: string,
  rightPaneId: string,
  leftWidth: number,
  rightWidth: number,
  deltaPx: number,
  minPaneWidth: number,
): void {
  const leftIndex = layoutItems.findIndex((item) => item.paneId === leftPaneId);
  const rightIndex = layoutItems.findIndex((item) => item.paneId === rightPaneId);
  if (leftIndex < 0 || rightIndex < 0 || Math.abs(leftIndex - rightIndex) !== 1) return;
  if (!Number.isFinite(leftWidth) || !Number.isFinite(rightWidth)) return;
  if (leftWidth <= 0 || rightWidth <= 0) return;
  const combinedWidth = leftWidth + rightWidth;
  if (combinedWidth < minPaneWidth * 2) return;

  const nextLeftWidth = Math.max(
    minPaneWidth,
    Math.min(combinedWidth - minPaneWidth, leftWidth + deltaPx),
  );
  const nextRightWidth = combinedWidth - nextLeftWidth;
  const combinedRatio = normalizeRatio(layoutItems[leftIndex].ratio) +
    normalizeRatio(layoutItems[rightIndex].ratio);
  const next = layoutItems.map(cloneLayoutItem);
  next[leftIndex] = {
    ...next[leftIndex],
    ratio: (nextLeftWidth / combinedWidth) * combinedRatio,
  };
  next[rightIndex] = {
    ...next[rightIndex],
    ratio: (nextRightWidth / combinedWidth) * combinedRatio,
  };
  layoutItems = next;
  requestLayoutPersistence(true);
}

export function averagePaneRatio(): number {
  if (layoutItems.length === 0) return DEFAULT_PANE_RATIO;
  const total = layoutItems.reduce((sum, item) => sum + normalizeRatio(item.ratio), 0);
  return total / layoutItems.length;
}

export function resetPaneLayoutForTest(): void {
  layoutItems = [defaultMainPaneLayoutItem()];
}

export function setPaneLayoutItems(items: PaneLayoutItem[]): void {
  layoutItems = items.map(cloneLayoutItem);
}

export function setPaneLayoutItemsForTest(items: PaneLayoutItem[]): void {
  setPaneLayoutItems(items);
}
