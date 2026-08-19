import { describe, expect, it } from 'vitest';
import type { Item } from '../../types/models';
import type { TimelineNode } from '../../utils/subagentGrouping';
import {
  NAV_TICK_SPACING_PX,
  deriveNavTicks,
  itemWindowBounds,
  markerGapFraction,
  mergeNavTicks,
  naturalRailHeightPx,
  previewTranslateYPercent,
  railCompressed,
  railHeightPx,
  tickDistanceScale,
  tickFraction,
  tickIndexFromPointer,
  tickNearestCenter,
  tickRangeInView,
  turnPreview,
  type BaselineTick,
  type MergedNavTicks,
  type NavTick,
} from './messageNavRail';

function item(partial: Partial<Item>): Item {
  return {
    id: 'i',
    threadId: 't',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'user_text',
    role: 'user',
    status: 'completed',
    summary: '',
    createdAt: 0,
    updatedAt: 0,
    ...partial,
  } as Item;
}

function leaf(partial: Partial<Item>): TimelineNode {
  return { kind: 'leaf', item: item(partial) } as TimelineNode;
}

function tick(id: string, turnIndex: number, nodeIndex: number | null): NavTick {
  return { id, turnIndex, itemIndex: 0, nodeIndex };
}

function base(id: string, turnIndex: number): BaselineTick {
  return { id, turnIndex, itemIndex: 0 };
}

describe('deriveNavTicks', () => {
  it('collects top-level user messages with node index and position', () => {
    const nodes: TimelineNode[] = [
      leaf({ id: 'u1', kind: 'user_text', turnIndex: 3, itemIndex: 1 }),
      leaf({ id: 'a1', kind: 'assistant_text', role: 'assistant' }),
      leaf({ id: 'u2', kind: 'user_text', turnIndex: 4 }),
    ];
    expect(deriveNavTicks(nodes)).toEqual([
      { id: 'u1', turnIndex: 3, itemIndex: 1, nodeIndex: 0 },
      { id: 'u2', turnIndex: 4, itemIndex: 0, nodeIndex: 2 },
    ]);
  });

  it('skips wire-only user messages and non-leaf nodes', () => {
    const nodes: TimelineNode[] = [
      leaf({ id: 'wire', kind: 'user_text', meta: '{"wire_only":true}' }),
      { kind: 'read_group', parent: item({ id: 'rg' }) } as unknown as TimelineNode,
      leaf({ id: 'u1', kind: 'user_text' }),
    ];
    expect(deriveNavTicks(nodes)).toEqual([
      { id: 'u1', turnIndex: 0, itemIndex: 0, nodeIndex: 2 },
    ]);
  });
});

describe('mergeNavTicks', () => {
  const baseline = [base('b1', 0), base('b2', 1), base('b3', 2), base('b4', 5), base('b5', 6)];
  const loaded = [tick('b3', 2, 4), tick('b4', 5, 9)];
  const bounds = { first: { turnIndex: 2, itemIndex: 0 }, last: { turnIndex: 5, itemIndex: 3 } };

  it('splices loaded ticks over the baseline inside the window span', () => {
    const merged = mergeNavTicks(baseline, loaded, bounds.first, bounds.last, true, true);
    expect(merged.ticks.map((t) => [t.id, t.nodeIndex])).toEqual([
      ['b1', null],
      ['b2', null],
      ['b3', 4],
      ['b4', 9],
      ['b5', null],
    ]);
    expect(merged.loadedStart).toBe(2);
    expect(merged.loadedEnd).toBe(3);
  });

  it('drops a side of the baseline when the window reaches that end', () => {
    // hasMoreNewer false: a baseline tail entry beyond the window is a
    // reverted message — stale, never rendered.
    const merged = mergeNavTicks(baseline, loaded, bounds.first, bounds.last, true, false);
    expect(merged.ticks.map((t) => t.id)).toEqual(['b1', 'b2', 'b3', 'b4']);
    const noHistory = mergeNavTicks(baseline, loaded, bounds.first, bounds.last, false, true);
    expect(noHistory.ticks.map((t) => t.id)).toEqual(['b3', 'b4', 'b5']);
    expect(noHistory.loadedStart).toBe(0);
  });

  it('a send the baseline has not heard of renders from the loaded side', () => {
    const withSend = [...loaded, tick('fresh', 5, 12)];
    const merged = mergeNavTicks(baseline, withSend, bounds.first, bounds.last, true, false);
    expect(merged.ticks.map((t) => t.id)).toEqual(['b1', 'b2', 'b3', 'b4', 'fresh']);
  });

  it('a baseline tick inside the window that is not loaded does not double-render', () => {
    // b3 sits in-span but the loaded list omits it (deleted / not yet
    // revealed): the window is the truth inside its own span.
    const merged = mergeNavTicks(baseline, [tick('b4', 5, 9)], bounds.first, bounds.last, true, true);
    expect(merged.ticks.map((t) => t.id)).toEqual(['b1', 'b2', 'b4', 'b5']);
  });

  it('an empty window still maps the whole baseline as unloaded', () => {
    const merged = mergeNavTicks(baseline, [], null, null, true, false);
    expect(merged.ticks.map((t) => t.nodeIndex)).toEqual([null, null, null, null, null]);
    expect(merged.loadedStart).toBe(-1);
  });

  it('a fully loaded empty thread yields no ticks', () => {
    expect(mergeNavTicks([], [], null, null, false, false).ticks).toEqual([]);
  });
});

describe('itemWindowBounds', () => {
  it('answers the loaded item span and null when empty', () => {
    expect(itemWindowBounds([])).toBeNull();
    const items = [item({ turnIndex: 2, itemIndex: 1 }), item({ turnIndex: 7, itemIndex: 4 })];
    expect(itemWindowBounds(items)).toEqual({
      first: { turnIndex: 2, itemIndex: 1 },
      last: { turnIndex: 7, itemIndex: 4 },
    });
  });
});

describe('rail geometry', () => {
  it('grows by the spacing constant until the cap compresses', () => {
    // Literal, not recomputed from the constant: a spacing change is a
    // deliberate test edit, not an automatic pass. 12px leaves a 4px
    // dot clear of two 2px lines.
    expect(NAV_TICK_SPACING_PX).toBe(12);
    expect(naturalRailHeightPx(5)).toBe(48);
    expect(railHeightPx(5, 1000)).toBe(48);
    expect(railHeightPx(200, 300)).toBe(300);
    expect(railCompressed(200, 300)).toBe(true);
    expect(railCompressed(5, 300)).toBe(false);
  });

  it('positions ticks fractionally, centering a lone tick', () => {
    expect(tickFraction(0, 1)).toBe(0.5);
    expect(tickFraction(0, 5)).toBe(0);
    expect(tickFraction(4, 5)).toBe(1);
    expect(tickFraction(2, 5)).toBe(0.5);
  });

  it('maps pointer Y linearly onto the index range, clamped', () => {
    expect(tickIndexFromPointer(-10, 100, 5)).toBe(0);
    expect(tickIndexFromPointer(0, 100, 5)).toBe(0);
    expect(tickIndexFromPointer(50, 100, 5)).toBe(2);
    expect(tickIndexFromPointer(500, 100, 5)).toBe(4);
    expect(tickIndexFromPointer(10, 0, 3)).toBe(0);
    expect(tickIndexFromPointer(10, 100, 0)).toBe(-1);
  });

  it('fisheye scale peaks at the hovered tick and tapers', () => {
    expect(tickDistanceScale(0)).toBe(1);
    expect(tickDistanceScale(1)).toBeLessThan(1);
    expect(tickDistanceScale(2)).toBeLessThan(tickDistanceScale(1));
    expect(tickDistanceScale(99)).toBe(tickDistanceScale(3));
    // null = no hover: every tick rests.
    expect(tickDistanceScale(null)).toBe(tickDistanceScale(3));
  });
});

describe('tickRangeInView', () => {
  // Two unloaded ticks flank three loaded ones — only the loaded
  // segment can be in view.
  const merged: MergedNavTicks = {
    ticks: [tick('p', 0, null), tick('a', 1, 2), tick('b', 2, 5), tick('c', 3, 9), tick('s', 4, null)],
    loadedStart: 1,
    loadedEnd: 3,
  };

  it('returns the inclusive tick range intersecting the node range', () => {
    expect(tickRangeInView(merged, 0, 20)).toEqual([1, 3]);
    expect(tickRangeInView(merged, 3, 9)).toEqual([2, 3]);
    expect(tickRangeInView(merged, 5, 5)).toEqual([2, 2]);
  });

  it('returns null when no loaded tick is in view', () => {
    expect(tickRangeInView(merged, 3, 4)).toBeNull();
    expect(tickRangeInView(merged, 10, 20)).toBeNull();
    expect(tickRangeInView({ ticks: [], loadedStart: -1, loadedEnd: -1 }, 0, 10)).toBeNull();
    expect(tickRangeInView(merged, 6, 3)).toBeNull();
  });
});

describe('markerGapFraction', () => {
  const offsets = [0, 1000, 3000];
  const offsetFor = (nodeIndex: number) => offsets[nodeIndex];
  const allLoaded: MergedNavTicks = {
    ticks: [tick('a', 0, 0), tick('b', 1, 1), tick('c', 2, 2)],
    loadedStart: 0,
    loadedEnd: 2,
  };

  it('sits centered in the gap between the two surrounding ticks', () => {
    // Center inside message a (offset 0..999): gap between a and b.
    expect(markerGapFraction(allLoaded, 0, offsetFor)).toBeCloseTo(0.25);
    expect(markerGapFraction(allLoaded, 500, offsetFor)).toBeCloseTo(0.25);
    // Reaching b hops the dot to the b→c gap in one step.
    expect(markerGapFraction(allLoaded, 1000, offsetFor)).toBeCloseTo(0.75);
    expect(markerGapFraction(allLoaded, 2999, offsetFor)).toBeCloseTo(0.75);
  });

  it('hides at the ends: above the first message and at the last', () => {
    expect(markerGapFraction(allLoaded, -50, offsetFor)).toBeNull();
    expect(markerGapFraction(allLoaded, 3000, offsetFor)).toBeNull();
    expect(markerGapFraction({ ticks: [tick('a', 0, 0)], loadedStart: 0, loadedEnd: 0 }, 0, offsetFor)).toBeNull();
  });

  it('points into the unloaded gaps at the window edges', () => {
    const withUnloaded: MergedNavTicks = {
      ticks: [tick('p', 0, null), tick('b', 1, 1), tick('c', 2, 2), tick('s', 3, null)],
      loadedStart: 1,
      loadedEnd: 2,
    };
    // Above the first loaded tick: between unloaded history and it.
    expect(markerGapFraction(withUnloaded, 500, offsetFor)).toBeCloseTo(0.5 / 3);
    // At/after the last loaded tick: between it and the unloaded tail.
    expect(markerGapFraction(withUnloaded, 3000, offsetFor)).toBeCloseTo(2.5 / 3);
  });

  it('answers null with nothing loaded (no geometry to place against)', () => {
    const noneLoaded: MergedNavTicks = {
      ticks: [tick('p', 0, null), tick('q', 1, null)],
      loadedStart: -1,
      loadedEnd: -1,
    };
    expect(markerGapFraction(noneLoaded, 500, offsetFor)).toBeNull();
  });
});

describe('turnPreview', () => {
  const items: Item[] = [
    item({ id: 'u1', kind: 'user_text', summary: 'fix the   bug\n\nplease' }),
    item({ id: 'a1', kind: 'assistant_text', role: 'assistant', summary: 'looking' }),
    item({ id: 'tc', kind: 'tool_call', role: 'assistant', summary: 'grep' }),
    item({ id: 'child', kind: 'assistant_text', role: 'assistant', summary: 'sub', parentId: 'tc' }),
    item({ id: 'a2', kind: 'assistant_text', role: 'assistant', summary: 'fixed it' }),
    item({ id: 'u2', kind: 'user_text', summary: 'thanks' }),
    item({ id: 'a3', kind: 'assistant_text', role: 'assistant', summary: 'after next turn' }),
  ];

  it('pairs the ask with the turn final top-level assistant reply', () => {
    expect(turnPreview(items, 'u1')).toEqual({
      userText: 'fix the bug please',
      assistantText: 'fixed it',
    });
  });

  it('stops at the next user message and tolerates a missing reply', () => {
    const tail = turnPreview(items, 'u2');
    expect(tail.userText).toBe('thanks');
    expect(tail.assistantText).toBe('after next turn');
    expect(turnPreview(items, 'missing')).toEqual({ userText: '', assistantText: '' });
  });

  it('a wire-only injection mid-turn neither ends the turn nor becomes the ask', () => {
    const withInjection = [
      item({ id: 'u1', kind: 'user_text', summary: 'real ask' }),
      item({ id: 'a1', kind: 'assistant_text', role: 'assistant', summary: 'early' }),
      item({
        id: 'inj',
        kind: 'user_text',
        summary: 'injected context',
        meta: '{"wire_only":true}',
      }),
      item({ id: 'a2', kind: 'assistant_text', role: 'assistant', summary: 'final answer' }),
      item({ id: 'u2', kind: 'user_text', summary: 'next ask' }),
    ];
    expect(turnPreview(withInjection, 'u1')).toEqual({
      userText: 'real ask',
      assistantText: 'final answer',
    });
  });

  it('scrubs attachment images and caps runaway text', () => {
    const long = 'x'.repeat(1000);
    const withAttachment = [
      item({
        id: 'u9',
        kind: 'user_text',
        summary: `see this\n\n![shot](attachment://abc123)`,
      }),
      item({ id: 'a9', kind: 'assistant_text', role: 'assistant', summary: long }),
    ];
    const preview = turnPreview(withAttachment, 'u9');
    expect(preview.userText).toBe('see this');
    expect(preview.assistantText.length).toBeLessThan(long.length);
    expect(preview.assistantText.endsWith('…')).toBe(true);
  });
});

describe('previewTranslateYPercent', () => {
  it('flips at the edges and centers elsewhere', () => {
    expect(previewTranslateYPercent(0, 5)).toBe(0);
    expect(previewTranslateYPercent(4, 5)).toBe(-100);
    expect(previewTranslateYPercent(2, 5)).toBe(-50);
    expect(previewTranslateYPercent(0, 1)).toBe(-50);
  });
});

describe('tickNearestCenter', () => {
  // Ticks at nodes 0 / 5 / 10, rows 100px tall at offset node·100 →
  // row centers 50 / 550 / 1050.
  const merged: MergedNavTicks = {
    ticks: [tick('u1', 0, 0), tick('u2', 1, 5), tick('u3', 2, 10)],
    loadedStart: 0,
    loadedEnd: 2,
  };
  const offsetForNode = (n: number) => n * 100;
  const sizeForNode = () => 100;

  it('picks the on-screen tick whose row center is nearest the given center', () => {
    expect(tickNearestCenter(merged, [0, 2], 400, offsetForNode, sizeForNode)).toBe(1);
    expect(tickNearestCenter(merged, [0, 2], 120, offsetForNode, sizeForNode)).toBe(0);
    expect(tickNearestCenter(merged, [0, 2], 900, offsetForNode, sizeForNode)).toBe(2);
  });

  it('only considers ticks inside the range', () => {
    // Center hugs u1, but the range says only u2/u3 are on screen.
    expect(tickNearestCenter(merged, [1, 2], 60, offsetForNode, sizeForNode)).toBe(1);
  });

  it('a single-tick range answers that tick wherever the center is', () => {
    expect(tickNearestCenter(merged, [2, 2], 0, offsetForNode, sizeForNode)).toBe(2);
  });

  it('a tie goes to the earlier tick', () => {
    // Center 300 is 250 from both row centers 50 and 550.
    expect(tickNearestCenter(merged, [0, 1], 300, offsetForNode, sizeForNode)).toBe(0);
  });
});
