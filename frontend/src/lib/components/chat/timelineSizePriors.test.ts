import { afterEach, describe, expect, it } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import type { ActivityRunNode, TimelineNode } from '../../utils/subagentGrouping';
import { ACTIVITY_RUN_CAP_REM_PX } from '../../utils/activityRunClip';
import { timelineRowStructuralSizeFor } from './timelineSizePriors.svelte';

// The estimate for a row the kind table cannot price. Only activity runs
// qualify: every other node kind has one typical height, a run has two — its
// header alone, or that header over a capped clip — and the engine places
// unmeasured rows with whichever it is in right now.

/** One header line. Always present, so it is a term in both shapes. */
const HEADER_PX = 24;

const REAL_INNER_HEIGHT = window.innerHeight;

/** happy-dom's viewport is fixed, and the clip's cap is half of it. */
function setViewportHeight(px: number): void {
  Object.defineProperty(window, 'innerHeight', { value: px, configurable: true });
}

afterEach(() => setViewportHeight(REAL_INNER_HEIGHT));

function leaf(id: string): TimelineNode {
  return { kind: 'leaf', item: makeItem({ id, kind: 'tool_call', toolName: 'Bash' }) };
}

function run(overrides: Partial<ActivityRunNode> = {}): ActivityRunNode {
  const children = overrides.children ?? Array.from({ length: 10 }, (_, i) => leaf(`i${i}`));
  return {
    kind: 'activity_run',
    runId: 'r1',
    threadId: 'thread-1',
    children,
    collapsed: false,
    live: false,
    mountedFrom: 0,
    mountedRows: children.length,
    membershipEpoch: 1,
    memberItemIds: [],
    ...overrides,
  };
}

describe('timelineRowStructuralSizeFor', () => {
  it('prices the same run differently in its two shapes', () => {
    const children = Array.from({ length: 10 }, (_, i) => leaf(`i${i}`));
    const closed = timelineRowStructuralSizeFor(run({ children, collapsed: true }));
    const open = timelineRowStructuralSizeFor(run({ children, collapsed: false }));

    // One header line vs that header over ten mounted rows: the whole reason
    // this is not a kind-table entry.
    expect(closed).toBe(HEADER_PX);
    expect(open).toBe(HEADER_PX + 200);
  });

  it('prices the two shapes apart, and prices liveness as neither', () => {
    // The whole 2x2, because the estimate must read ONE of the two facts. A
    // closed run is its header; an open one is that header over a capped clip,
    // and pricing it short would estimate the tallest row on the screen at a
    // twelfth of its height — a fast scroll past it lands nowhere near where it
    // aimed.
    //
    // Liveness is not the estimate's business: `collapsed` arrives with it
    // already folded in (`ActivityRunIdentity.collapsedFor`), so a run that
    // renders open while it works is simply an open run, and pricing on `live`
    // would move an estimate at the moment the height did not change.
    const children = Array.from({ length: 10 }, (_, i) => leaf(`i${i}`));
    const priceOf = (collapsed: boolean, live: boolean) =>
      timelineRowStructuralSizeFor(run({ children, collapsed, live }));

    expect(priceOf(true, false)).toBe(HEADER_PX);
    expect(priceOf(true, true)).toBe(HEADER_PX);
    expect(priceOf(false, true)).toBe(HEADER_PX + 200);
    expect(priceOf(false, false)).toBe(HEADER_PX + 200);
  });

  it('scales with the mounted window, not the run length', () => {
    // A 500-row run mounts a window of them. Estimating from the full child
    // list would place a row an order of magnitude taller than what renders —
    // and the cap below would hide the error, so this window is deliberately
    // small enough to stay under it.
    const long = run({
      children: Array.from({ length: 500 }, (_, i) => leaf(`i${i}`)),
      mountedRows: 12,
    });

    expect(timelineRowStructuralSizeFor(long)).toBe(HEADER_PX + 12 * 20);
  });

  it('never estimates the clip past its cap', () => {
    // 40 rows of floor height would be 800px; the clip cannot exceed its own
    // ceiling, and an overshooting estimate shrinks totalSize when the real
    // measurement lands — the failure mode this whole table is floored against.
    // The cap bounds the CLIP; the header sits above it and is not part of it.
    const wide = run({
      children: Array.from({ length: 40 }, (_, i) => leaf(`i${i}`)),
      mountedRows: 40,
    });

    // The cap is a `min()` of a viewport half and a rem height, so which half
    // wins depends on the window. Taking the rem half unconditionally would
    // overshoot the real ceiling on a short one.
    setViewportHeight(1400);
    expect(timelineRowStructuralSizeFor(wide)).toBe(HEADER_PX + ACTIVITY_RUN_CAP_REM_PX);

    setViewportHeight(400);
    expect(timelineRowStructuralSizeFor(wide)).toBe(HEADER_PX + 200);
  });

  it('declines every other node, so the kind table still decides', () => {
    expect(timelineRowStructuralSizeFor(leaf('i0'))).toBeUndefined();
    expect(timelineRowStructuralSizeFor(undefined)).toBeUndefined();
  });
});
