import { describe, expect, it } from 'vitest';
import { initSizeStore, setItemSize, type SizeStore } from './sizes';
import { computeWindow, EMPTY_WINDOW, fullWindow, rangesEqual, seedTailWindow } from './window';

// 10 rows × 100px measured — offsets are i*100, total 1000.
const measuredStore = (count = 10, height = 100): SizeStore => {
  const store = initSizeStore(count, () => 56);
  for (let i = 0; i < count; i++) {
    setItemSize(store, i, height);
  }
  return store;
};

describe('computeWindow', () => {
  it('expands the visible range symmetrically by bufferSize', () => {
    const store = measuredStore();
    // Visible [350, 650] → buffered [150, 850] → rows 1..8.
    expect(
      computeWindow(store, { scrollOffset: 350, viewportSize: 300, bufferSize: 200 }, 0),
    ).toEqual([1, 8]);
  });

  it('returns exactly the visible rows with bufferSize 0', () => {
    const store = measuredStore();
    expect(
      computeWindow(store, { scrollOffset: 350, viewportSize: 300, bufferSize: 0 }, 0),
    ).toEqual([3, 6]);
  });

  it('clamps at the top', () => {
    const store = measuredStore();
    expect(
      computeWindow(store, { scrollOffset: 0, viewportSize: 300, bufferSize: 200 }, 0),
    ).toEqual([0, 5]);
  });

  it('clamps at the bottom', () => {
    const store = measuredStore();
    expect(
      computeWindow(store, { scrollOffset: 700, viewportSize: 300, bufferSize: 200 }, 0),
    ).toEqual([5, 9]);
  });

  it('is prevStartIndex-independent in result (locality is only a hint)', () => {
    const store = measuredStore();
    const viewport = { scrollOffset: 350, viewportSize: 300, bufferSize: 200 };
    expect(computeWindow(store, viewport, 0)).toEqual([1, 8]);
    expect(computeWindow(store, viewport, 5)).toEqual([1, 8]);
    expect(computeWindow(store, viewport, 9)).toEqual([1, 8]);
    expect(computeWindow(store, viewport, 999)).toEqual([1, 8]);
  });

  it('returns EMPTY_WINDOW for an empty store', () => {
    const store = initSizeStore(0, () => 56);
    expect(
      computeWindow(store, { scrollOffset: 0, viewportSize: 300, bufferSize: 200 }, 0),
    ).toEqual(EMPTY_WINDOW);
  });

  it('returns EMPTY_WINDOW before the viewport is measured', () => {
    const store = measuredStore();
    expect(
      computeWindow(store, { scrollOffset: 0, viewportSize: 0, bufferSize: 200 }, 0),
    ).toEqual(EMPTY_WINDOW);
  });
});

describe('seedTailWindow', () => {
  it('mounts the last viewport + buffer px', () => {
    const store = measuredStore();
    // total 1000 − (300 + 200) = 500 → rows 5..9.
    expect(seedTailWindow(store, 300, 200)).toEqual([5, 9]);
  });

  it('covers everything when content fits the seeded extent', () => {
    const store = measuredStore(3);
    expect(seedTailWindow(store, 300, 200)).toEqual([0, 2]);
  });

  it('returns EMPTY_WINDOW for an empty store or unmeasured viewport', () => {
    expect(seedTailWindow(initSizeStore(0, () => 56), 300, 200)).toEqual(EMPTY_WINDOW);
    expect(seedTailWindow(measuredStore(), 0, 200)).toEqual(EMPTY_WINDOW);
  });
});

describe('fullWindow', () => {
  it('spans the whole store', () => {
    expect(fullWindow(measuredStore())).toEqual([0, 9]);
  });

  it('returns EMPTY_WINDOW for an empty store', () => {
    expect(fullWindow(initSizeStore(0, () => 56))).toEqual(EMPTY_WINDOW);
  });
});

describe('rangesEqual', () => {
  it('compares both bounds', () => {
    expect(rangesEqual([1, 8], [1, 8])).toBe(true);
    expect(rangesEqual([1, 8], [1, 9])).toBe(false);
    expect(rangesEqual([1, 8], [2, 8])).toBe(false);
    expect(rangesEqual(EMPTY_WINDOW, [0, -1])).toBe(true);
  });
});
