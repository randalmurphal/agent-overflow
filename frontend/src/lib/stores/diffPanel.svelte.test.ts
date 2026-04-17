import { describe, expect, it, beforeEach } from 'vitest';
import type { Checkpoint } from '../types/checkpoint';
import { createDiffPanelState, TURN_DIFF_CACHE_LIMIT } from './diffPanel.svelte';

function checkpoint(overrides: Partial<Checkpoint> = {}): Checkpoint {
  return {
    id: 'c-1',
    threadId: 't-1',
    turnIndex: 0,
    refName: 'refs/ao/t-1/0',
    capturedAt: 0,
    workspacePath: '/ws',
    ...overrides,
  };
}

describe('createDiffPanelState', () => {
  let store: ReturnType<typeof createDiffPanelState>;

  beforeEach(() => {
    store = createDiffPanelState();
  });

  it('starts closed with sensible defaults', () => {
    expect(store.open).toBe(false);
    expect(store.source).toBe('turn');
    expect(store.turnCompareMode).toBe('next');
    expect(store.viewMode).toBe('stacked');
    expect(store.selectedTurnIndex).toBeNull();
    expect(store.checkpoints).toEqual([]);
    expect(store.checkpointsLoaded).toBe(false);
    expect(store.checkpointsUnavailable).toBe(false);
    expect(store.error).toBeNull();
  });

  describe('open / close / toggle', () => {
    it('open_() sets open=true', () => {
      store.open_();
      expect(store.open).toBe(true);
    });

    it('close() returns open to false', () => {
      store.open_();
      store.close();
      expect(store.open).toBe(false);
    });

    it('toggle() flips open each call', () => {
      store.toggle();
      expect(store.open).toBe(true);
      store.toggle();
      expect(store.open).toBe(false);
    });
  });

  describe('source / compare / view mutations', () => {
    it('setSource() swaps the active source', () => {
      store.setSource('worktree');
      expect(store.source).toBe('worktree');
      store.setSource('cumulative');
      expect(store.source).toBe('cumulative');
    });

    it('setSource() clears the error banner', () => {
      store.setError('boom');
      store.setSource('worktree');
      expect(store.error).toBeNull();
    });

    it('setTurnCompareMode() toggles between next and worktree', () => {
      store.setTurnCompareMode('worktree');
      expect(store.turnCompareMode).toBe('worktree');
      store.setTurnCompareMode('next');
      expect(store.turnCompareMode).toBe('next');
    });

    it('setViewMode() toggles between stacked and split', () => {
      store.setViewMode('split');
      expect(store.viewMode).toBe('split');
    });

    it('selectTurn() stores the turn index or null', () => {
      store.selectTurn(3);
      expect(store.selectedTurnIndex).toBe(3);
      store.selectTurn(null);
      expect(store.selectedTurnIndex).toBeNull();
    });
  });

  describe('setCheckpoints', () => {
    it('sorts by turnIndex regardless of input order', () => {
      store.setCheckpoints([
        checkpoint({ id: 'c3', turnIndex: 3 }),
        checkpoint({ id: 'c1', turnIndex: 1 }),
        checkpoint({ id: 'c2', turnIndex: 2 }),
      ]);
      expect(store.checkpoints.map((c) => c.id)).toEqual(['c1', 'c2', 'c3']);
      expect(store.checkpointsLoaded).toBe(true);
    });

    it('clears unavailable flag when a non-empty list arrives', () => {
      store.markCheckpointsUnavailable('not-a-git-repo');
      expect(store.checkpointsUnavailable).toBe(true);
      store.setCheckpoints([checkpoint({ turnIndex: 0 })]);
      expect(store.checkpointsUnavailable).toBe(false);
      expect(store.checkpointsUnavailableReason).toBeNull();
    });

    it('does not clear unavailable flag when an empty list arrives', () => {
      store.markCheckpointsUnavailable('not-a-git-repo');
      store.setCheckpoints([]);
      expect(store.checkpointsUnavailable).toBe(true);
      expect(store.checkpointsUnavailableReason).toBe('not-a-git-repo');
    });
  });

  describe('markCheckpointsUnavailable', () => {
    it('empties the checkpoint list and records the reason', () => {
      store.setCheckpoints([checkpoint({ turnIndex: 0 })]);
      store.markCheckpointsUnavailable('not-a-git-repo');
      expect(store.checkpointsUnavailable).toBe(true);
      expect(store.checkpointsUnavailableReason).toBe('not-a-git-repo');
      expect(store.checkpoints).toEqual([]);
      expect(store.checkpointsLoaded).toBe(true);
    });
  });

  describe('turn diff cache', () => {
    it('returns undefined when no entry is stored', () => {
      expect(store.readTurnDiff('t', 0, 'next')).toBeUndefined();
    });

    it('round-trips a stored diff', () => {
      store.writeTurnDiff('t', 0, 'next', 'DIFF-A');
      expect(store.readTurnDiff('t', 0, 'next')).toBe('DIFF-A');
    });

    it('separates by thread / turnIndex / mode', () => {
      store.writeTurnDiff('t1', 0, 'next', 'A');
      store.writeTurnDiff('t1', 0, 'worktree', 'B');
      store.writeTurnDiff('t2', 0, 'next', 'C');
      store.writeTurnDiff('t1', 1, 'next', 'D');

      expect(store.readTurnDiff('t1', 0, 'next')).toBe('A');
      expect(store.readTurnDiff('t1', 0, 'worktree')).toBe('B');
      expect(store.readTurnDiff('t2', 0, 'next')).toBe('C');
      expect(store.readTurnDiff('t1', 1, 'next')).toBe('D');
    });

    it('evicts the oldest entry once capacity is exceeded', () => {
      // Fill beyond capacity with distinct keys.
      for (let i = 0; i <= TURN_DIFF_CACHE_LIMIT; i += 1) {
        store.writeTurnDiff('t', i, 'next', `d-${i}`);
      }
      // First insertion (turnIndex=0) was evicted.
      expect(store.readTurnDiff('t', 0, 'next')).toBeUndefined();
      // Last insertion is still live.
      expect(store.readTurnDiff('t', TURN_DIFF_CACHE_LIMIT, 'next')).toBe(
        `d-${TURN_DIFF_CACHE_LIMIT}`,
      );
    });

    it('read promotes the entry to the tail so it outlives newer inserts', () => {
      // Fill to capacity.
      for (let i = 0; i < TURN_DIFF_CACHE_LIMIT; i += 1) {
        store.writeTurnDiff('t', i, 'next', `d-${i}`);
      }
      // Touch the oldest.
      expect(store.readTurnDiff('t', 0, 'next')).toBe('d-0');
      // Overflow.
      store.writeTurnDiff('t', TURN_DIFF_CACHE_LIMIT, 'next', 'newest');
      // Oldest survived because the read bumped it to the tail.
      expect(store.readTurnDiff('t', 0, 'next')).toBe('d-0');
      // The next-oldest (turnIndex=1) was evicted.
      expect(store.readTurnDiff('t', 1, 'next')).toBeUndefined();
    });

    it('overwriting a key does not leak a stale LRU slot', () => {
      store.writeTurnDiff('t', 0, 'next', 'v1');
      store.writeTurnDiff('t', 0, 'next', 'v2');
      expect(store.readTurnDiff('t', 0, 'next')).toBe('v2');
      // Capacity must not have been consumed twice.
      for (let i = 1; i < TURN_DIFF_CACHE_LIMIT; i += 1) {
        store.writeTurnDiff('t', i, 'next', `d-${i}`);
      }
      // turn 0 is still live since overwrite reused the slot.
      expect(store.readTurnDiff('t', 0, 'next')).toBe('v2');
    });
  });

  describe('cumulative cache', () => {
    it('starts empty', () => {
      expect(store.cumulativeCache.size).toBe(0);
    });

    it('exposes the same Map so aggregation helper can read/write', () => {
      store.cumulativeCache.set('p1', 'diff-p1');
      expect(store.cumulativeCache.get('p1')).toBe('diff-p1');
    });

    it('invalidateCumulative() clears every entry', () => {
      store.cumulativeCache.set('p1', 'diff-p1');
      store.cumulativeCache.set('p2', 'diff-p2');
      store.invalidateCumulative();
      expect(store.cumulativeCache.size).toBe(0);
    });
  });

  // --- Bug D4 regression ---
  describe('invalidateTurn', () => {
    it('drops every compare-mode entry for (threadId, turnIndex)', () => {
      store.writeTurnDiff('t1', 3, 'next', 'DIFF-NEXT');
      store.writeTurnDiff('t1', 3, 'worktree', 'DIFF-WT');
      store.writeTurnDiff('t1', 4, 'next', 'OTHER-TURN');

      store.invalidateTurn('t1', 3);

      expect(store.readTurnDiff('t1', 3, 'next')).toBeUndefined();
      expect(store.readTurnDiff('t1', 3, 'worktree')).toBeUndefined();
      // Neighbouring turn survives.
      expect(store.readTurnDiff('t1', 4, 'next')).toBe('OTHER-TURN');
    });

    it('does not touch entries for other threads', () => {
      store.writeTurnDiff('t1', 3, 'next', 'T1');
      store.writeTurnDiff('t2', 3, 'next', 'T2');

      store.invalidateTurn('t1', 3);

      expect(store.readTurnDiff('t1', 3, 'next')).toBeUndefined();
      expect(store.readTurnDiff('t2', 3, 'next')).toBe('T2');
    });

    it('clears the cumulative cache too since aggregation is built from turns', () => {
      store.writeTurnDiff('t1', 3, 'next', 'X');
      store.cumulativeCache.set('payload-1', 'AGG');
      store.cumulativeCache.set('payload-2', 'AGG2');

      store.invalidateTurn('t1', 3);

      expect(store.cumulativeCache.size).toBe(0);
    });

    it('is a no-op when the key is absent', () => {
      store.writeTurnDiff('t1', 0, 'next', 'A');
      store.invalidateTurn('t1', 99);
      expect(store.readTurnDiff('t1', 0, 'next')).toBe('A');
    });

    it('prefix is not sensitive to turnIndex substrings (e.g. 3 vs 30)', () => {
      store.writeTurnDiff('t', 3, 'next', 'three');
      store.writeTurnDiff('t', 30, 'next', 'thirty');

      store.invalidateTurn('t', 3);

      expect(store.readTurnDiff('t', 3, 'next')).toBeUndefined();
      // Critical: key "t|30|next" must NOT have been dropped by a naive
      // startsWith("t|3") check on the key. The separator is part of the
      // prefix so "t|3|" only matches turnIndex 3.
      expect(store.readTurnDiff('t', 30, 'next')).toBe('thirty');
    });
  });

  describe('clearForThread', () => {
    it('resets every field and both caches', () => {
      store.open_();
      store.setSource('worktree');
      store.setTurnCompareMode('worktree');
      store.setViewMode('split');
      store.selectTurn(5);
      store.setCheckpoints([checkpoint({ turnIndex: 0 })]);
      store.setError('oops');
      store.writeTurnDiff('t', 0, 'next', 'x');
      store.cumulativeCache.set('p1', 'y');

      store.clearForThread();

      expect(store.open).toBe(false);
      expect(store.source).toBe('turn');
      expect(store.turnCompareMode).toBe('next');
      expect(store.viewMode).toBe('stacked');
      expect(store.selectedTurnIndex).toBeNull();
      expect(store.checkpoints).toEqual([]);
      expect(store.checkpointsLoaded).toBe(false);
      expect(store.error).toBeNull();
      expect(store.readTurnDiff('t', 0, 'next')).toBeUndefined();
      expect(store.cumulativeCache.size).toBe(0);
    });
  });
});
