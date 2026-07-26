import { describe, expect, it } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import type { ActivityRunNode, TimelineNode } from '../../utils/subagentGrouping';
import { timelineRowStructuralSizeFor } from './timelineSizePriors.svelte';

// The estimate for a row the kind table cannot price. Only activity runs
// qualify: every other node kind has one typical height, a run has two (chip
// and capped clip) and the engine places unmeasured rows with whichever it is
// in right now.

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
    mountedFrom: 0,
    mountedRows: children.length,
    memberItemIds: [],
    ...overrides,
  };
}

describe('timelineRowStructuralSizeFor', () => {
  it('prices the same run differently in its two states', () => {
    const children = Array.from({ length: 10 }, (_, i) => leaf(`i${i}`));
    const chip = timelineRowStructuralSizeFor(run({ children, collapsed: true }));
    const clip = timelineRowStructuralSizeFor(run({ children, collapsed: false }));

    // One chip line vs ten mounted rows: the whole reason this is not a
    // kind-table entry.
    expect(chip).toBe(24);
    expect(clip).toBe(200);
  });

  it('scales with the mounted window, not the run length', () => {
    // A 500-row run mounts a window of them. Estimating from the full child
    // list would place a row an order of magnitude taller than what renders —
    // and the cap below would hide the error, so this window is deliberately
    // small enough to stay under it.
    const long = run({
      children: Array.from({ length: 500 }, (_, i) => leaf(`i${i}`)),
      mountedRows: 15,
    });

    expect(timelineRowStructuralSizeFor(long)).toBe(15 * 20);
  });

  it('never estimates past the clip cap', () => {
    // 40 rows of floor height would be 800px; the clip cannot exceed its own
    // ceiling, and an overshooting estimate shrinks totalSize when the real
    // measurement lands — the failure mode this whole table is floored against.
    const wide = run({
      children: Array.from({ length: 40 }, (_, i) => leaf(`i${i}`)),
      mountedRows: 40,
    });

    expect(timelineRowStructuralSizeFor(wide)).toBe(512);
  });

  it('declines every other node, so the kind table still decides', () => {
    expect(timelineRowStructuralSizeFor(leaf('i0'))).toBeUndefined();
    expect(timelineRowStructuralSizeFor(undefined)).toBeUndefined();
  });
});
