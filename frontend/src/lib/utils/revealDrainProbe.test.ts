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
  it('reads an idle set of panes as fully drained', () => {
    expect(summarizeRevealDrain([pane(0), pane(0), pane(0)])).toEqual({
      v: 1,
      panes: 3,
      draining: 0,
      smoothers: 0,
      boundaries: 0,
    });
  });

  it('answers an empty registry without inventing a drain', () => {
    expect(summarizeRevealDrain([])).toEqual({
      v: 1,
      panes: 0,
      draining: 0,
      smoothers: 0,
      boundaries: 0,
    });
  });

  it('totals smoothers across panes and counts each draining pane once', () => {
    const summary = summarizeRevealDrain([pane(2), pane(0), pane(5)]);
    expect(summary).toEqual({ v: 1, panes: 3, draining: 2, smoothers: 7, boundaries: 0 });
  });

  // A gate can stand with no smoother left: the boundary is published by
  // the reveal recompute and cleared by it, and the window must not close in
  // the gap. Counting a gated pane as draining is what keeps that honest.
  it('counts a pane whose gate is engaged even with no live smoother', () => {
    const summary = summarizeRevealDrain([pane(0, { turnIndex: 3, itemIndex: 1 })]);
    expect(summary).toEqual({ v: 1, panes: 1, draining: 1, smoothers: 0, boundaries: 1 });
  });

  it('does not double-count a pane that is both gated and smoothing', () => {
    const summary = summarizeRevealDrain([pane(4, { turnIndex: 0, itemIndex: 0 })]);
    expect(summary).toEqual({ v: 1, panes: 1, draining: 1, smoothers: 4, boundaries: 1 });
  });

  it('treats an unreadable pane as contributing nothing rather than NaN', () => {
    const broken = { smoothingItemCount: Number.NaN, revealBoundary: undefined } as RevealDrainPane;
    const summary = summarizeRevealDrain([broken, pane(1)]);
    expect(summary).toEqual({ v: 1, panes: 2, draining: 1, smoothers: 1, boundaries: 0 });
  });
});
