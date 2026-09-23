// Per-launch-anchor live aggregates for subagent rows.
//
// Child rows never enter a pane's item window. The pane admits each one
// here instead: the registry keeps what a collapsed card needs (entry
// count, newest active and terminal previews) per launch anchor, plus a
// bounded ledger of recent child ids for replay dedupe, delta swallowing
// and nested-anchor resolution. Expanded cards and the agent pane load
// the rows themselves through scoped surfaces.
//
// Memory is bounded independently of how many rows stream: the ledger
// holds at most MAX_TRACKED_CHILDREN ids, each anchor at most
// MAX_ACTIVE_PER_ANCHOR active previews, and an anchor record is dropped
// with its loaded root row.

/** Live aggregate exposed to cards for one launch anchor. */
export interface SubagentFoldAggregate {
  /** Distinct descendants admitted while the anchor's root was loaded. */
  count: number;
  /** Normalized preview of the newest active descendant, '' when none. */
  activePreview: string;
  activeTurnIndex: number;
  activeItemIndex: number;
  /** Normalized preview of the newest settled descendant with text. */
  terminalPreview: string;
  /** Timeline position of `terminalPreview`'s source row. */
  terminalTurnIndex: number;
  terminalItemIndex: number;
}

/** Cache-snapshot shape: plain data so thread switch can carry folds. */
export interface SubagentFoldSnapshot {
  anchors: Array<{
    anchorId: string;
    rootId: string;
    count: number;
    terminalPreview: string;
    terminalTurnIndex: number;
    terminalItemIndex: number;
  }>;
  /**
   * Per loaded root, the newest child position already counted. A
   * re-delivered child at or below it is not counted again once the
   * ledger no longer holds its id.
   */
  roots: Array<{ rootId: string; floorTurnIndex: number; floorItemIndex: number }>;
}

/** What the pane knows about one admitted child row. */
export interface SubagentChildRow {
  id: string;
  parentId: string;
  turnIndex: number;
  itemIndex: number;
  /** The row can anchor its own card (`isPotentialSubagentLaunch`). */
  launch: boolean;
  /** Running or streaming. */
  active: boolean;
  /** Normalized preview text; '' when the row contributes none. */
  preview: string;
  updatedAt: number;
}

/**
 * Whether `id` is a loaded root row: undefined when it is not loaded,
 * otherwise whether it anchors a card.
 */
export type LoadedRootLookup = (id: string) => boolean | undefined;

export interface SubagentFoldRegistry {
  /** Admit a new child or restate a known one. */
  admit(row: SubagentChildRow, loadedRoot: LoadedRootLookup): void;
  /**
   * Restate a known child. `active` and `preview` undefined keep what the
   * row already has. Returns false when the id is not tracked.
   */
  update(id: string, active: boolean | undefined, preview: string | undefined, updatedAt: number | undefined): boolean;
  /** True when the id is a tracked child. Refreshes its ledger slot. */
  isKnown(id: string): boolean;
  /** Aggregate for one anchor, or undefined when it has none. */
  aggregate(anchorId: string): SubagentFoldAggregate | undefined;
  /** Drop every record whose root row no longer satisfies `keep`. */
  retainRoots(keep: (rootId: string) => boolean): void;
  /**
   * Hand every anchor changed since the last drain to `visit`, with
   * whether this change created its record.
   */
  drainChanged(visit: (anchorId: string, created: boolean) => void): void;
  clear(): void;
  snapshot(): SubagentFoldSnapshot | null;
  restore(snapshot: SubagentFoldSnapshot | null | undefined): void;
  /** Retained sizes, for the memory-bound tests. */
  stats(): { children: number; anchors: number; activeEntries: number };
}

/** Ledger capacity; beyond it the least recently touched id is forgotten. */
export const MAX_TRACKED_CHILDREN = 4096;
/** Active previews kept per anchor; beyond it the oldest is dropped. */
export const MAX_ACTIVE_PER_ANCHOR = 16;

interface Position {
  turnIndex: number;
  itemIndex: number;
}

interface PreviewPoint extends Position {
  preview: string;
}

interface AnchorRecord {
  rootId: string;
  count: number;
  active: Map<string, PreviewPoint>;
  terminal: PreviewPoint | null;
  /**
   * Memoized `aggregate()` result, cleared by every mutation, so a card
   * reading an unchanged anchor gets the same reference.
   */
  built: SubagentFoldAggregate | null;
}

interface ChildEntry extends Position {
  rootId: string;
  /** Launch anchors whose card this row belongs to, outermost first. */
  anchors: readonly string[];
  /** `anchors` for this row's own children. */
  descendantAnchors: readonly string[];
  /** `updatedAt` of the observation that settled the row; -1 while active. */
  settledAt: number;
}

const NO_ANCHORS: readonly string[] = Object.freeze([]);

function comparePositions(a: Position, b: Position): number {
  return a.turnIndex !== b.turnIndex ? a.turnIndex - b.turnIndex : a.itemIndex - b.itemIndex;
}

export function createSubagentFoldRegistry(): SubagentFoldRegistry {
  const records = new Map<string, AnchorRecord>();
  // Insertion-ordered: the first entry is the least recently touched.
  const children = new Map<string, ChildEntry>();
  const childCountByRoot = new Map<string, number>();
  const floors = new Map<string, Position>();
  const rootChains = new Map<string, readonly string[]>();
  const changed = new Map<string, boolean>();

  function markChanged(anchorId: string, created = false): void {
    changed.set(anchorId, created || changed.get(anchorId) === true);
  }

  function recordFor(anchorId: string, rootId: string): AnchorRecord {
    let record = records.get(anchorId);
    if (!record) {
      record = { rootId, count: 0, active: new Map(), terminal: null, built: null };
      records.set(anchorId, record);
      markChanged(anchorId, true);
    }
    return record;
  }

  function rootChain(rootId: string): readonly string[] {
    let chain = rootChains.get(rootId);
    if (!chain) {
      chain = Object.freeze([rootId]);
      rootChains.set(rootId, chain);
    }
    return chain;
  }

  function raiseFloor(rootId: string, position: Position): void {
    const floor = floors.get(rootId);
    if (!floor || comparePositions(position, floor) > 0) {
      floors.set(rootId, { turnIndex: position.turnIndex, itemIndex: position.itemIndex });
    }
  }

  function touch(id: string, entry: ChildEntry): void {
    children.delete(id);
    children.set(id, entry);
  }

  function evictOldestChild(): void {
    const oldest = children.entries().next();
    if (oldest.done) return;
    const [id, entry] = oldest.value;
    children.delete(id);
    if (!entry.rootId) return;
    // The id stays counted: a later delivery at or below the floor is a
    // replay, not a new row.
    raiseFloor(entry.rootId, entry);
    const remaining = (childCountByRoot.get(entry.rootId) ?? 1) - 1;
    if (remaining > 0) childCountByRoot.set(entry.rootId, remaining);
    else childCountByRoot.delete(entry.rootId);
    // Its settle event would no longer find it, so an active preview it
    // holds must not outlive the entry.
    for (const anchorId of entry.anchors) {
      const record = records.get(anchorId);
      if (record?.active.delete(id)) {
        record.built = null;
        markChanged(anchorId);
      }
    }
  }

  function applyPreview(
    anchorId: string,
    record: AnchorRecord,
    id: string,
    position: Position,
    active: boolean,
    preview: string | undefined,
  ): void {
    const held = record.active.get(id);
    const text = preview ?? held?.preview ?? '';
    if (active) {
      if (text) {
        if (held?.preview === text) return;
        if (!held && record.active.size >= MAX_ACTIVE_PER_ANCHOR) {
          const first = record.active.keys().next();
          if (!first.done) record.active.delete(first.value);
        }
        record.active.set(id, { preview: text, turnIndex: position.turnIndex, itemIndex: position.itemIndex });
      } else if (!record.active.delete(id)) {
        return;
      }
      record.built = null;
      markChanged(anchorId);
      return;
    }
    let moved = record.active.delete(id);
    const terminal = record.terminal;
    const order = terminal ? comparePositions(position, terminal) : 1;
    if (text && order >= 0 && (order > 0 || terminal?.preview !== text)) {
      record.terminal = { preview: text, turnIndex: position.turnIndex, itemIndex: position.itemIndex };
      moved = true;
    }
    if (!moved) return;
    record.built = null;
    markChanged(anchorId);
  }

  /**
   * Record the observation's status on the entry. False when it is an
   * older live observation of a row already settled (the window merge's
   * `isItemStatusRegression` rule), which must not reopen its preview.
   */
  function settle(entry: ChildEntry, active: boolean, updatedAt: number | undefined): boolean {
    if (active) {
      if (entry.settledAt >= 0 && (updatedAt ?? entry.settledAt) <= entry.settledAt) return false;
      entry.settledAt = -1;
      return true;
    }
    entry.settledAt = Math.max(entry.settledAt, updatedAt ?? 0);
    return true;
  }

  function restate(id: string, entry: ChildEntry, active: boolean, preview: string | undefined, updatedAt: number | undefined): void {
    if (!settle(entry, active, updatedAt)) return;
    for (const anchorId of entry.anchors) {
      applyPreview(anchorId, recordFor(anchorId, entry.rootId), id, entry, active, preview);
    }
  }

  function admit(row: SubagentChildRow, loadedRoot: LoadedRootLookup): void {
    let entry = children.get(row.id);
    if (entry) {
      touch(row.id, entry);
    } else {
      let rootId = '';
      let anchors = NO_ANCHORS;
      const parentIsLaunch = row.parentId ? loadedRoot(row.parentId) : undefined;
      if (parentIsLaunch !== undefined) {
        rootId = row.parentId;
        if (parentIsLaunch) anchors = rootChain(rootId);
      } else {
        const parent = row.parentId ? children.get(row.parentId) : undefined;
        if (parent?.rootId) {
          // A nested launch stays in the ledger while its children arrive.
          touch(row.parentId, parent);
          rootId = parent.rootId;
          anchors = parent.descendantAnchors;
        }
      }
      entry = {
        rootId,
        anchors,
        descendantAnchors: rootId && row.launch ? Object.freeze([...anchors, row.id]) : anchors,
        turnIndex: row.turnIndex,
        itemIndex: row.itemIndex,
        settledAt: -1,
      };
      children.set(row.id, entry);
      if (rootId) {
        childCountByRoot.set(rootId, (childCountByRoot.get(rootId) ?? 0) + 1);
        const floor = floors.get(rootId);
        if (!floor || comparePositions(entry, floor) > 0) {
          for (const anchorId of anchors) {
            const record = recordFor(anchorId, rootId);
            record.count += 1;
            record.built = null;
            markChanged(anchorId);
          }
        }
      }
      if (children.size > MAX_TRACKED_CHILDREN) evictOldestChild();
    }
    restate(row.id, entry, row.active, row.preview, row.updatedAt);
  }

  function buildAggregate(record: AnchorRecord): SubagentFoldAggregate {
    let newest: PreviewPoint | null = null;
    for (const point of record.active.values()) {
      if (!newest || comparePositions(point, newest) > 0) newest = point;
    }
    return {
      count: record.count,
      activePreview: newest?.preview ?? '',
      activeTurnIndex: newest?.turnIndex ?? -1,
      activeItemIndex: newest?.itemIndex ?? -1,
      terminalPreview: record.terminal?.preview ?? '',
      terminalTurnIndex: record.terminal?.turnIndex ?? -1,
      terminalItemIndex: record.terminal?.itemIndex ?? -1,
    };
  }

  function clearAll(): void {
    for (const anchorId of records.keys()) markChanged(anchorId);
    records.clear();
    children.clear();
    childCountByRoot.clear();
    floors.clear();
    rootChains.clear();
  }

  return {
    admit,

    update(id, active, preview, updatedAt) {
      const entry = children.get(id);
      if (!entry) return false;
      touch(id, entry);
      if (active === undefined) {
        if (preview === undefined) return true;
        active = entry.settledAt < 0;
        updatedAt = active ? updatedAt : entry.settledAt;
      }
      restate(id, entry, active, preview, updatedAt);
      return true;
    },

    isKnown(id) {
      const entry = children.get(id);
      if (!entry) return false;
      touch(id, entry);
      return true;
    },

    aggregate(anchorId) {
      const record = records.get(anchorId);
      if (!record) return undefined;
      record.built ??= buildAggregate(record);
      return record.built;
    },

    retainRoots(keep) {
      const dropped = new Set<string>();
      for (const rootId of childCountByRoot.keys()) if (!keep(rootId)) dropped.add(rootId);
      for (const rootId of floors.keys()) if (!keep(rootId)) dropped.add(rootId);
      for (const [anchorId, record] of records) {
        if (dropped.has(record.rootId) || !keep(record.rootId)) {
          dropped.add(record.rootId);
          records.delete(anchorId);
          markChanged(anchorId);
        }
      }
      if (dropped.size === 0) return;
      for (const [id, entry] of children) {
        if (dropped.has(entry.rootId)) children.delete(id);
      }
      for (const rootId of dropped) {
        childCountByRoot.delete(rootId);
        floors.delete(rootId);
        rootChains.delete(rootId);
      }
    },

    drainChanged(visit) {
      if (changed.size === 0) return;
      const drained = [...changed];
      changed.clear();
      for (const [anchorId, created] of drained) visit(anchorId, created);
    },

    clear: clearAll,

    snapshot() {
      const roots = new Map<string, Position>(floors);
      for (const entry of children.values()) {
        if (!entry.rootId) continue;
        const floor = roots.get(entry.rootId);
        if (!floor || comparePositions(entry, floor) > 0) roots.set(entry.rootId, entry);
      }
      if (records.size === 0 && roots.size === 0) return null;
      const anchors: SubagentFoldSnapshot['anchors'] = [];
      for (const [anchorId, record] of records) {
        anchors.push({
          anchorId,
          rootId: record.rootId,
          count: record.count,
          terminalPreview: record.terminal?.preview ?? '',
          terminalTurnIndex: record.terminal?.turnIndex ?? -1,
          terminalItemIndex: record.terminal?.itemIndex ?? -1,
        });
      }
      return {
        anchors,
        roots: [...roots].map(([rootId, floor]) => ({
          rootId,
          floorTurnIndex: floor.turnIndex,
          floorItemIndex: floor.itemIndex,
        })),
      };
    },

    restore(snapshot) {
      clearAll();
      if (!snapshot) return;
      for (const entry of snapshot.anchors) {
        records.set(entry.anchorId, {
          rootId: entry.rootId,
          count: entry.count,
          // Active rows are not carried: their settle events can be missed
          // while the thread is away, and a stale running preview would
          // never clear.
          active: new Map(),
          terminal: entry.terminalPreview
            ? { preview: entry.terminalPreview, turnIndex: entry.terminalTurnIndex, itemIndex: entry.terminalItemIndex }
            : null,
          built: null,
        });
        markChanged(entry.anchorId, true);
      }
      for (const root of snapshot.roots) {
        floors.set(root.rootId, { turnIndex: root.floorTurnIndex, itemIndex: root.floorItemIndex });
      }
    },

    stats() {
      let activeEntries = 0;
      for (const record of records.values()) activeEntries += record.active.size;
      return { children: children.size, anchors: records.size, activeEntries };
    },
  };
}
