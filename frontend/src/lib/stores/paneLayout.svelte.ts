import { untrack } from 'svelte';
import {
  FALLBACK_PANE_WIDTH_PX,
  OVERFLOW_EPSILON_PX,
  minAnchorPaneWidths,
  normalizePaneWidthPx,
  resolvePaneBoundaryDrag,
  type PaneBoundaryDrag,
} from '../utils/paneWidths';
import { getPaneHostWidth } from './layoutMetrics.svelte';
import type { AgentPaneScopeSnapshot } from '../types/settings';

// 'thread' panes host a ChatView for a ThreadPane (the registry in
// panes.svelte.ts). Companion panes are NOT ThreadPanes: they host surfaces
// paired to a source thread pane via `sourcePaneId`. take-control and browser
// are live/ephemeral; plan, review, and agent companions are
// persisted by layout persistence and restored through companionPanes.svelte.ts.
export type PaneLayoutKind = 'thread' | 'take-control' | 'browser' | 'plan' | 'review' | 'agent';
export type CompanionPaneKind = 'take-control' | 'browser' | 'plan' | 'review' | 'agent';

export function isCompanionKind(kind: PaneLayoutKind): kind is CompanionPaneKind {
  return kind === 'take-control' ||
    kind === 'browser' ||
    kind === 'plan' ||
    kind === 'review' ||
    kind === 'agent';
}

export interface PaneLayoutItem {
  id: string;
  paneId: string;
  kind: PaneLayoutKind;
  // Base width in px. PaneHost renders it as the flex basis: panes
  // stretch proportionally when the window is wider than the sum of
  // widths and horizontal-scroll when it is narrower. Resize semantics
  // live in utils/paneWidths.ts.
  widthPx: number;
  // Set only on companion items: the paneId of the source thread pane this
  // surface is paired to. Drives adjacency (companions sit immediately right
  // of their source) and cascade close (source closes → companions close).
  sourcePaneId?: string;
  // Set only on 'agent' companion items, and only by layout RESTORE: the
  // scope the pane was persisted at. It is a carrier, not the truth — the
  // live scope lives in agentPane.svelte.ts, and buildSnapshot reads this
  // only for a pane whose live state has not been created yet (restore ran,
  // the body has not mounted, a width drag already asked for a save). Live
  // state always wins.
  agentScope?: AgentPaneScopeSnapshot;
}

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
    widthPx: FALLBACK_PANE_WIDTH_PX,
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

/**
 * Ask for a layout save from a store that owns state the SNAPSHOT reads but
 * the layout items do not hold — currently the agent companion's scope
 * (`agentPane.svelte.ts`). Without it a scope change would sit unsaved until
 * some unrelated layout mutation happened to write, and a reload would
 * restore the pane to a scope the reader had already left.
 */
export function requestPaneLayoutPersistence(): void {
  requestLayoutPersistence();
}

export function flushPaneLayoutPersistence(): Promise<void> {
  return persistenceHandlers?.flush() ?? Promise.resolve();
}

function shouldPersist(options?: PaneLayoutMutationOptions): boolean {
  return options?.persist !== false;
}

function cloneLayoutItem(item: PaneLayoutItem): PaneLayoutItem {
  const cloned: PaneLayoutItem = {
    id: item.id,
    paneId: item.paneId,
    kind: item.kind,
    widthPx: normalizePaneWidthPx(item.widthPx),
  };
  if (item.sourcePaneId !== undefined) cloned.sourcePaneId = item.sourcePaneId;
  // Deep-copied: every clone hands out a layout item callers may hold, and a
  // shared breadcrumb array would let one item's trail mutate another's.
  if (item.agentScope !== undefined) {
    cloned.agentScope = {
      scopeItemId: item.agentScope.scopeItemId,
      breadcrumb: item.agentScope.breadcrumb.map((entry) => ({ ...entry })),
    };
  }
  return cloned;
}

export function getPaneLayoutItems(): PaneLayoutItem[] {
  return layoutItems;
}

// Membership + pairing identity of the layout, independent of widths.
// Divider drags reassign `layoutItems` every frame with only widthPx
// changed; a $derived whose recomputed value is unchanged stops
// propagation, so routing pairing lookups through this string key keeps
// width churn from re-running reactive consumers (e.g. ChatView's
// read-mark gates via getFocusedThreadPaneId).
const paneMembershipKey = $derived(
  layoutItems.map((item) => `${item.paneId}<${item.sourcePaneId ?? ''}`).join('\n'),
);

const sourcePaneIdByPaneId = $derived.by(() => {
  paneMembershipKey;
  return untrack(() => {
    const bySource = new Map<string, string | null>();
    for (const item of layoutItems) bySource.set(item.paneId, item.sourcePaneId ?? null);
    return bySource;
  });
});

/**
 * `sourcePaneId` of a mounted layout pane: the paired source thread pane
 * for companions, null for thread panes and unmounted ids. Reactive reads
 * track layout membership/pairing only — never pane widths.
 */
export function sourcePaneIdOf(paneId: string): string | null {
  return sourcePaneIdByPaneId.get(paneId) ?? null;
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

export interface PaneBoundaryDragArgs
  extends Omit<PaneBoundaryDrag, 'widths' | 'leftIndex' | 'hasRightPane'> {
  /** Pane immediately left of the grabbed boundary. */
  leftPaneId: string;
  /** Adjacent right pane, or null for the end handle at the strip's right edge. */
  rightPaneId: string | null;
  /** Measured pane widths at drag start, by paneId. */
  startWidths: ReadonlyMap<string, number>;
}

// Live-applied on every drag frame. Resolution is pure in deltaPx over
// the drag-start snapshot, so repeated calls retrace instead of
// accumulating. Semantics live in utils/paneWidths.ts.
export function applyPaneBoundaryDrag(args: PaneBoundaryDragArgs): void {
  const leftIndex = layoutItems.findIndex((item) => item.paneId === args.leftPaneId);
  if (leftIndex < 0) return;
  if (args.rightPaneId !== null) {
    if (layoutItems[leftIndex + 1]?.paneId !== args.rightPaneId) return;
  } else if (leftIndex !== layoutItems.length - 1) {
    return;
  }
  const widths = layoutItems.map(
    (item) => args.startWidths.get(item.paneId) ?? normalizePaneWidthPx(item.widthPx),
  );
  const resolved = resolvePaneBoundaryDrag({
    widths,
    leftIndex,
    hasRightPane: args.rightPaneId !== null,
    deltaPx: args.deltaPx,
    minPaneWidth: args.minPaneWidth,
    overflowPx: args.overflowPx,
    zeroSum: args.zeroSum,
  });
  if (!resolved) return;
  const next = resolved.map(normalizePaneWidthPx);
  if (layoutItems.every((item, index) => item.widthPx === next[index])) return;
  layoutItems = layoutItems.map((item, index) =>
    item.widthPx === next[index] ? item : { ...item, widthPx: next[index] },
  );
  requestLayoutPersistence(true);
}

/** Double-click reset: every pane back to the density minimum (equal fit). */
export function equalizePaneWidths(minPaneWidth: number): void {
  const width = normalizePaneWidthPx(minPaneWidth);
  if (layoutItems.every((item) => item.widthPx === width)) return;
  layoutItems = layoutItems.map((item) => ({ ...item, widthPx: width }));
  requestLayoutPersistence();
}

// Called at drag end. The fit gate lives HERE, on store data, because
// the caller's DOM may not have flushed the final drag frame yet — a
// stale scrollWidth read could rescale an overflowing layout (see
// minAnchorPaneWidths for why overflow widths must be left alone).
export function minAnchorPaneLayoutWidths(minPaneWidth: number): void {
  if (layoutItems.length === 0) return;
  // Unmeasured host: cannot know whether the strip overflows, so leave
  // the widths alone.
  const hostWidth = getPaneHostWidth();
  if (!Number.isFinite(hostWidth)) return;
  // Dividers are zero-width overlays (see PaneDivider), so the panes'
  // widths alone decide whether the strip fits the host.
  const total = layoutItems.reduce(
    (sum, item) => sum + normalizePaneWidthPx(item.widthPx),
    0,
  );
  if (total > hostWidth + OVERFLOW_EPSILON_PX) return;
  const anchored = minAnchorPaneWidths(
    layoutItems.map((item) => item.widthPx),
    minPaneWidth,
  );
  if (!anchored) return;
  layoutItems = layoutItems.map((item, index) => ({
    ...item,
    widthPx: normalizePaneWidthPx(anchored[index]),
  }));
  requestLayoutPersistence();
}

export function averagePaneWidthPx(): number {
  if (layoutItems.length === 0) return FALLBACK_PANE_WIDTH_PX;
  const total = layoutItems.reduce(
    (sum, item) => sum + normalizePaneWidthPx(item.widthPx),
    0,
  );
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
