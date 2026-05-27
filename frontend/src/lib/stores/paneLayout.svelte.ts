export type PaneLayoutKind = 'thread';

export interface PaneLayoutItem {
  id: string;
  paneId: string;
  kind: PaneLayoutKind;
  ratio: number;
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
  return {
    id: item.id,
    paneId: item.paneId,
    kind: item.kind,
    ratio: normalizeRatio(item.ratio),
  };
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
  layoutItems = next;
  requestLayoutPersistence();
}

export function movePaneLayoutItemToIndex(paneId: string, insertIndex: number): void {
  const index = layoutItems.findIndex((item) => item.paneId === paneId);
  if (index < 0) return;
  const next = layoutItems.slice();
  const [item] = next.splice(index, 1);
  const clamped = Math.max(0, Math.min(next.length, insertIndex));
  next.splice(clamped, 0, item);
  layoutItems = next;
  requestLayoutPersistence();
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
