import { beforeEach, describe, expect, it } from 'vitest';
import {
  clearAllThreadSizePriorsForTest,
  clearThreadSizePriors,
  createRowEstimate,
  getReplayableSizePriors,
  peekThreadSizePriorsForTest,
  setThreadSizePriors,
  type SizePriorsEntry,
} from './priors';
import { UNMEASURED } from './sizes';

const entry = (overrides: Partial<SizePriorsEntry> = {}): SizePriorsEntry => ({
  width: 800,
  structureSig: 'sig-a',
  expansionSig: '',
  sizes: [100, 200, UNMEASURED, 56],
  ...overrides,
});

beforeEach(() => {
  clearAllThreadSizePriorsForTest();
});

describe('thread size priors persistence', () => {
  it('replays only on an exact key match', () => {
    setThreadSizePriors('t1', entry());
    expect(
      getReplayableSizePriors('t1', { width: 800, structureSig: 'sig-a', expansionSig: '' }),
    ).toEqual([100, 200, UNMEASURED, 56]);
  });

  it.each([
    ['width', { width: 640, structureSig: 'sig-a', expansionSig: '' }],
    ['structureSig', { width: 800, structureSig: 'sig-b', expansionSig: '' }],
    ['expansionSig', { width: 800, structureSig: 'sig-a', expansionSig: 'diff:x' }],
  ])('refuses the snapshot on a %s mismatch', (_, key) => {
    setThreadSizePriors('t1', entry());
    expect(getReplayableSizePriors('t1', key)).toBeUndefined();
  });

  it('returns undefined for an unknown thread', () => {
    expect(
      getReplayableSizePriors('nope', { width: 800, structureSig: 'sig-a', expansionSig: '' }),
    ).toBeUndefined();
  });

  it('clears a single thread', () => {
    setThreadSizePriors('t1', entry());
    clearThreadSizePriors('t1');
    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined();
  });

  it('evicts the least recently used entry past the cap', () => {
    for (let i = 0; i < 50; i++) {
      setThreadSizePriors(`t${i}`, entry());
    }
    expect(peekThreadSizePriorsForTest('t0')).toBeDefined();

    setThreadSizePriors('t50', entry());
    expect(peekThreadSizePriorsForTest('t0')).toBeUndefined();
    expect(peekThreadSizePriorsForTest('t1')).toBeDefined();
    expect(peekThreadSizePriorsForTest('t50')).toBeDefined();
  });

  it('bumps recency on a successful replay', () => {
    for (let i = 0; i < 50; i++) {
      setThreadSizePriors(`t${i}`, entry());
    }
    // t0 becomes most recent; the next eviction takes t1 instead.
    getReplayableSizePriors('t0', { width: 800, structureSig: 'sig-a', expansionSig: '' });

    setThreadSizePriors('t50', entry());
    expect(peekThreadSizePriorsForTest('t0')).toBeDefined();
    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined();
  });

  it('bumps recency on re-set of an existing thread', () => {
    for (let i = 0; i < 50; i++) {
      setThreadSizePriors(`t${i}`, entry());
    }
    setThreadSizePriors('t0', entry({ structureSig: 'sig-updated' }));

    setThreadSizePriors('t50', entry());
    expect(peekThreadSizePriorsForTest('t0')?.structureSig).toBe('sig-updated');
    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined();
  });

  it('a refused replay does not bump recency', () => {
    for (let i = 0; i < 50; i++) {
      setThreadSizePriors(`t${i}`, entry());
    }
    getReplayableSizePriors('t0', { width: 641, structureSig: 'sig-a', expansionSig: '' });

    setThreadSizePriors('t50', entry());
    expect(peekThreadSizePriorsForTest('t0')).toBeUndefined();
  });
});

describe('createRowEstimate', () => {
  it('resolves snapshot → kind → default in that order', () => {
    const estimate = createRowEstimate({
      snapshot: [120, UNMEASURED],
      kindOf: (index) => (index === 1 ? 'tool' : index === 2 ? 'unknown-kind' : undefined),
      kindHeights: { tool: 44 },
      defaultSize: 56,
    });
    expect(estimate.at(0)).toBe(120); // snapshot hit
    expect(estimate.at(1)).toBe(44); // snapshot UNMEASURED → kind
    expect(estimate.at(2)).toBe(56); // kind not in table → default
    expect(estimate.at(3)).toBe(56); // out of snapshot, no kind → default
  });

  it('treats a measured 0 as a valid prior', () => {
    const estimate = createRowEstimate({ snapshot: [0], defaultSize: 56 });
    expect(estimate.at(0)).toBe(0);
  });

  it('falls back to kind heights without any snapshot', () => {
    const estimate = createRowEstimate({
      kindOf: () => 'prose',
      kindHeights: { prose: 88 },
      defaultSize: 56,
    });
    expect(estimate.at(7)).toBe(88);
  });

  it('uses the flat default with no inputs', () => {
    const estimate = createRowEstimate({ defaultSize: 56 });
    expect(estimate.at(0)).toBe(56);
  });

  describe('shiftBase', () => {
    it('remaps the snapshot after a head prepend', () => {
      const estimate = createRowEstimate({ snapshot: [120, 130], defaultSize: 56 });
      estimate.shiftBase(2);
      // New head rows have no prior; old rows keep theirs at +2.
      expect(estimate.at(0)).toBe(56);
      expect(estimate.at(1)).toBe(56);
      expect(estimate.at(2)).toBe(120);
      expect(estimate.at(3)).toBe(130);
    });

    it('remaps the snapshot after a head removal', () => {
      const estimate = createRowEstimate({ snapshot: [120, 130], defaultSize: 56 });
      estimate.shiftBase(-1);
      expect(estimate.at(0)).toBe(130);
      expect(estimate.at(1)).toBe(56);
    });

    it('accumulates across splices', () => {
      const estimate = createRowEstimate({ snapshot: [120, 130], defaultSize: 56 });
      estimate.shiftBase(3);
      estimate.shiftBase(-2);
      expect(estimate.at(1)).toBe(120);
      expect(estimate.at(2)).toBe(130);
    });

    it('never aliases removed rows onto later-prepended fresh rows', () => {
      const estimate = createRowEstimate({ snapshot: [120, 130, 140], defaultSize: 56 });
      estimate.shiftBase(-2);
      expect(estimate.at(0)).toBe(140);

      estimate.shiftBase(2);
      // Net bias is 0 again, but rows 0..1 are new identities.
      expect(estimate.at(0)).toBe(56);
      expect(estimate.at(1)).toBe(56);
      expect(estimate.at(2)).toBe(140);
    });

    it('a removal consumes fresh head rows before originals', () => {
      const estimate = createRowEstimate({ snapshot: [120, 130], defaultSize: 56 });
      estimate.shiftBase(2);
      estimate.shiftBase(-1);
      // One fresh row survives at the head; original row 0 sits at 1.
      expect(estimate.at(0)).toBe(56);
      expect(estimate.at(1)).toBe(120);
      expect(estimate.at(2)).toBe(130);
    });

    it('does not remap kind lookups (they resolve against live indices)', () => {
      const seen: number[] = [];
      const estimate = createRowEstimate({
        snapshot: [120],
        kindOf: (index) => {
          seen.push(index);
          return 'tool';
        },
        kindHeights: { tool: 44 },
        defaultSize: 56,
      });
      estimate.shiftBase(2);
      expect(estimate.at(0)).toBe(44);
      expect(seen).toEqual([0]);
    });
  });
});
