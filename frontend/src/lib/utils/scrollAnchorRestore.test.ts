import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { rect, setElementRect } from '../../test/helpers/scrollDom';
import {
  firstVisibleItemAnchor,
  restoreAnchorSnapshot,
  restoreLoadedAnchorSnapshot,
} from './scrollAnchorRestore';

describe('firstVisibleItemAnchor', () => {
  let container: HTMLDivElement;

  beforeEach(() => {
    container = document.createElement('div');
    document.body.appendChild(container);
  });

  afterEach(() => {
    container.remove();
  });

  it('returns null when there are no item elements', () => {
    setElementRect(container, { top: 100, bottom: 700, height: 600 });
    expect(firstVisibleItemAnchor(container)).toBeNull();
  });

  it('skips items that are entirely above the viewport top', () => {
    setElementRect(container, { top: 100, bottom: 700, height: 600 });
    const a = document.createElement('div');
    a.dataset.itemId = 'a';
    container.appendChild(a);
    setElementRect(a, { top: 0, bottom: 50, height: 50 }); // bottom 50 < viewport top 100
    const b = document.createElement('div');
    b.dataset.itemId = 'b';
    container.appendChild(b);
    setElementRect(b, { top: 200, bottom: 280, height: 80 });

    expect(firstVisibleItemAnchor(container)).toEqual({
      kind: 'anchor',
      itemId: 'b',
      offsetTop: 100, // 200 - 100
    });
  });

  it('skips zero-height items', () => {
    setElementRect(container, { top: 0, bottom: 600, height: 600 });
    const a = document.createElement('div');
    a.dataset.itemId = 'a';
    container.appendChild(a);
    setElementRect(a, { top: 50, bottom: 50, height: 0 });
    const b = document.createElement('div');
    b.dataset.itemId = 'b';
    container.appendChild(b);
    setElementRect(b, { top: 100, bottom: 200, height: 100 });

    expect(firstVisibleItemAnchor(container)).toEqual({
      kind: 'anchor',
      itemId: 'b',
      offsetTop: 100,
    });
  });

  it('skips elements without a data-item-id', () => {
    setElementRect(container, { top: 0, bottom: 600, height: 600 });
    const naked = document.createElement('div');
    container.appendChild(naked);
    setElementRect(naked, { top: 50, bottom: 100, height: 50 });
    const real = document.createElement('div');
    real.dataset.itemId = 'real';
    container.appendChild(real);
    setElementRect(real, { top: 80, bottom: 130, height: 50 });

    expect(firstVisibleItemAnchor(container)).toEqual({
      kind: 'anchor',
      itemId: 'real',
      offsetTop: 80,
    });
  });

  it('captures negative offset when the item starts above the viewport top but extends in', () => {
    setElementRect(container, { top: 100, bottom: 700, height: 600 });
    const a = document.createElement('div');
    a.dataset.itemId = 'a';
    container.appendChild(a);
    setElementRect(a, { top: 80, bottom: 200, height: 120 });

    expect(firstVisibleItemAnchor(container)).toEqual({
      kind: 'anchor',
      itemId: 'a',
      offsetTop: -20, // 80 - 100
    });
  });
});

describe('restoreLoadedAnchorSnapshot', () => {
  let container: HTMLDivElement;

  beforeEach(() => {
    container = document.createElement('div');
    Object.defineProperty(container, 'scrollTop', {
      configurable: true,
      writable: true,
      value: 0,
    });
    document.body.appendChild(container);
  });

  afterEach(() => {
    container.remove();
  });

  it('returns false when itemId is empty', async () => {
    const result = await restoreLoadedAnchorSnapshot({
      container,
      snapshot: { kind: 'anchor', itemId: '', offsetTop: 0 },
      findNodeIndex: () => 0,
      offsetForIndex: () => 0,
      syncViewportState: () => {},
    });
    expect(result).toBe(false);
  });

  it('returns false when findNodeIndex returns -1', async () => {
    const result = await restoreLoadedAnchorSnapshot({
      container,
      snapshot: { kind: 'anchor', itemId: 'missing', offsetTop: 0 },
      findNodeIndex: () => -1,
      offsetForIndex: () => 0,
      syncViewportState: () => {},
    });
    expect(result).toBe(false);
  });

  it('rolls back the approximated scrollTop when shouldContinue flips false mid-flight', async () => {
    const target = document.createElement('div');
    target.dataset.itemId = 'target';
    container.appendChild(target);
    container.scrollTop = 100;

    let callCount = 0;
    const result = await restoreLoadedAnchorSnapshot({
      container,
      snapshot: { kind: 'anchor', itemId: 'target', offsetTop: 50 },
      findNodeIndex: () => 0,
      offsetForIndex: () => 500, // would set scrollTop to 450
      syncViewportState: () => {},
      shouldContinue: () => {
        // First call (after the first tick) returns true; second call
        // (after the rect-measure tick) returns false → triggers
        // rollback.
        callCount += 1;
        return callCount === 1;
      },
    });

    expect(result).toBe(false);
    // Rolled back to 100.
    expect(container.scrollTop).toBe(100);
  });
});

describe('restoreAnchorSnapshot', () => {
  let container: HTMLDivElement;

  beforeEach(() => {
    container = document.createElement('div');
    Object.defineProperty(container, 'scrollTop', {
      configurable: true,
      writable: true,
      value: 0,
    });
    document.body.appendChild(container);
  });

  afterEach(() => {
    container.remove();
  });

  it('bails when loadUntilItem returns false', async () => {
    const loadUntilItem = vi.fn().mockResolvedValue(false);
    const result = await restoreAnchorSnapshot({
      container,
      snapshot: { kind: 'anchor', itemId: 'gone', offsetTop: 10 },
      loadUntilItem,
      findNodeIndex: () => 0,
      offsetForIndex: () => 0,
      syncViewportState: () => {},
    });
    expect(result).toBe(false);
    expect(loadUntilItem).toHaveBeenCalledWith('gone');
  });

  it('honors shouldContinue() returning false before loadUntilItem fires', async () => {
    const loadUntilItem = vi.fn().mockResolvedValue(true);
    const result = await restoreAnchorSnapshot({
      container,
      snapshot: { kind: 'anchor', itemId: 'x', offsetTop: 0 },
      loadUntilItem,
      findNodeIndex: () => 0,
      offsetForIndex: () => 0,
      syncViewportState: () => {},
      shouldContinue: () => false,
    });
    expect(result).toBe(false);
    expect(loadUntilItem).not.toHaveBeenCalled();
  });
});
