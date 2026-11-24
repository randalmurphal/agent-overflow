// The fold behind `window.__aoRevealDrain()`. What is asserted here is the
// arithmetic a measurement window's END depends on: `draining === 0 &&
// smoothers === 0` is the claim that the reveal queue has handed everything
// to the reader, and a summary that got it wrong would close a bench window
// mid-stream and report numbers for a shorter run than it claims.

import { describe, expect, it } from 'vitest';
import { summarizeRevealDrain, type RevealDrainPane } from './revealDrainProbe';

function pane(smoothingItemCount: number, revealBoundary: unknown = null): RevealDrainPane {
  return { smoothingItemCount, revealBoundary };
}

describe('summarizeRevealDrain', () => {
  const broken = { smoothingItemCount: Number.NaN, revealBoundary: undefined } as RevealDrainPane;

  it.each<[string, RevealDrainPane[], { panes: number; draining: number; smoothers: number; boundaries: number }]>([
    ['reads an idle set of panes as fully drained', [pane(0), pane(0), pane(0)], { panes: 3, draining: 0, smoothers: 0, boundaries: 0 }],
    ['answers an empty registry without inventing a drain', [], { panes: 0, draining: 0, smoothers: 0, boundaries: 0 }],
    ['totals smoothers across panes and counts each draining pane once', [pane(2), pane(0), pane(5)], { panes: 3, draining: 2, smoothers: 7, boundaries: 0 }],
    // A gate can stand with no smoother left: the boundary is published by
    // the reveal recompute and cleared by it, and the window must not close in
    // the gap. Counting a gated pane as draining is what keeps that honest.
    ['counts a pane whose gate is engaged even with no live smoother', [pane(0, { turnIndex: 3, itemIndex: 1 })], { panes: 1, draining: 1, smoothers: 0, boundaries: 1 }],
    ['does not double-count a pane that is both gated and smoothing', [pane(4, { turnIndex: 0, itemIndex: 0 })], { panes: 1, draining: 1, smoothers: 4, boundaries: 1 }],
    ['treats an unreadable pane as contributing nothing rather than NaN', [broken, pane(1)], { panes: 2, draining: 1, smoothers: 1, boundaries: 0 }],
  ])('%s', (_name, panes, want) => {
    expect(summarizeRevealDrain(panes)).toEqual({ v: 1, ...want });
  });
});
