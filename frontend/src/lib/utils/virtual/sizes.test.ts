// Ported from virtua's src/core/cache.spec.ts at 0.49.1 (MIT — see
// VIRTUA_LICENSE) and adapted to the sizes.ts surgery:
//   - a per-index `estimate` function replaces the flat default size;
//   - `updateLength` (tail) and `spliceHead` (head) replace
//     updateCacheLength(_, _, isShift), and the shift block's
//     expectations change because spliceHead REBUILDS the offsets memo
//     watermark-preservingly instead of discarding it;
//   - the estimateDefaultItemSize (median estimator) block is dropped —
//     priors replace the median estimator (plan §8 D2).

import { describe, expect, it } from 'vitest';
import {
  computeRange,
  findIndex,
  getItemOffset,
  getItemSize,
  getTotalSize,
  initSizeStore,
  isMeasured,
  setItemSize,
  spliceHead,
  takeSizeSnapshot,
  UNMEASURED,
  updateLength,
  type SizeStore,
} from './sizes';

const flat = (size: number) => () => size;

const range = <T>(length: number, cb: (i: number) => T): T[] => {
  const array: T[] = [];
  for (let i = 0; i < length; i++) {
    array.push(cb(i));
  }
  return array;
};

const sum = (sizes: readonly number[]): number => sizes.reduce((acc, s) => acc + s, 0);

const findComputedOffsetIndex = (offsets: readonly number[]): number => {
  const index = offsets.findIndex((o) => o === UNMEASURED);
  return (index === -1 ? offsets.length : index) - 1;
};

const sizesToOffsets = (sizes: readonly number[]): number[] => {
  return sizes.reduce(
    (acc, s, i) => {
      acc.push(acc[i] + s);
      return acc;
    },
    [0] as number[],
  );
};

/** Store with measured sizes but an untouched (empty) offsets memo. */
const storeWithSizes = (
  sizes: readonly number[],
  estimate: (index: number) => number,
): SizeStore => {
  const store = initSizeStore(sizes.length, estimate);
  for (let i = 0; i < sizes.length; i++) {
    store.sizes[i] = sizes[i];
  }
  return store;
};

/** Store with measured sizes and a fully computed offsets memo. */
const storeWithMeasuredOffsets = (
  sizes: readonly number[],
  estimate: (index: number) => number,
): SizeStore => {
  return storeWithOffsets(sizes, estimate, sizesToOffsets(sizes));
};

/** Store with an explicitly seeded (possibly partial) offsets memo. */
const storeWithOffsets = (
  sizes: readonly number[],
  estimate: (index: number) => number,
  offsets: readonly number[],
): SizeStore => {
  if (sizes.length + 1 !== offsets.length) {
    throw new Error('wrong offsets for sizes');
  }
  const store = storeWithSizes(sizes, estimate);
  store.offsets = [...offsets];
  store.offsetWatermark = findComputedOffsetIndex(offsets);
  return store;
};

const snapshotFields = (store: SizeStore) => ({
  length: store.length,
  sizes: [...store.sizes],
  offsets: [...store.offsets],
  offsetWatermark: store.offsetWatermark,
});

describe('getItemSize', () => {
  it('should get measured height', () => {
    const store = storeWithSizes([10, UNMEASURED], flat(20));
    expect(getItemSize(store, 0)).toBe(10);
  });
  it('should fall back to the estimate when unmeasured', () => {
    const store = storeWithSizes([10, UNMEASURED], flat(20));
    expect(getItemSize(store, 1)).toBe(20);
  });
  it('should consult the estimate per index', () => {
    const store = storeWithSizes([UNMEASURED, UNMEASURED, 7], (i) => (i + 1) * 100);
    expect(getItemSize(store, 0)).toBe(100);
    expect(getItemSize(store, 1)).toBe(200);
    expect(getItemSize(store, 2)).toBe(7);
  });
});

describe('isMeasured', () => {
  it('should report measurement state', () => {
    const store = storeWithSizes([10, UNMEASURED], flat(20));
    expect(isMeasured(store, 0)).toBe(true);
    expect(isMeasured(store, 1)).toBe(false);
  });
});

describe('setItemSize', () => {
  describe('with offsets not measured', () => {
    it.each([
      ['first', 0, [123, 20, 20, 20, 20, 20, 20, 20, 20, 20]],
      ['middle', 4, [20, 20, 20, 20, 123, 20, 20, 20, 20, 20]],
      ['last', 9, [20, 20, 20, 20, 20, 20, 20, 20, 20, 123]],
    ])('should set at %s', (_, index, expected) => {
      const store = storeWithSizes(
        range(10, () => 20),
        flat(20),
      );
      const initialOffsets = [...store.offsets];
      const initialWatermark = store.offsetWatermark;

      setItemSize(store, index, 123);
      expect(store.sizes).toEqual(expected);
      expect(store.offsets).toEqual(initialOffsets);
      expect(store.offsetWatermark).toBe(initialWatermark);
    });
  });

  describe('with offsets measured', () => {
    const offsets = [0, 10, 20, 30, 40, -1, -1, -1, -1, -1, -1];

    it('should lower the watermark if size is changed before it', () => {
      const store = storeWithOffsets(range(10, () => 20), flat(20), offsets);
      setItemSize(store, 1, 123);
      expect(store.sizes).toEqual([20, 123, 20, 20, 20, 20, 20, 20, 20, 20]);
      expect(store.offsets).toEqual(offsets);
      expect(store.offsetWatermark).toBe(1);
    });

    it('should not lower the watermark if size is changed at it', () => {
      const store = storeWithOffsets(range(10, () => 20), flat(20), offsets);
      setItemSize(store, 4, 123);
      expect(store.offsets).toEqual(offsets);
      expect(store.offsetWatermark).toBe(4);
    });

    it('should not lower the watermark if size is changed after it', () => {
      const store = storeWithOffsets(range(10, () => 20), flat(20), offsets);
      setItemSize(store, 5, 123);
      expect(store.offsets).toEqual(offsets);
      expect(store.offsetWatermark).toBe(4);
    });
  });

  describe('should return measurement status', () => {
    it('should return false if already measured', () => {
      const store = storeWithSizes(
        range(10, () => 20),
        flat(20),
      );
      expect(setItemSize(store, 0, 123)).toBe(false);
    });

    it('should return true if not measured', () => {
      const store = storeWithSizes(
        range(10, () => UNMEASURED),
        flat(20),
      );
      expect(setItemSize(store, 0, 123)).toBe(true);
    });
  });
});

describe('getItemOffset', () => {
  it('should get 0 if index is at start', () => {
    const store = storeWithSizes(
      range(10, () => 20),
      flat(30),
    );
    expect(getItemOffset(store, 0)).toBe(0);
    expect(store.offsets).toEqual([0, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1]);
  });

  it('should get 1 item if index is at start + 1', () => {
    const store = storeWithSizes(
      range(10, () => 20),
      flat(30),
    );
    expect(getItemOffset(store, 1)).toBe(20);
    expect(store.offsets).toEqual([0, 20, -1, -1, -1, -1, -1, -1, -1, -1, -1]);
  });

  it('should get total - 1 item if index is at last', () => {
    const sizes = range(10, () => 20);
    const store = storeWithSizes(sizes, flat(30));
    const last = sizes.length - 1;
    expect(getItemOffset(store, last)).toBe(sum(sizes) - sizes[last]);
    expect(store.offsets).toEqual([0, 20, 40, 60, 80, 100, 120, 140, 160, 180, -1]);
  });

  it('should resolve estimated height', () => {
    const store = storeWithSizes(
      range(10, () => UNMEASURED),
      flat(30),
    );
    expect(getItemOffset(store, 2)).toBe(60);
    expect(store.offsets).toEqual([0, 30, 60, -1, -1, -1, -1, -1, -1, -1, -1]);
  });

  it('should return 0 if store length is 0', () => {
    const store = initSizeStore(0, flat(30));
    expect(getItemOffset(store, 0)).toBe(0);
    expect(getItemOffset(store, 10)).toBe(0);
    expect(store.offsets).toEqual([-1]);
  });

  describe('with cached offsets', () => {
    const offsets = [0, 11, 22, 33, -1, -1, -1, -1, -1, -1, -1];

    it('should return cached offset if index is before the watermark', () => {
      const store = storeWithOffsets(range(10, () => 20), flat(30), offsets);
      expect(getItemOffset(store, 2)).toBe(22);
      expect(store.offsets).toEqual(offsets);
    });

    it('should return cached offset if index is at the watermark', () => {
      const store = storeWithOffsets(range(10, () => 20), flat(30), offsets);
      expect(getItemOffset(store, 3)).toBe(33);
      expect(store.offsets).toEqual(offsets);
    });

    it('should extend from cached offset if index is after the watermark', () => {
      const store = storeWithOffsets(range(10, () => 20), flat(30), offsets);
      expect(getItemOffset(store, 5)).toBe(33 + 20 * 2);
      expect(store.offsets).toEqual([0, 11, 22, 33, 53, 73, -1, -1, -1, -1, -1]);
    });
  });
});

describe('getTotalSize', () => {
  it('should succeed if sizes are filled', () => {
    const sizes = range(10, () => 20);
    const store = storeWithSizes(sizes, flat(30));
    expect(getTotalSize(store)).toBe(sum(sizes));
    expect(store.offsets).toEqual(sizesToOffsets(sizes));
  });

  it('should use estimates if sizes are not filled', () => {
    const store = storeWithSizes(
      range(10, () => UNMEASURED),
      flat(30),
    );
    expect(getTotalSize(store)).toBe(300);
    expect(store.offsets).toEqual(sizesToOffsets(range(10, () => 30)));
  });

  it('should return 0 if length is 0', () => {
    const store = initSizeStore(0, flat(30));
    expect(getTotalSize(store)).toBe(0);
    expect(store.offsets).toEqual([-1]);
  });

  describe('with cached offsets', () => {
    it('should extend from the watermark', () => {
      const store = storeWithOffsets(
        range(10, () => 20),
        flat(30),
        [0, 11, 22, 33, -1, -1, -1, -1, -1, -1, -1],
      );
      // Force a lower watermark than the seeded offsets imply, matching
      // the upstream case.
      store.offsetWatermark = 2;
      expect(getTotalSize(store)).toBe(sum(range(8, () => 20)) + 22);
      expect(store.offsets).toEqual([0, 11, 22, 42, 62, 82, 102, 122, 142, 162, 182]);
    });

    it('should add 1 item size if the watermark is at the last item', () => {
      const store = storeWithOffsets(
        range(10, () => 20),
        flat(30),
        [0, 11, 22, 33, 44, 55, 66, 77, 88, 99, -1],
      );
      expect(getTotalSize(store)).toBe(99 + 20);
      expect(store.offsets).toEqual([0, 11, 22, 33, 44, 55, 66, 77, 88, 99, 119]);
    });
  });
});

describe('findIndex', () => {
  const CACHE_LENGTH = 10;
  const measured = () =>
    storeWithMeasuredOffsets(
      range(CACHE_LENGTH, () => 20),
      flat(30),
    );

  it('should resolve estimated height', () => {
    const store = storeWithSizes(
      range(10, () => UNMEASURED),
      flat(25),
    );
    expect(findIndex(store, 100)).toBe(4);
  });

  it.each([
    ['far before start', -Number.MAX_SAFE_INTEGER, 0],
    ['start - 1px', -1, 0],
    ['start - 0.01px', -0.01, 0],
    ['start', 0, 0],
    ['start + 0.01px', 0.01, 0],
    ['start + 1px', 1, 0],
    ['index 1 - 1px', 19, 0],
    ['index 1 - 0.01px', 19.99, 0],
    ['index 1', 20, 1],
    ['index 1 + 0.01px', 20.01, 1],
    ['index 1 + 1px', 21, 1],
    ['index 1.5 - 1px', 29, 1],
    ['index 1.5 - 0.01px', 29.99, 1],
    ['index 1.5', 30, 1],
    ['index 1.5 + 0.01px', 30.01, 1],
    ['index 1.5 + 1px', 31, 1],
    ['end - 1px', 199, 9],
    ['end - 0.01px', 199.99, 9],
    ['end', 200, 9],
    ['end + 0.01px', 200.01, 9],
    ['end + 1px', 201, 9],
    ['far after end', Number.MAX_SAFE_INTEGER, 9],
  ])('should get %s', (_, offset, expected) => {
    expect(findIndex(measured(), offset)).toBe(expected);
  });

  it('should not get items with size 0', () => {
    const LENGTH = 20;
    const sizes = range(LENGTH, (i) => ([0, 1, LENGTH - 2, LENGTH - 1].includes(i) ? 20 : 0));
    const store = storeWithMeasuredOffsets(sizes, flat(30));
    expect(findIndex(store, sum(sizes) / 2 - 0.00001)).toBe(sizes.indexOf(0) - 1);
    expect(findIndex(store, sum(sizes) / 2)).toBe(sizes.lastIndexOf(0) + 1);
  });
});

describe('computeRange', () => {
  const CACHE_LENGTH = 10;
  const measured = () =>
    storeWithMeasuredOffsets(
      range(CACHE_LENGTH, () => 20),
      flat(30),
    );

  describe.each([[0], [Math.floor(CACHE_LENGTH / 2)], [CACHE_LENGTH - 1]])(
    'start from %i',
    (initialIndex) => {
      it('should get start if offset is at start', () => {
        expect(computeRange(measured(), 0, 100, initialIndex)).toEqual([0, 5]);
      });

      it('should get start + 1 if offset is at start + 1', () => {
        expect(computeRange(measured(), 20, 20 + 100, initialIndex)).toEqual([1, 6]);
      });

      it('should get last if offset is at end', () => {
        const store = measured();
        const last = store.length - 1;
        const start = sum(store.sizes);
        expect(computeRange(store, start, start + 100, initialIndex)).toEqual([last, last]);
      });

      it('should get last if offset is at end - 1', () => {
        const store = measured();
        const last = store.length - 1;
        const start = sum(store.sizes) - 20;
        expect(computeRange(store, start, start + 100, initialIndex)).toEqual([last, last]);
      });

      it('should get last - 1 if offset is at end - 1 and more', () => {
        const store = measured();
        const last = store.length - 1;
        const start = sum(store.sizes) - 20 - 1;
        expect(computeRange(store, start, start + 100, initialIndex)).toEqual([last - 1, last]);
      });

      it('should get start if offset is before start', () => {
        expect(computeRange(measured(), -1000, -1000 + 100, initialIndex)).toEqual([0, 0]);
      });

      it('should get last if offset is after end', () => {
        const store = measured();
        const last = store.length - 1;
        const start = sum(store.sizes) + 1000;
        expect(computeRange(store, start, start + 100, initialIndex)).toEqual([last, last]);
      });

      it('should get prevStartIndex if offset fits prevStartIndex', () => {
        const store = measured();
        const start = sum(store.sizes.slice(0, initialIndex));
        expect(computeRange(store, start, start + 100, initialIndex)).toEqual([
          initialIndex,
          expect.any(Number),
        ]);
      });
    },
  );
});

describe('initSizeStore', () => {
  it('should create an empty store', () => {
    const store = initSizeStore(10, flat(23));
    expect(store.length).toBe(10);
    expect(store.sizes).toEqual(range(10, () => UNMEASURED));
    expect(store.offsets).toEqual(range(11, () => UNMEASURED));
    expect(store.offsetWatermark).toBe(-1);
  });
});

describe('takeSizeSnapshot', () => {
  it('should copy measured sizes without sharing the array', () => {
    const store = storeWithSizes([10, UNMEASURED, 30], flat(40));
    const snapshot = takeSizeSnapshot(store);
    expect(snapshot).toEqual([10, UNMEASURED, 30]);

    snapshot[0] = 999;
    expect(store.sizes[0]).toBe(10);
    expect(takeSizeSnapshot(store)).toEqual([10, UNMEASURED, 30]);
  });
});

describe('updateLength', () => {
  it('should recover store length from 0', () => {
    const estimate = flat(40);
    const store = initSizeStore(10, estimate);
    const initial = snapshotFields(store);
    expect(updateLength(store, 0)).toBe(-400);
    expect(updateLength(store, 10)).toBe(400);
    expect(snapshotFields(store)).toEqual(initial);
  });

  it('should increase store length', () => {
    const store = initSizeStore(10, flat(40));
    const before = getTotalSize(initSizeStore(10, flat(40)));
    const res = updateLength(store, 15);
    expect(res).toBe(40 * 5);
    expect(store.length).toBe(15);
    expect(store.sizes).toEqual(range(15, () => UNMEASURED));
    expect(store.offsets.length).toBe(16);
    expect(getTotalSize(store)).toBe(before + res);
  });

  it('should increase filled store length and keep the watermark', () => {
    const sizes = range(10, (i) => (i + 1) * 10);
    const store = storeWithMeasuredOffsets(sizes, flat(40));
    const before = getTotalSize(store);
    const res = updateLength(store, 15);
    expect(res).toBe(40 * 5);
    expect(store.offsetWatermark).toBe(10);
    expect(store.offsets).toEqual([0, 10, 30, 60, 100, 150, 210, 280, 360, 450, 550, -1, -1, -1, -1, -1]);
    expect(store.sizes).toEqual([...sizes, -1, -1, -1, -1, -1]);
    expect(getTotalSize(store)).toBe(before + res);
  });

  it('should decrease store length', () => {
    const store = initSizeStore(10, flat(40));
    const res = updateLength(store, 5);
    expect(res).toBe(-(40 * 5));
    expect(store.length).toBe(5);
    expect(store.sizes).toEqual(range(5, () => UNMEASURED));
    expect(store.offsets.length).toBe(6);
  });

  it('should decrease filled store length with the exact removed sum', () => {
    const sizes = range(10, (i) => (i + 1) * 10);
    const store = storeWithMeasuredOffsets(sizes, flat(40));
    const before = getTotalSize(store);
    const res = updateLength(store, 5);
    expect(res).toBe(-sum(sizes.slice(-5)));
    expect(store.offsetWatermark).toBe(4);
    expect(store.offsets).toEqual([0, 10, 30, 60, 100, 150]);
    expect(store.sizes).toEqual([10, 20, 30, 40, 50]);
    expect(getTotalSize(store)).toBe(before + res);
  });

  it('should sum removed unmeasured rows through the estimate', () => {
    const store = storeWithSizes([10, UNMEASURED, 30, UNMEASURED, 50], flat(40));
    expect(updateLength(store, 2)).toBe(-(30 + 40 + 50));
  });

  it('should sum appended rows through the per-index estimate', () => {
    const store = initSizeStore(2, (i) => i * 10);
    expect(updateLength(store, 5)).toBe(20 + 30 + 40);
  });
});

describe('spliceHead', () => {
  it('should be a no-op at 0', () => {
    const store = storeWithMeasuredOffsets(
      range(10, (i) => (i + 1) * 10),
      flat(40),
    );
    const before = snapshotFields(store);
    expect(spliceHead(store, 0)).toBe(0);
    expect(snapshotFields(store)).toEqual(before);
  });

  describe('prepend', () => {
    it('should rebuild the offsets memo and keep the watermark', () => {
      const sizes = range(10, (i) => (i + 1) * 10);
      const store = storeWithMeasuredOffsets(sizes, flat(40));
      const before = getTotalSize(store);

      const res = spliceHead(store, 5);
      expect(res).toBe(40 * 5);
      expect(store.length).toBe(15);
      expect(store.sizes).toEqual([-1, -1, -1, -1, -1, ...sizes]);
      // New head rows are estimate-sized prefix entries; every previously
      // computed offset shifts by their sum (200).
      expect(store.offsets).toEqual([
        0, 40, 80, 120, 160, 200, 210, 230, 260, 300, 350, 410, 480, 560, 650, 750,
      ]);
      expect(store.offsetWatermark).toBe(15);
      expect(getTotalSize(store)).toBe(before + res);
    });

    it('should fill head offsets when no offsets were computed yet', () => {
      const store = storeWithSizes([7, 9], flat(40));
      expect(store.offsetWatermark).toBe(-1);

      const res = spliceHead(store, 3);
      expect(res).toBe(120);
      expect(store.sizes).toEqual([-1, -1, -1, 7, 9]);
      expect(store.offsets).toEqual([0, 40, 80, 120, -1, -1]);
      expect(store.offsetWatermark).toBe(3);
      expect(getTotalSize(store)).toBe(120 + 7 + 9);
    });

    it('should shift only up to a partial watermark', () => {
      const sizes = range(10, (i) => (i + 1) * 10);
      const store = storeWithOffsets(sizes, flat(40), [0, 10, 30, 60, -1, -1, -1, -1, -1, -1, -1]);
      expect(store.offsetWatermark).toBe(3);

      const res = spliceHead(store, 2);
      expect(res).toBe(80);
      expect(store.offsets).toEqual([0, 40, 80, 90, 110, 140, -1, -1, -1, -1, -1, -1, -1]);
      expect(store.offsetWatermark).toBe(5);
    });

    it('should consult the estimate at the NEW head indices', () => {
      const consulted: number[] = [];
      const store = storeWithSizes([7, 9], (i) => {
        consulted.push(i);
        return (i + 1) * 100;
      });

      const res = spliceHead(store, 3);
      expect(consulted).toEqual([0, 1, 2]);
      expect(res).toBe(100 + 200 + 300);
      expect(store.offsets).toEqual([0, 100, 300, 600, -1, -1]);
    });
  });

  describe('remove', () => {
    it('should shift surviving offsets when the watermark covers the removed prefix', () => {
      const sizes = range(10, (i) => (i + 1) * 10);
      const store = storeWithMeasuredOffsets(sizes, flat(40));
      const before = getTotalSize(store);

      const res = spliceHead(store, -5);
      expect(res).toBe(-sum(sizes.slice(0, 5)));
      expect(store.length).toBe(5);
      expect(store.sizes).toEqual([60, 70, 80, 90, 100]);
      expect(store.offsets).toEqual([0, 60, 130, 210, 300, 400]);
      expect(store.offsetWatermark).toBe(5);
      expect(getTotalSize(store)).toBe(before + res);
    });

    it('should drop the memo when the watermark is inside the removed prefix', () => {
      const sizes = range(10, (i) => (i + 1) * 10);
      const store = storeWithOffsets(sizes, flat(40), [0, 10, 30, -1, -1, -1, -1, -1, -1, -1, -1]);
      expect(store.offsetWatermark).toBe(2);

      const res = spliceHead(store, -5);
      expect(res).toBe(-sum(sizes.slice(0, 5)));
      expect(store.sizes).toEqual([60, 70, 80, 90, 100]);
      expect(store.offsets).toEqual([-1, -1, -1, -1, -1, -1]);
      expect(store.offsetWatermark).toBe(-1);
    });

    it('should consult the estimate at the PRE-splice indices of unmeasured removed rows', () => {
      const consulted: number[] = [];
      const store = storeWithSizes([UNMEASURED, UNMEASURED, 30, 40], (i) => {
        consulted.push(i);
        return i === 0 ? 5 : i === 1 ? 6 : 999;
      });

      const res = spliceHead(store, -2);
      expect(consulted).toEqual([0, 1]);
      expect(res).toBe(-(5 + 6));
      expect(store.sizes).toEqual([30, 40]);
    });

    it('should remove everything', () => {
      const sizes = range(4, (i) => (i + 1) * 10);
      const store = storeWithMeasuredOffsets(sizes, flat(40));
      const res = spliceHead(store, -4);
      expect(res).toBe(-sum(sizes));
      expect(store.length).toBe(0);
      expect(store.sizes).toEqual([]);
      expect(getTotalSize(store)).toBe(0);
    });

    it('should clamp removal to the store length', () => {
      const store = storeWithMeasuredOffsets([10, 20], flat(40));
      const res = spliceHead(store, -5);
      expect(res).toBe(-30);
      expect(store.length).toBe(0);
    });
  });
});
