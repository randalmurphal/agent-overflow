import type { Item } from '../types/models';
import type { ItemPatchEvent } from '../types/events';
import {
  isItemActive,
  isSubagentActivityPreviewKind,
  normalizePreviewText,
  subagentActivityPreview,
} from '../utils/subagentGrouping';
import { isPotentialSubagentLaunch } from '../utils/subagentLaunch';
import {
  createSubagentFoldRegistry,
  type SubagentFoldAggregate,
  type SubagentFoldSnapshot,
} from '../utils/subagentFold';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

export interface ThreadSubagentMemoryOptions {
  getThreadId(): string | null;
  /** The loaded window row with this id, read without tracking. */
  getLoadedItem(itemId: string): Item | undefined;
  /**
   * A loaded `Skill` row admitted its first child. Forked-skill detection
   * reads the anchor's aggregate, so the row now groups as a card.
   */
  noteStructureChanged(): void;
}

export interface ThreadSubagentMemory {
  /**
   * Admit streamed subagent rows (`parentId` set). They never enter the
   * pane window; their launch anchors' aggregates record them instead.
   */
  admitChildren(items: readonly Item[]): void;
  /** Apply a field patch to a tracked child. False when the id is not one. */
  applyChildPatch(evt: ItemPatchEvent): boolean;
  /** True when the id is a tracked child, whose deltas have no window row. */
  isKnownChild(itemId: string): boolean;
  /**
   * Live aggregate for a launch anchor, or undefined when it has none.
   * Reactive per anchor: a reader wakes only when this anchor changes.
   */
  aggregate(anchorId: string): SubagentFoldAggregate | undefined;
  /** Drop aggregates whose root row has left the loaded window. */
  retainFoldAnchors(): void;
  /** Plain-data copy of the aggregates for the thread-switch snapshot cache. */
  snapshotFolds(): SubagentFoldSnapshot | null;
  /** Replace the aggregates from a cached snapshot (thread re-entry). */
  restoreFolds(snapshot: SubagentFoldSnapshot | null | undefined): void;
  clearFolds(): void;
  /** Retained sizes, for the memory-bound tests. */
  foldStats(): { children: number; anchors: number; activeEntries: number };
}

export function createThreadSubagentMemory(
  options: ThreadSubagentMemoryOptions,
): ThreadSubagentMemory {
  const folds = createSubagentFoldRegistry();
  // One box per anchor with an aggregate. Boxes are created by the
  // writers below, which run from event handlers and loads, never from
  // the reactions that read them.
  const signals = createKeyedSignalRegistry<SubagentFoldAggregate | undefined>(undefined);
  const published = new Set<string>();

  const loadedRoot = (id: string): boolean | undefined => {
    const item = options.getLoadedItem(id);
    return item === undefined ? undefined : isPotentialSubagentLaunch(item);
  };

  /** Publish every changed anchor once per mutation batch. */
  function publish(): void {
    let forkedSkill = false;
    folds.drainChanged((anchorId, created) => {
      const aggregate = folds.aggregate(anchorId);
      if (aggregate) {
        signals.set(anchorId, aggregate);
        published.add(anchorId);
      } else if (published.delete(anchorId)) {
        signals.drop(anchorId);
      }
      if (created && !forkedSkill && options.getLoadedItem(anchorId)?.toolName === 'Skill') {
        forkedSkill = true;
      }
    });
    if (forkedSkill) options.noteStructureChanged();
  }

  function admitChildren(items: readonly Item[]): void {
    const threadId = options.getThreadId();
    if (threadId === null) return;
    for (const item of items) {
      const parentId = item.parentId ?? '';
      if (!parentId || item.threadId !== threadId) continue;
      folds.admit({
        id: item.id,
        parentId,
        turnIndex: item.turnIndex,
        itemIndex: item.itemIndex,
        launch: isPotentialSubagentLaunch(item),
        active: isItemActive(item),
        preview: subagentActivityPreview(item),
        updatedAt: item.updatedAt,
      }, loadedRoot);
    }
    publish();
  }

  function applyChildPatch(evt: ItemPatchEvent): boolean {
    const { status, summary, updatedAt } = evt.patch;
    const active = status === undefined ? undefined : status === 'running' || status === 'streaming';
    const preview = summary === undefined
      ? undefined
      : isSubagentActivityPreviewKind(evt.kind) ? normalizePreviewText(summary) : '';
    if (!folds.update(evt.itemId, active, preview, updatedAt)) return false;
    publish();
    return true;
  }

  function clearFolds(): void {
    folds.clear();
    publish();
  }

  return {
    admitChildren,
    applyChildPatch,
    isKnownChild: (itemId) => folds.isKnown(itemId),
    aggregate: (anchorId) => signals.get(anchorId),
    retainFoldAnchors(): void {
      folds.retainRoots((rootId) => options.getLoadedItem(rootId) !== undefined);
      publish();
    },
    snapshotFolds: () => folds.snapshot(),
    restoreFolds(snapshot): void {
      folds.restore(snapshot);
      publish();
    },
    clearFolds,
    foldStats: () => folds.stats(),
  };
}
