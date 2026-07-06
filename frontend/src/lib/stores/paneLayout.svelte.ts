// 'thread' panes host a ChatView for a ThreadPane (the registry in
// panes.svelte.ts). Companion panes are NOT ThreadPanes: they host surfaces
// paired to a source thread pane via `sourcePaneId`. take-control remains
// ephemeral; plan, design-preview, and review companions are persisted by layout
// persistence and restored through companionPanes.svelte.ts.
export type PaneLayoutKind = 'thread' | 'take-control' | 'plan' | 'design-preview' | 'review';
export type CompanionPaneKind = 'take-control' | 'plan' | 'design-preview' | 'review';

export function isCompanionKind(kind: PaneLayoutKind): kind is CompanionPaneKind {
  return kind === 'take-control' || kind === 'plan' || kind === 'design-preview' || kind === 'review';
}

export interface PaneLayoutItem {
  id: string;
  paneId: string;
  kind: PaneLayoutKind;
  ratio: number;
  // Set only on companion items: the paneId of the source thread pane this
  // surface is paired to. Drives adjacency (companions sit immediately right
  // of their source) and cascade close (source closes → companions close).
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
  // Resnap so an insert aimed INSIDE a source+companions block (e.g. "new
  // pane right of the focused pane" while that pane has a review open)
  // lands after the block instead of splitting it.
  layoutItems = resnapCompanionItems([
    ...layoutItems.slice(0, index),
    nextItem,
    ...layoutItems.slice(index),
  ]);
  if (shouldPersist(options)) requestLayoutPersistence();
}

export function removePaneLayoutItem(paneId: string, options?: PaneLayoutMutationOptions): void {
  const next = layoutItems.filter((item) => item.paneId !== paneId);
  if (next.length === layoutItems.length) return;
  layoutItems = next;
  if (shouldPersist(options)) requestLayoutPersistence();
}

// A source pane and its companions move as ONE unit: a single-slot move
// that only swapped the pane with its own companion would be undone by the
// resnap (a visible no-op), and a thread could never step past a
// neighbor's companion run. Blocks are [lead + its companions]; moving by
// one means moving past the whole adjacent block.
export function movePaneLayoutItem(paneId: string, direction: -1 | 1): void {
  const blocks = groupPaneBlocks(layoutItems);
  const blockIndex = blocks.findIndex((block) => block.some((item) => item.paneId === paneId));
  if (blockIndex < 0) return;
  const nextIndex = blockIndex + direction;
  if (nextIndex < 0 || nextIndex >= blocks.length) return;
  const next = blocks.slice();
  const [block] = next.splice(blockIndex, 1);
  next.splice(nextIndex, 0, block);
  layoutItems = resnapCompanionItems(next.flat());
  requestLayoutPersistence();
}

export function movePaneLayoutItemToIndex(paneId: string, insertIndex: number): void {
  const index = layoutItems.findIndex((item) => item.paneId === paneId);
  if (index < 0) return;
  const next = layoutItems.slice();
  const [item] = next.splice(index, 1);
  const clamped = Math.max(0, Math.min(next.length, insertIndex));
  next.splice(clamped, 0, item);
  layoutItems = resnapCompanionItems(next);
  requestLayoutPersistence();
}

// resnapCompanionItems re-pins every companion pane immediately to the right
// of the thread pane it is paired to (sourcePaneId), preserving the relative
// order of thread panes and the relative order of companions for the same
// source. This is the structural guarantee behind the user-facing rule "it
// doesn't leave a dangling mf on the side": no reorder can separate companions
// from their source. A companion item whose source is gone is dropped (the
// cascade-close path is the authoritative remover; this is only a defensive
// sweep so a transient orphan can't render).
/**
 * Item-index range of the [source + companions] block containing `index`
 * in a snapped layout. A plain thread pane is a block of one. Drop
 * targeting uses this so the only insert slots offered around a block are
 * its edges.
 */
export function paneBlockRangeAt(
  items: readonly PaneLayoutItem[],
  index: number,
): { start: number; end: number } {
  let start = index;
  const item = items[index];
  if (item && isCompanionKind(item.kind) && item.sourcePaneId) {
    const leadIndex = items.findIndex((candidate) => candidate.paneId === item.sourcePaneId);
    if (leadIndex >= 0 && leadIndex < index) start = leadIndex;
  }
  const leadId = items[start]?.paneId;
  let end = start;
  for (let i = start + 1; i < items.length; i += 1) {
    const candidate = items[i];
    if (!isCompanionKind(candidate.kind) || candidate.sourcePaneId !== leadId) break;
    end = i;
  }
  return { start, end };
}

// Groups a (snapped) layout into [lead + its companions] blocks for
// unit-wise movement. A companion whose source is absent forms its own
// block — defensive only, resnap drops those before they persist.
function groupPaneBlocks(items: PaneLayoutItem[]): PaneLayoutItem[][] {
  const blocks: PaneLayoutItem[][] = [];
  const blockByLead = new Map<string, PaneLayoutItem[]>();
  for (const item of items) {
    if (isCompanionKind(item.kind) && item.sourcePaneId) {
      const block = blockByLead.get(item.sourcePaneId);
      if (block) {
        block.push(item);
        continue;
      }
    }
    const block = [item];
    blocks.push(block);
    blockByLead.set(item.paneId, block);
  }
  return blocks;
}

function resnapCompanionItems(items: PaneLayoutItem[]): PaneLayoutItem[] {
  const companionsBySource = new Map<string, PaneLayoutItem[]>();
  const threadItems: PaneLayoutItem[] = [];
  for (const item of items) {
    if (isCompanionKind(item.kind) && item.sourcePaneId) {
      const companions = companionsBySource.get(item.sourcePaneId) ?? [];
      companions.push(item);
      companionsBySource.set(item.sourcePaneId, companions);
    } else {
      threadItems.push(item);
    }
  }
  const result: PaneLayoutItem[] = [];
  for (const item of threadItems) {
    result.push(item);
    const companions = companionsBySource.get(item.paneId);
    if (companions) result.push(...companions);
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

// Verbatim setter, deliberately NOT resnapped: persistence restore
// rebuilds source-then-companions order itself, and PaneHost's broken
// companion fallback needs orphan states to be representable.
export function setPaneLayoutItems(items: PaneLayoutItem[]): void {
  layoutItems = items.map(cloneLayoutItem);
}

export function setPaneLayoutItemsForTest(items: PaneLayoutItem[]): void {
  setPaneLayoutItems(items);
}
