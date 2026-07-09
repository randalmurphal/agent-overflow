import { describe, expect, it } from 'vitest';
import {
  decodeThreadDragPayload,
  encodeThreadDragPayload,
  projectedPaneDropWidth,
} from './threadDragPayload';

describe('pane thread drop helpers', () => {
  it('round-trips thread drag payloads', () => {
    const raw = encodeThreadDragPayload({ threadId: 'thread-1', title: 'Build UI' });

    expect(decodeThreadDragPayload(raw)).toEqual({
      threadId: 'thread-1',
      title: 'Build UI',
    });
  });

  it('returns null for malformed JSON', () => {
    expect(decodeThreadDragPayload('{not json')).toBeNull();
    expect(decodeThreadDragPayload('')).toBeNull();
  });

  it('returns null when threadId is missing', () => {
    expect(decodeThreadDragPayload(JSON.stringify({ title: 'Just a title' }))).toBeNull();
  });

  it('returns null when threadId is not a string', () => {
    expect(decodeThreadDragPayload(JSON.stringify({ threadId: 42, title: 'x' }))).toBeNull();
    expect(decodeThreadDragPayload(JSON.stringify({ threadId: null, title: 'x' }))).toBeNull();
  });

  it("defaults title to 'Untitled' when missing or non-string", () => {
    expect(decodeThreadDragPayload(JSON.stringify({ threadId: 't' }))).toEqual({
      threadId: 't',
      title: 'Untitled',
    });
    expect(decodeThreadDragPayload(JSON.stringify({ threadId: 't', title: 7 }))).toEqual({
      threadId: 't',
      title: 'Untitled',
    });
  });

  it('projects the fit-mode share of the row for the new pane', () => {
    // avg 800 of a 2400 post-insert total over a 3200px row -> 1067.
    const width = projectedPaneDropWidth([
      { widthPx: 700 },
      { widthPx: 900 },
    ], 3200, 560);

    expect(width).toBe(1067);
  });

  it('meets exactly at the average where fit and overflow regimes cross', () => {
    // The two regimes cross where paneRowWidth === total + average. Here
    // total 1800, average 900, row 2700: the fit share equals the base
    // width, so both formulas agree at 900.
    const width = projectedPaneDropWidth(
      [{ widthPx: 900 }, { widthPx: 900 }],
      2700,
      560,
    );

    expect(width).toBe(900);
  });

  it('projects the average base width when the strip overflows', () => {
    // Post-insert total (4000) exceeds the row (1000): no stretch, the
    // new pane lands at its base width (the average) verbatim.
    const width = projectedPaneDropWidth(
      Array.from({ length: 4 }, () => ({ widthPx: 800 })),
      1000,
      560,
    );

    expect(width).toBe(800);
  });

  it('uses full available width for the first dropped pane', () => {
    expect(projectedPaneDropWidth([], 900, 560)).toBe(900);
  });

  it('clamps the projection to minPaneWidth when the share is small', () => {
    // four 100px widths -> share = 100/500 -> 200 of 1000; average is
    // even smaller. minPaneWidth (560) wins.
    const width = projectedPaneDropWidth(
      Array.from({ length: 4 }, () => ({ widthPx: 100 })),
      1000,
      560,
    );
    expect(width).toBe(560);
  });

  it('falls back to minPaneWidth when widths sum to zero', () => {
    expect(projectedPaneDropWidth([{ widthPx: 0 }, { widthPx: 0 }], 1000, 320)).toBe(320);
  });

  it('falls back to minPaneWidth when widths are non-finite', () => {
    expect(projectedPaneDropWidth([{ widthPx: Number.NaN }], 1000, 240)).toBe(240);
    expect(projectedPaneDropWidth([{ widthPx: Number.POSITIVE_INFINITY }], 1000, 240)).toBe(240);
  });
});
