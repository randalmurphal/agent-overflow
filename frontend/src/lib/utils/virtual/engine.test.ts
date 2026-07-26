import { describe, expect, it } from 'vitest';
import { createEngine, mergeCompensations, type EngineOptions } from './engine';
import { createRowEstimate } from './priors';
import { UNMEASURED } from './sizes';
import type { RowEstimate } from './types';

const flatEstimate = (size: number): RowEstimate => ({
  at: () => size,
});

// 10 rows, 100px estimates, 300px viewport, 200px symmetric buffer.
// Offsets are i*100, total 1000, tail seed = last 500px = rows 5..9.
const mountedEngine = (overrides: Partial<EngineOptions> = {}) => {
  const engine = createEngine({
    itemCount: 10,
    estimate: flatEstimate(100),
    bufferSize: 200,
    ...overrides,
  });
  engine.applyViewportResize(300);
  return engine;
};

describe('mount seeding', () => {
  it('mounts nothing before the viewport is measured', () => {
    const engine = createEngine({ itemCount: 10, estimate: flatEstimate(100), bufferSize: 200 });
    expect(engine.getWindow()).toEqual([0, -1]);
    expect(engine.getTotalSize()).toBe(1000);
  });

  it('seeds the tail window on first viewport measure', () => {
    const engine = createEngine({ itemCount: 10, estimate: flatEstimate(100), bufferSize: 200 });
    const update = engine.applyViewportResize(300);
    expect(update).toEqual({ window: [5, 9], totalSize: 1000 });
    expect(engine.getWindow()).toEqual([5, 9]);
  });

  it('returns null for an unchanged viewport size', () => {
    const engine = mountedEngine();
    expect(engine.applyViewportResize(300)).toBeNull();
  });

  it('keeps the window tail-anchored until the first scroll input', () => {
    const engine = mountedEngine();
    const update = engine.applyMeasurements([[9, 150]]);
    expect(update).toEqual({ window: [5, 9], totalSize: 1050 });
  });

  it('seeds when data arrives after the viewport', () => {
    const engine = createEngine({ itemCount: 0, estimate: flatEstimate(100), bufferSize: 200 });
    expect(engine.applyViewportResize(300)).toBeNull();
    expect(engine.applyLength(5)).toEqual({ window: [0, 4], totalSize: 500 });
  });

  it('renderAll mounts every row regardless of geometry', () => {
    const engine = createEngine({
      itemCount: 10,
      estimate: flatEstimate(100),
      bufferSize: 200,
      renderAll: true,
    });
    expect(engine.getWindow()).toEqual([0, 9]);
    expect(engine.applyScroll(100)).toBeNull();
    expect(engine.getWindow()).toEqual([0, 9]);
    expect(engine.applyLength(12)?.window).toEqual([0, 11]);
  });
});

describe('applyScroll', () => {
  it('early-outs with null when the window is unchanged', () => {
    const engine = mountedEngine();
    // Bottom pin: [500, 1200] still resolves to the seeded rows 5..9.
    expect(engine.applyScroll(700)).toBeNull();
    expect(engine.getScrollOffset()).toBe(700);
    expect(engine.applyScroll(710)).toBeNull();
  });

  it('emits the new window when the range moves', () => {
    const engine = mountedEngine();
    expect(engine.applyScroll(0)).toEqual({ window: [0, 5], totalSize: 1000 });
    expect(engine.getWindow()).toEqual([0, 5]);
  });
});

describe('applyMeasurements', () => {
  it('growth entirely above the viewport emits remeasure-above compensation', () => {
    const engine = mountedEngine();
    engine.applyScroll(700);
    const update = engine.applyMeasurements([[2, 150]]);
    expect(update).toEqual({
      window: [4, 9],
      totalSize: 1050,
      compensation: { kind: 'remeasure-above', delta: 50, target: 750 },
    });
  });

  it('shrink above the viewport emits a negative delta', () => {
    const engine = mountedEngine();
    engine.applyScroll(700);
    const update = engine.applyMeasurements([[2, 40]]);
    expect(update?.compensation).toEqual({ kind: 'remeasure-above', delta: -60, target: 640 });
    expect(update?.totalSize).toBe(940);
  });

  it('growth at or below the viewport top does not compensate', () => {
    const engine = mountedEngine();
    engine.applyScroll(0);
    const update = engine.applyMeasurements([[3, 200]]);
    expect(update?.compensation).toBeUndefined();
    expect(update?.totalSize).toBe(1100);
  });

  it('a row straddling the viewport top does not compensate without attribution', () => {
    const engine = mountedEngine();
    engine.applyScroll(350);
    // Row 3 spans [300, 400): starts above 350 but ends below it. Whole-row
    // [index, size] cannot say which side of 350 the +100 landed on, so
    // with no measurer the engine compensates nothing.
    const update = engine.applyMeasurements([[3, 200]]);
    expect(update?.compensation).toBeUndefined();
  });
});

describe('applyMeasurements — straddling-row attribution', () => {
  it('compensates the measured part of the delta that landed above the top', () => {
    const engine = mountedEngine();
    engine.applyScroll(350);
    // Row 3 spans [300, 400) and grows by 100; 40 of it landed above 350.
    const update = engine.applyMeasurements([[3, 200]], () => 40);
    expect(update?.compensation).toEqual({ kind: 'remeasure-above', delta: 40, target: 390 });
  });

  it('asks only about the row that spans the top, and only once', () => {
    const engine = mountedEngine();
    engine.applyScroll(350);
    const asked: number[] = [];
    // Rows 1 (fully above), 3 (straddling), 6 (fully below) all change.
    engine.applyMeasurements(
      [
        [1, 150],
        [3, 200],
        [6, 150],
      ],
      (index) => {
        asked.push(index);
        return 40;
      },
    );
    expect(asked).toEqual([3]);
  });

  it('never asks about a row whose height did not actually move', () => {
    const engine = mountedEngine();
    engine.applyScroll(350);
    const asked: number[] = [];

    // First measurement landing exactly on the estimate: recorded
    // (UNMEASURED → 100) but zero delta, so there is nothing to attribute.
    expect(engine.applyMeasurements([[3, 100]], (index) => {
      asked.push(index);
      return 40;
    })).not.toBeNull();
    expect(asked).toEqual([]);

    // Re-delivering the same size filters out before the loop entirely.
    expect(engine.applyMeasurements([[3, 100]], (index) => {
      asked.push(index);
      return 40;
    })).toBeNull();
    expect(asked).toEqual([]);
  });

  it('composes with the exact above-viewport sum', () => {
    const engine = mountedEngine();
    engine.applyScroll(350);
    const update = engine.applyMeasurements(
      [
        [0, 160], // fully above: exact +60
        [3, 200], // straddling: measured +40 of its +100
      ],
      () => 40,
    );
    expect(update?.compensation?.delta).toBe(100);
  });

  it('clamps an over-reported shift to the row delta', () => {
    const engine = mountedEngine();
    engine.applyScroll(350);
    // A stale anchor claims 400 of a 100px growth landed above the top.
    const update = engine.applyMeasurements([[3, 200]], () => 400);
    expect(update?.compensation?.delta).toBe(100);
  });

  it('clamps a sign-flipped shift to zero (degrades to compensating nothing)', () => {
    const engine = mountedEngine();
    engine.applyScroll(350);
    // Row grew, measurer says content above the reading position moved UP.
    const update = engine.applyMeasurements([[3, 200]], () => -400);
    expect(update?.compensation).toBeUndefined();
  });

  it('mirrors both clamps for a shrinking row', () => {
    const engine = mountedEngine();
    engine.applyScroll(350);
    expect(engine.applyMeasurements([[3, 40]], () => -30)?.compensation?.delta).toBe(-30);

    const over = mountedEngine();
    over.applyScroll(350);
    expect(over.applyMeasurements([[3, 40]], () => -400)?.compensation?.delta).toBe(-60);

    const flipped = mountedEngine();
    flipped.applyScroll(350);
    expect(flipped.applyMeasurements([[3, 40]], () => 400)?.compensation).toBeUndefined();
  });

  it('treats a non-finite measurement as no information', () => {
    const engine = mountedEngine();
    engine.applyScroll(350);
    expect(engine.applyMeasurements([[3, 200]], () => NaN)?.compensation).toBeUndefined();
  });

  it('does not ask about the row exactly at the viewport top', () => {
    const engine = mountedEngine();
    engine.applyScroll(300);
    // Row 3 starts exactly at 300 — entirely at/below the top, so its
    // growth pushes content down and needs no compensation at all.
    const asked: number[] = [];
    engine.applyMeasurements([[3, 200]], (index) => {
      asked.push(index);
      return 40;
    });
    expect(asked).toEqual([]);
  });

  it('does not ask about a row that ends exactly at the viewport top', () => {
    const engine = mountedEngine();
    engine.applyScroll(400);
    // Row 3 spans [300, 400): entirely above, so it compensates exactly and
    // the measured path must not double-count it.
    const asked: number[] = [];
    const update = engine.applyMeasurements([[3, 200]], (index) => {
      asked.push(index);
      return 40;
    });
    expect(asked).toEqual([]);
    expect(update?.compensation?.delta).toBe(100);
  });

  it('a mixed batch sums only the rows above the viewport', () => {
    const engine = mountedEngine();
    engine.applyScroll(700);
    const update = engine.applyMeasurements([
      [0, 150],
      [9, 200],
    ]);
    expect(update?.compensation).toEqual({ kind: 'remeasure-above', delta: 50, target: 750 });
    expect(update?.totalSize).toBe(1150);
  });

  it('returns null when every entry matches the stored size', () => {
    const engine = mountedEngine();
    expect(engine.applyMeasurements([[5, 100]])).not.toBeNull();
    expect(engine.applyMeasurements([[5, 100]])).toBeNull();
  });

  it('ignores out-of-range indices (stale RO deliveries)', () => {
    const engine = mountedEngine();
    expect(
      engine.applyMeasurements([
        [99, 50],
        [-1, 50],
      ]),
    ).toBeNull();
  });

  it('a measurement equal to the estimate records without moving geometry', () => {
    const engine = mountedEngine();
    engine.applyScroll(700);
    expect(engine.isMeasuredAt(5)).toBe(false);
    const update = engine.applyMeasurements([[5, 100]]);
    expect(update).toEqual({ window: [5, 9], totalSize: 1000 });
    expect(engine.isMeasuredAt(5)).toBe(true);
  });
});

describe('applyLength', () => {
  it('tail growth extends the window and totalSize without compensation', () => {
    const engine = mountedEngine();
    engine.applyScroll(700);
    expect(engine.applyLength(12)).toEqual({ window: [5, 11], totalSize: 1200 });
  });

  it('tail shrink clamps the window', () => {
    const engine = mountedEngine();
    engine.applyScroll(700);
    expect(engine.applyLength(8)).toEqual({ window: [5, 7], totalSize: 800 });
  });

  it('returns null when nothing changed', () => {
    const engine = mountedEngine();
    expect(engine.applyLength(10)).toBeNull();
  });

  it('head prepend emits head-splice compensation', () => {
    const engine = mountedEngine();
    engine.applyScroll(300);
    const update = engine.applyLength(12, 2);
    expect(update).toEqual({
      window: [1, 8],
      totalSize: 1200,
      compensation: { kind: 'head-splice', delta: 200, target: 500 },
    });
  });

  it('a prepend consults the estimate at the NEW post-splice head indices', () => {
    // The estimate carries no index-keyed state to remap (unlike the
    // deleted snapshot+shiftBase design) — it is simply called fresh for
    // whatever index the store asks about. This test double records the
    // indices to pin which ones prependHead actually consults.
    const seen: number[] = [];
    const indexedEstimate: RowEstimate = {
      at(index) {
        seen.push(index);
        return 100;
      },
    };
    const engine = mountedEngine({ estimate: indexedEstimate });
    seen.length = 0;

    engine.applyLength(12, 2);
    // prependHead computes offsets for the 2 new head rows via estimate
    // at their post-splice indices 0 and 1.
    expect(seen).toEqual([0, 1]);
  });

  it('head removal takes the offsets-memo path and consults no estimate at all', () => {
    const seen: number[] = [];
    const indexedEstimate: RowEstimate = {
      at(index) {
        seen.push(index);
        return 100;
      },
    };
    const engine = mountedEngine({ estimate: indexedEstimate });
    engine.applyScroll(300); // seeds the full offsets memo
    seen.length = 0;

    const update = engine.applyLength(8, -2);
    expect(update?.compensation).toEqual({ kind: 'head-splice', delta: -200, target: 100 });
    // Seeding computed the full offsets memo, so the removal reads the
    // memo's already-known removed size instead of calling estimate.
    expect(seen).toEqual([]);
  });

  it('clamps the compensation target at 0', () => {
    const engine = mountedEngine();
    engine.applyScroll(100);
    const update = engine.applyLength(8, -2);
    expect(update?.compensation).toEqual({ kind: 'head-splice', delta: -200, target: 0 });
  });

  it('handles a head splice and tail change in one batch', () => {
    const engine = mountedEngine();
    engine.applyScroll(300);
    const update = engine.applyLength(11, 2);
    expect(update?.totalSize).toBe(1100);
    expect(update?.compensation).toEqual({ kind: 'head-splice', delta: 200, target: 500 });
  });

  it('handles a head removal and tail growth in one batch', () => {
    const engine = mountedEngine();
    engine.applyScroll(300);
    // 10 → 12 rows where 2 left the head: 4 rows of tail growth.
    const update = engine.applyLength(12, -2);
    expect(update?.totalSize).toBe(1200);
    expect(update?.compensation).toEqual({ kind: 'head-splice', delta: -200, target: 100 });
  });

  it('handles the net-zero shape: head removal offset by equal tail growth', () => {
    const engine = mountedEngine();
    engine.applyScroll(300);
    // Length stays 10, but 2 rows left the head and 2 arrived at the
    // tail — the first-line guard deliberately admits it because
    // headSplice is nonzero.
    const update = engine.applyLength(10, -2);
    expect(update?.totalSize).toBe(1000);
    expect(update?.compensation).toEqual({ kind: 'head-splice', delta: -200, target: 100 });
  });

  it('shrinks to empty and regrows from estimates', () => {
    const engine = mountedEngine();
    engine.applyScroll(300);
    const emptied = engine.applyLength(0);
    expect(emptied?.window).toEqual([0, -1]);
    expect(emptied?.totalSize).toBe(0);
    expect(emptied?.compensation).toBeUndefined();
    const regrown = engine.applyLength(5);
    expect(regrown?.totalSize).toBe(500);
  });
});

describe('applyKeyedReorder', () => {
  it('returns null for the identity permutation', () => {
    const engine = mountedEngine();
    engine.applyMeasurements([[5, 80]]);
    expect(engine.applyKeyedReorder([0, 1, 2, 3, 4, 5, 6, 7, 8, 9])).toBeNull();
    expect(engine.sizeAt(5)).toBe(80);
  });

  it('measured sizes travel with their rows (the reorder-overlap regression)', () => {
    // The 2026-07-03 bug shape: a queued user message (80px, row 5) and a
    // thinking row (240px, row 6) swap order via an upsert. Moved rows
    // keep their DOM size, so no RO delivery follows — position-keyed
    // sizes would leave row 6's offset computed from the 80px entry and
    // the rows below overlapping.
    const engine = mountedEngine();
    engine.applyScroll(0);
    engine.applyMeasurements([
      [5, 80],
      [6, 240],
    ]);
    const totalBefore = engine.getTotalSize();

    const update = engine.applyKeyedReorder([0, 1, 2, 3, 4, 6, 5, 7, 8, 9]);
    expect(update).not.toBeNull();
    expect(engine.sizeAt(5)).toBe(240);
    expect(engine.sizeAt(6)).toBe(80);
    expect(engine.isMeasuredAt(5)).toBe(true);
    expect(engine.isMeasuredAt(6)).toBe(true);
    expect(engine.getItemOffset(6)).toBe(engine.getItemOffset(5) + 240);
    expect(engine.getItemOffset(7)).toBe(engine.getItemOffset(6) + 80);
    // A permutation moves sizes, it never creates or destroys extent.
    expect(engine.getTotalSize()).toBe(totalBefore);
    // Below-viewport-top moves carry no compensation.
    expect(update?.compensation).toBeUndefined();
  });

  it('a new key comes in unmeasured', () => {
    const engine = mountedEngine();
    engine.applyMeasurements([[5, 80]]);
    // Same length, but index 5's key is brand new (-1) and the old row 5
    // moved to index 6.
    engine.applyKeyedReorder([0, 1, 2, 3, 4, -1, 5, 7, 8, 9]);
    expect(engine.isMeasuredAt(5)).toBe(false);
    expect(engine.sizeAt(5)).toBe(100); // estimate
    expect(engine.sizeAt(6)).toBe(80);
    expect(engine.isMeasuredAt(6)).toBe(true);
  });

  it('size changes entirely above the viewport top emit remeasure-above compensation', () => {
    const engine = mountedEngine();
    engine.applyMeasurements([
      [2, 150],
      [8, 100],
    ]);
    engine.applyScroll(700);
    // Swap rows 2 and 8: the region above the viewport shrinks by 50px.
    const update = engine.applyKeyedReorder([0, 1, 8, 3, 4, 5, 6, 7, 2, 9]);
    expect(update?.compensation).toEqual({ kind: 'remeasure-above', delta: -50, target: 650 });
    expect(engine.sizeAt(2)).toBe(100);
    expect(engine.sizeAt(8)).toBe(150);
  });

  it('handles a keyed change that also changes length (mid-list insert)', () => {
    const engine = mountedEngine();
    engine.applyMeasurements([
      [5, 80],
      [6, 240],
    ]);
    // A new row lands between old rows 5 and 6.
    const update = engine.applyKeyedReorder([0, 1, 2, 3, 4, 5, -1, 6, 7, 8, 9]);
    expect(update).not.toBeNull();
    expect(engine.getItemCount()).toBe(11);
    expect(engine.sizeAt(5)).toBe(80);
    expect(engine.isMeasuredAt(6)).toBe(false);
    expect(engine.sizeAt(7)).toBe(240);
  });

  // The next four pin the anchor-based compensation rule for mid-list
  // splices — the review-pane collapse/expand shape. The anchor is the
  // row under the viewport top; compensation keeps it (or the nearest
  // surviving row after it) at the same distance from the viewport.
  it('mid-list removal above the viewport compensates by the removed extent', () => {
    const engine = mountedEngine();
    engine.applyMeasurements([
      [2, 150],
      [3, 150],
    ]);
    // Offsets: r4=500 … r7=800; viewport top 800 sits on row 7.
    engine.applyScroll(800);
    // Remove rows 2 and 3 (a collapsed region of 300 measured px).
    const update = engine.applyKeyedReorder([0, 1, 4, 5, 6, 7, 8, 9]);
    expect(update?.compensation).toEqual({
      kind: 'remeasure-above',
      delta: -300,
      target: 500,
    });
    // Old row 7 now sits at index 5, offset 500 — stationary under the target.
    expect(engine.getItemOffset(5)).toBe(500);
  });

  it('mid-list insert above the viewport compensates by the inserted extent', () => {
    const engine = mountedEngine();
    engine.applyScroll(800); // viewport top on row 8
    // Two new rows land at index 2 (an expanded region, estimate-sized).
    const update = engine.applyKeyedReorder([0, 1, -1, -1, 2, 3, 4, 5, 6, 7, 8, 9]);
    expect(update?.compensation).toEqual({
      kind: 'remeasure-above',
      delta: 200,
      target: 1000,
    });
    expect(engine.getItemCount()).toBe(12);
  });

  it('mid-list removal below the viewport does not compensate', () => {
    const engine = mountedEngine();
    engine.applyScroll(0);
    // Remove rows 7 and 8, far below the 300px viewport.
    const update = engine.applyKeyedReorder([0, 1, 2, 3, 4, 5, 6, 9]);
    expect(update?.compensation).toBeUndefined();
    expect(update?.totalSize).toBe(800);
  });

  it('keeps the next surviving row stationary when the anchor row is removed', () => {
    const engine = mountedEngine();
    engine.applyScroll(450); // viewport top inside row 4
    // Rows 3-5 vanish (the region being read collapses); row 6 survives.
    const update = engine.applyKeyedReorder([0, 1, 2, 6, 7, 8, 9]);
    // Row 6's top moves 600 → 300; keeping it stationary is the -300 delta.
    expect(update?.compensation).toEqual({
      kind: 'remeasure-above',
      delta: -300,
      target: 150,
    });
    expect(engine.getItemOffset(3)).toBe(300);
  });

  it('reports no compensation when nothing at or after the anchor survives', () => {
    const engine = mountedEngine();
    engine.applyScroll(700);
    // Everything from the anchor down is removed; the browser's own
    // scrollTop clamp is the right outcome.
    const update = engine.applyKeyedReorder([0, 1, 2, 3, 4]);
    expect(update?.compensation).toBeUndefined();
    expect(update?.totalSize).toBe(500);
  });
});

describe('exact-height rows', () => {
  const exactEstimate = (size: number): RowEstimate => ({
    at: () => size,
    isExact: () => true,
  });

  it('exact rows count as measured without any measurement delivery', () => {
    const engine = createEngine({ itemCount: 10, estimate: exactEstimate(100), bufferSize: 200 });
    engine.applyViewportResize(300);
    expect(engine.isMeasuredAt(0)).toBe(true);
    expect(engine.isMeasuredAt(9)).toBe(true);
    expect(engine.sizeAt(4)).toBe(100);
    expect(engine.getItemOffset(7)).toBe(700);
    // Snapshots stay honest: exactness is not a measurement.
    expect(engine.takeSnapshot()).toEqual(new Array(10).fill(UNMEASURED));
  });

  it('a mixed estimate reports exactness per row', () => {
    // Even rows exact (line blocks), odd rows measured (comment threads).
    const engine = createEngine({
      itemCount: 4,
      estimate: { at: () => 100, isExact: (index) => index % 2 === 0 },
      bufferSize: 200,
    });
    engine.applyViewportResize(300);
    expect(engine.isMeasuredAt(0)).toBe(true);
    expect(engine.isMeasuredAt(1)).toBe(false);
    engine.applyMeasurements([[1, 140]]);
    expect(engine.isMeasuredAt(1)).toBe(true);
    expect(engine.getItemOffset(2)).toBe(240);
  });
});

describe('mergeCompensations (same-flush adapter merge)', () => {
  it('recomputes the combined target from summed deltas', () => {
    // Both targets derive from the same scrollOffset (100).
    const prior = { kind: 'remeasure-above' as const, delta: 50, target: 150 };
    const next = { kind: 'remeasure-above' as const, delta: 30, target: 130 };
    expect(mergeCompensations(prior, next, 100)).toEqual({
      kind: 'remeasure-above',
      delta: 80,
      target: 180,
    });
  });

  it('a clamped next-target cannot inflate the merge', () => {
    // Near the top of the thread: prior grows +100, next shrinks −100.
    // next.target clamped at 0 (50 − 100 < 0); deriving the merge from
    // `next.target + prior.delta` would say 100 — the true combined
    // target is the unchanged 50.
    const prior = { kind: 'remeasure-above' as const, delta: 100, target: 150 };
    const next = { kind: 'remeasure-above' as const, delta: -100, target: 0 };
    expect(mergeCompensations(prior, next, 50)).toEqual({
      kind: 'remeasure-above',
      delta: 0,
      target: 50,
    });
  });

  it('clamps the merged target at 0 and keeps head-splice precedence', () => {
    const prior = { kind: 'head-splice' as const, delta: -300, target: 0 };
    const next = { kind: 'remeasure-above' as const, delta: 20, target: 120 };
    expect(mergeCompensations(prior, next, 100)).toEqual({
      kind: 'head-splice',
      delta: -280,
      target: 0,
    });
  });
});

describe('priors integration (hot revisit)', () => {
  it('a priors-hit revisit mounts at final geometry and re-measures with zero movement', () => {
    const sizes = [120, 80, 200, 56, 90, 130, 75, 110, 60, 95];
    const engine = createEngine({
      itemCount: 10,
      estimate: createRowEstimate({ rowPrior: (index) => sizes[index], defaultSize: 56 }),
      bufferSize: 200,
    });

    // First paint is at the exact persisted total — no estimate cascade.
    expect(engine.applyViewportResize(300)?.totalSize).toBe(1016);

    // The pin write lands; the seeded tail window already matches.
    expect(engine.applyScroll(716)).toBeNull();

    // The RO delivers identical measurements: geometry must not move and
    // nothing must be compensated.
    const update = engine.applyMeasurements(sizes.map((size, index) => [index, size] as const));
    expect(update).toEqual({ window: [4, 9], totalSize: 1016 });
  });
});

describe('queries', () => {
  it('answers geometry questions from the model', () => {
    const engine = mountedEngine();
    expect(engine.getItemCount()).toBe(10);
    expect(engine.getViewportSize()).toBe(300);
    expect(engine.findItemIndex(250)).toBe(2);
    expect(engine.getItemOffset(7)).toBe(700);
    expect(engine.sizeAt(3)).toBe(100);
    expect(engine.isMeasuredAt(3)).toBe(false);

    engine.applyMeasurements([[3, 140]]);
    expect(engine.sizeAt(3)).toBe(140);
    expect(engine.isMeasuredAt(3)).toBe(true);
  });

  it('takes a measured-only snapshot for priors persistence', () => {
    const engine = mountedEngine();
    engine.applyMeasurements([[0, 110]]);
    const snapshot = engine.takeSnapshot();
    expect(snapshot[0]).toBe(110);
    expect(snapshot.slice(1)).toEqual(new Array(9).fill(UNMEASURED));
  });

  describe('targetOffsetFor', () => {
    it('aligns start/end/center', () => {
      const engine = mountedEngine();
      expect(engine.targetOffsetFor(5, 'start')).toBe(500);
      expect(engine.targetOffsetFor(5, 'end')).toBe(300);
      expect(engine.targetOffsetFor(5, 'center')).toBe(400);
    });

    it('nearest stays put when fully visible', () => {
      const engine = mountedEngine();
      engine.applyScroll(450);
      expect(engine.targetOffsetFor(5, 'nearest')).toBe(450);
    });

    it('nearest aligns start when the row is above', () => {
      const engine = mountedEngine();
      engine.applyScroll(600);
      expect(engine.targetOffsetFor(5, 'nearest')).toBe(500);
    });

    it('nearest aligns end when the row is below', () => {
      const engine = mountedEngine();
      engine.applyScroll(100);
      expect(engine.targetOffsetFor(5, 'nearest')).toBe(300);
    });

    it('applies extraOffset and clamps to the scrollable extent', () => {
      const engine = mountedEngine();
      expect(engine.targetOffsetFor(5, 'start', -20)).toBe(480);
      expect(engine.targetOffsetFor(9, 'start', 500)).toBe(700);
      expect(engine.targetOffsetFor(0, 'start', -50)).toBe(0);
    });

    it('clamps the index', () => {
      const engine = mountedEngine();
      expect(engine.targetOffsetFor(-5)).toBe(0);
      expect(engine.targetOffsetFor(99)).toBe(700);
    });

    it('returns 0 for an empty store', () => {
      const engine = createEngine({ itemCount: 0, estimate: flatEstimate(100), bufferSize: 200 });
      expect(engine.targetOffsetFor(0)).toBe(0);
    });

    it('clamps every align to 0 when content is shorter than the viewport', () => {
      // 2×100px rows in a 300px viewport: maxOffset is 0 — the align-end
      // and center math would otherwise go negative (short threads hit
      // this via scroll-to-item).
      const engine = createEngine({ itemCount: 2, estimate: flatEstimate(100), bufferSize: 200 });
      engine.applyViewportResize(300);
      expect(engine.targetOffsetFor(1, 'start')).toBe(0);
      expect(engine.targetOffsetFor(1, 'end')).toBe(0);
      expect(engine.targetOffsetFor(1, 'center')).toBe(0);
      expect(engine.targetOffsetFor(1, 'nearest')).toBe(0);
    });
  });

  it('findItemIndex returns -1 on an empty store (a sentinel callers clamp)', () => {
    const engine = createEngine({ itemCount: 0, estimate: flatEstimate(100), bufferSize: 200 });
    expect(engine.findItemIndex(0)).toBe(-1);
  });
});
