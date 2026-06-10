import { describe, expect, it } from 'vitest';
import { createSubagentFoldRegistry } from './subagentFold';
import type { Item } from '../types/models';

function mkItem(overrides: Partial<Item> & { id: string }): Item {
  return {
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'completed',
    summary: '',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

describe('createSubagentFoldRegistry', () => {
  it('records evicted ids per anchor and reports the aggregate', () => {
    const folds = createSubagentFoldRegistry();

    expect(folds.recordEvicted('anchor', mkItem({ id: 'c1', itemIndex: 1 }), 'ran build')).toBe(true);
    expect(folds.recordEvicted('anchor', mkItem({ id: 'c2', itemIndex: 2 }), 'ran tests')).toBe(true);

    expect(folds.isEvicted('c1')).toBe(true);
    expect(folds.isEvicted('c2')).toBe(true);
    expect(folds.isEvicted('other')).toBe(false);
    expect(folds.aggregate('anchor')).toEqual({
      evictedCount: 2,
      terminalPreview: 'ran tests',
      terminalTurnIndex: 0,
      terminalItemIndex: 2,
    });
    expect(folds.aggregate('unknown')).toBeUndefined();
  });

  it('treats a replayed id as a no-op and reports it to the caller', () => {
    const folds = createSubagentFoldRegistry();
    folds.recordEvicted('anchor', mkItem({ id: 'c1', itemIndex: 1 }), 'first');

    // Replay under the same anchor — and under a different anchor (a
    // malformed echo must not double-count either way).
    expect(folds.recordEvicted('anchor', mkItem({ id: 'c1', itemIndex: 1 }), 'echo')).toBe(false);
    expect(folds.recordEvicted('other', mkItem({ id: 'c1', itemIndex: 1 }), 'echo')).toBe(false);

    expect(folds.aggregate('anchor')?.evictedCount).toBe(1);
    expect(folds.aggregate('anchor')?.terminalPreview).toBe('first');
    expect(folds.aggregate('other')).toBeUndefined();
  });

  it('keeps the highest-position non-empty preview', () => {
    const folds = createSubagentFoldRegistry();
    folds.recordEvicted('anchor', mkItem({ id: 'c2', turnIndex: 1, itemIndex: 5 }), 'later');
    // Earlier position must not overwrite.
    folds.recordEvicted('anchor', mkItem({ id: 'c1', turnIndex: 1, itemIndex: 2 }), 'earlier');
    // Later position with an empty preview must not blank it.
    folds.recordEvicted('anchor', mkItem({ id: 'c3', turnIndex: 2, itemIndex: 0 }), '');

    expect(folds.aggregate('anchor')).toEqual({
      evictedCount: 3,
      terminalPreview: 'later',
      terminalTurnIndex: 1,
      terminalItemIndex: 5,
    });

    // Same position (at-or-after) takes the fresher text — a re-settled
    // row at the same slot carries the newer summary.
    folds.recordEvicted('anchor2', mkItem({ id: 'd1', turnIndex: 1, itemIndex: 5 }), 'one');
    folds.recordEvicted('anchor2', mkItem({ id: 'd2', turnIndex: 1, itemIndex: 5 }), 'two');
    expect(folds.aggregate('anchor2')?.terminalPreview).toBe('two');
  });

  it('reclaim removes ids and drops the fold once empty', () => {
    const folds = createSubagentFoldRegistry();
    folds.recordEvicted('anchor', mkItem({ id: 'c1', itemIndex: 1 }), 'one');
    folds.recordEvicted('anchor', mkItem({ id: 'c2', itemIndex: 2 }), 'two');

    folds.reclaim(['c1', 'never-evicted']);
    expect(folds.isEvicted('c1')).toBe(false);
    // Partial reclaim keeps the count honest and the preview intact.
    expect(folds.aggregate('anchor')).toEqual({
      evictedCount: 1,
      terminalPreview: 'two',
      terminalTurnIndex: 0,
      terminalItemIndex: 2,
    });

    folds.reclaim(['c2']);
    expect(folds.aggregate('anchor')).toBeUndefined();
    expect(folds.isEvicted('c2')).toBe(false);

    // A reclaimed id can be folded again (collapse after re-hydration).
    expect(folds.recordEvicted('anchor', mkItem({ id: 'c1', itemIndex: 1 }), 'again')).toBe(true);
    expect(folds.aggregate('anchor')?.evictedCount).toBe(1);
  });

  it('dropAnchor forgets the fold and its id mappings', () => {
    const folds = createSubagentFoldRegistry();
    folds.recordEvicted('anchor', mkItem({ id: 'c1', itemIndex: 1 }), 'one');
    folds.recordEvicted('keep', mkItem({ id: 'k1', itemIndex: 2 }), 'kept');

    folds.dropAnchor('anchor');

    expect(folds.aggregate('anchor')).toBeUndefined();
    expect(folds.isEvicted('c1')).toBe(false);
    expect(folds.aggregate('keep')?.evictedCount).toBe(1);

    // Unknown anchor is a no-op.
    folds.dropAnchor('missing');
  });

  it('retainAnchors drops every fold the predicate rejects', () => {
    const folds = createSubagentFoldRegistry();
    folds.recordEvicted('a', mkItem({ id: 'c1', itemIndex: 1 }), '');
    folds.recordEvicted('b', mkItem({ id: 'c2', itemIndex: 2 }), '');
    folds.recordEvicted('c', mkItem({ id: 'c3', itemIndex: 3 }), '');

    folds.retainAnchors((anchorId) => anchorId === 'b');

    expect(folds.aggregate('a')).toBeUndefined();
    expect(folds.aggregate('c')).toBeUndefined();
    expect(folds.isEvicted('c1')).toBe(false);
    expect(folds.isEvicted('c3')).toBe(false);
    expect(folds.aggregate('b')?.evictedCount).toBe(1);
  });

  it('clear empties everything', () => {
    const folds = createSubagentFoldRegistry();
    folds.recordEvicted('a', mkItem({ id: 'c1' }), 'one');

    folds.clear();

    expect(folds.aggregate('a')).toBeUndefined();
    expect(folds.isEvicted('c1')).toBe(false);
    expect(folds.snapshot()).toBeNull();
  });

  it('round-trips through snapshot/restore', () => {
    const folds = createSubagentFoldRegistry();
    expect(folds.snapshot()).toBeNull();

    folds.recordEvicted('a', mkItem({ id: 'c1', turnIndex: 3, itemIndex: 2 }), 'preview a');
    folds.recordEvicted('b', mkItem({ id: 'c2', turnIndex: 4, itemIndex: 0 }), 'preview b');

    const snapshot = folds.snapshot();
    expect(snapshot).not.toBeNull();

    const restored = createSubagentFoldRegistry();
    // Restore replaces pre-existing content wholesale.
    restored.recordEvicted('stale', mkItem({ id: 'old' }), 'stale');
    restored.restore(snapshot);

    expect(restored.aggregate('stale')).toBeUndefined();
    expect(restored.isEvicted('old')).toBe(false);
    expect(restored.aggregate('a')).toEqual({
      evictedCount: 1,
      terminalPreview: 'preview a',
      terminalTurnIndex: 3,
      terminalItemIndex: 2,
    });
    expect(restored.aggregate('b')?.terminalPreview).toBe('preview b');
    expect(restored.isEvicted('c1')).toBe(true);

    // restore(null) clears (fresh-thread entry with no cached folds).
    restored.restore(null);
    expect(restored.aggregate('a')).toBeUndefined();
    expect(restored.isEvicted('c1')).toBe(false);
  });
});
