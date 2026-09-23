import { describe, expect, it } from 'vitest';
import {
  createSubagentFoldRegistry,
  MAX_ACTIVE_PER_ANCHOR,
  MAX_TRACKED_CHILDREN,
  type LoadedRootLookup,
  type SubagentChildRow,
} from './subagentFold';

function row(overrides: Partial<SubagentChildRow> & { id: string }): SubagentChildRow {
  return {
    parentId: 'anchor',
    turnIndex: 1,
    itemIndex: 0,
    launch: false,
    active: false,
    preview: '',
    updatedAt: 0,
    ...overrides,
  };
}

/** Loaded roots: `anchor`, `anchor2` and `skill` anchor cards; `plain` does not. */
const roots: LoadedRootLookup = (id) => {
  if (id === 'anchor' || id === 'anchor2' || id === 'skill') return true;
  if (id === 'plain') return false;
  return undefined;
};

function drained(folds: ReturnType<typeof createSubagentFoldRegistry>): Array<[string, boolean]> {
  const out: Array<[string, boolean]> = [];
  folds.drainChanged((anchorId, created) => out.push([anchorId, created]));
  return out;
}

describe('createSubagentFoldRegistry', () => {
  it('counts distinct children and reports the newest settled preview', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'c1', itemIndex: 1, preview: 'ran build' }), roots);
    folds.admit(row({ id: 'c2', itemIndex: 2, preview: 'ran tests' }), roots);

    expect(folds.aggregate('anchor')).toEqual({
      count: 2,
      activePreview: '',
      activeTurnIndex: -1,
      activeItemIndex: -1,
      terminalPreview: 'ran tests',
      terminalTurnIndex: 1,
      terminalItemIndex: 2,
    });
    expect(folds.isKnown('c1')).toBe(true);
    expect(folds.isKnown('other')).toBe(false);
    expect(folds.aggregate('unknown')).toBeUndefined();
  });

  it('does not count a replayed id twice', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'c1', itemIndex: 1, preview: 'first' }), roots);
    folds.admit(row({ id: 'c1', itemIndex: 1, preview: 'first' }), roots);
    expect(folds.aggregate('anchor')?.count).toBe(1);
  });

  it('memoizes the aggregate until the anchor changes', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'c1', itemIndex: 1, preview: 'one' }), roots);
    const first = folds.aggregate('anchor');
    expect(folds.aggregate('anchor')).toBe(first);
    folds.admit(row({ id: 'c2', itemIndex: 2, preview: 'two' }), roots);
    expect(folds.aggregate('anchor')).not.toBe(first);
  });

  it('keeps the highest-position non-empty terminal preview', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'c2', itemIndex: 5, preview: 'later' }), roots);
    folds.admit(row({ id: 'c1', itemIndex: 2, preview: 'earlier' }), roots);
    folds.admit(row({ id: 'c3', turnIndex: 2, itemIndex: 0, preview: '' }), roots);
    expect(folds.aggregate('anchor')).toMatchObject({ count: 3, terminalPreview: 'later', terminalItemIndex: 5 });

    // The same row restated with newer text takes it.
    folds.update('c2', false, 'later, restated', 10);
    expect(folds.aggregate('anchor')?.terminalPreview).toBe('later, restated');
  });

  it('tracks the newest active preview and clears it when the row settles', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'c1', itemIndex: 1, active: true, preview: 'building', updatedAt: 1 }), roots);
    folds.admit(row({ id: 'c2', itemIndex: 2, active: true, preview: 'testing', updatedAt: 1 }), roots);
    expect(folds.aggregate('anchor')).toMatchObject({ activePreview: 'testing', activeItemIndex: 2 });

    expect(folds.update('c2', false, undefined, 5)).toBe(true);
    expect(folds.aggregate('anchor')).toMatchObject({
      activePreview: 'building',
      activeItemIndex: 1,
      terminalPreview: 'testing',
      terminalItemIndex: 2,
    });
    expect(folds.stats().activeEntries).toBe(1);
  });

  it('ignores an older live observation of a settled row', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'c1', itemIndex: 1, active: false, preview: 'done', updatedAt: 10 }), roots);
    folds.admit(row({ id: 'c1', itemIndex: 1, active: true, preview: 'running', updatedAt: 9 }), roots);
    expect(folds.aggregate('anchor')).toMatchObject({ activePreview: '', terminalPreview: 'done' });

    // A newer live observation reopens it (resumed row).
    folds.admit(row({ id: 'c1', itemIndex: 1, active: true, preview: 'resumed', updatedAt: 11 }), roots);
    expect(folds.aggregate('anchor')?.activePreview).toBe('resumed');
  });

  it('update keeps status when a patch names only the preview', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'c1', itemIndex: 1, active: true, preview: 'one', updatedAt: 1 }), roots);
    expect(folds.update('c1', undefined, 'two', 2)).toBe(true);
    expect(folds.aggregate('anchor')?.activePreview).toBe('two');
    expect(folds.update('c1', undefined, undefined, 3)).toBe(true);
    expect(folds.update('missing', false, 'x', 3)).toBe(false);
  });

  it('counts nested children on every enclosing launch anchor', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'inner', itemIndex: 1, launch: true, active: true, preview: 'inner agent', updatedAt: 1 }), roots);
    folds.admit(row({ id: 'g1', parentId: 'inner', itemIndex: 2, preview: 'grandchild' }), roots);

    expect(folds.aggregate('anchor')).toMatchObject({ count: 2, terminalPreview: 'grandchild' });
    expect(folds.aggregate('inner')).toMatchObject({ count: 1, terminalPreview: 'grandchild' });
  });

  it('keeps a nested launch in the ledger while its children arrive', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'inner', itemIndex: 0, launch: true, active: true, updatedAt: 1 }), roots);
    for (let i = 1; i <= MAX_TRACKED_CHILDREN + 10; i += 1) {
      folds.admit(row({ id: `g${i}`, parentId: 'inner', itemIndex: i }), roots);
    }
    expect(folds.isKnown('inner')).toBe(true);
    expect(folds.aggregate('inner')?.count).toBe(MAX_TRACKED_CHILDREN + 10);
  });

  it('records children of a non-launch root for dedupe without an aggregate', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'c1', parentId: 'plain', itemIndex: 1, preview: 'x' }), roots);
    expect(folds.isKnown('c1')).toBe(true);
    expect(folds.aggregate('plain')).toBeUndefined();
    expect(drained(folds)).toEqual([]);
  });

  it('drains each changed anchor once with whether it was created', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'c1', itemIndex: 1, preview: 'one' }), roots);
    folds.admit(row({ id: 'c2', itemIndex: 2, preview: 'two' }), roots);
    expect(drained(folds)).toEqual([['anchor', true]]);
    expect(drained(folds)).toEqual([]);

    folds.admit(row({ id: 'c3', itemIndex: 3, preview: 'three' }), roots);
    expect(drained(folds)).toEqual([['anchor', false]]);

    // An unchanged restatement changes nothing.
    folds.admit(row({ id: 'c3', itemIndex: 3, preview: 'three' }), roots);
    expect(drained(folds)).toEqual([]);
  });

  it('bounds the ledger and does not recount an evicted id replayed at or below the floor', () => {
    const folds = createSubagentFoldRegistry();
    const total = MAX_TRACKED_CHILDREN + 100;
    for (let i = 0; i < total; i += 1) {
      folds.admit(row({ id: `c${i}`, itemIndex: i }), roots);
    }
    expect(folds.stats().children).toBe(MAX_TRACKED_CHILDREN);
    expect(folds.isKnown('c0')).toBe(false);
    expect(folds.aggregate('anchor')?.count).toBe(total);

    folds.admit(row({ id: 'c0', itemIndex: 0 }), roots);
    expect(folds.aggregate('anchor')?.count).toBe(total);

    folds.admit(row({ id: 'fresh', itemIndex: total }), roots);
    expect(folds.aggregate('anchor')?.count).toBe(total + 1);
  });

  it('drops an evicted child active preview with its ledger slot', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'stuck', parentId: 'anchor2', itemIndex: 0, active: true, preview: 'running', updatedAt: 1 }), roots);
    for (let i = 1; i <= MAX_TRACKED_CHILDREN; i += 1) {
      folds.admit(row({ id: `c${i}`, itemIndex: i }), roots);
    }
    expect(folds.isKnown('stuck')).toBe(false);
    expect(folds.aggregate('anchor2')).toMatchObject({ count: 1, activePreview: '' });
  });

  it('bounds active previews per anchor', () => {
    const folds = createSubagentFoldRegistry();
    for (let i = 0; i < MAX_ACTIVE_PER_ANCHOR + 5; i += 1) {
      folds.admit(row({ id: `c${i}`, itemIndex: i, active: true, preview: `p${i}`, updatedAt: 1 }), roots);
    }
    expect(folds.stats().activeEntries).toBe(MAX_ACTIVE_PER_ANCHOR);
    expect(folds.aggregate('anchor')?.activePreview).toBe(`p${MAX_ACTIVE_PER_ANCHOR + 4}`);
  });

  it('retainRoots drops records, ledger entries and floors of unloaded roots', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'a1', parentId: 'anchor', itemIndex: 1 }), roots);
    folds.admit(row({ id: 'b1', parentId: 'anchor2', itemIndex: 2 }), roots);
    drained(folds);

    folds.retainRoots((rootId) => rootId === 'anchor2');

    expect(folds.aggregate('anchor')).toBeUndefined();
    expect(folds.isKnown('a1')).toBe(false);
    expect(folds.aggregate('anchor2')?.count).toBe(1);
    expect(drained(folds)).toEqual([['anchor', false]]);
    expect(folds.snapshot()?.roots.map((r) => r.rootId)).toEqual(['anchor2']);
  });

  it('clear empties everything and reports the dropped anchors', () => {
    const folds = createSubagentFoldRegistry();
    folds.admit(row({ id: 'c1' }), roots);
    drained(folds);
    folds.clear();
    expect(folds.aggregate('anchor')).toBeUndefined();
    expect(folds.isKnown('c1')).toBe(false);
    expect(folds.snapshot()).toBeNull();
    expect(drained(folds)).toEqual([['anchor', false]]);
  });

  it('round-trips counts, terminal previews and floors, but not active previews', () => {
    const folds = createSubagentFoldRegistry();
    expect(folds.snapshot()).toBeNull();
    folds.admit(row({ id: 'c1', turnIndex: 3, itemIndex: 2, preview: 'preview a' }), roots);
    folds.admit(row({ id: 'c2', turnIndex: 3, itemIndex: 4, active: true, preview: 'live', updatedAt: 1 }), roots);

    const snapshot = folds.snapshot();
    expect(snapshot?.roots).toEqual([{ rootId: 'anchor', floorTurnIndex: 3, floorItemIndex: 4 }]);

    const restored = createSubagentFoldRegistry();
    restored.admit(row({ id: 'old', parentId: 'anchor2' }), roots);
    drained(restored);
    restored.restore(snapshot);

    expect(restored.aggregate('anchor2')).toBeUndefined();
    expect(restored.isKnown('old')).toBe(false);
    expect(restored.aggregate('anchor')).toEqual({
      count: 2,
      activePreview: '',
      activeTurnIndex: -1,
      activeItemIndex: -1,
      terminalPreview: 'preview a',
      terminalTurnIndex: 3,
      terminalItemIndex: 2,
    });
    expect(drained(restored)).toEqual([['anchor2', false], ['anchor', true]]);

    // Replay of a carried child is not recounted; a new one is.
    restored.admit(row({ id: 'c2', turnIndex: 3, itemIndex: 4, preview: 'settled' }), roots);
    expect(restored.aggregate('anchor')).toMatchObject({ count: 2, terminalPreview: 'settled' });
    restored.admit(row({ id: 'c3', turnIndex: 3, itemIndex: 5 }), roots);
    expect(restored.aggregate('anchor')?.count).toBe(3);

    restored.restore(null);
    expect(restored.aggregate('anchor')).toBeUndefined();
  });
});
