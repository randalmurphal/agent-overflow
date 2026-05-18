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

  it('computes projected width from average existing ratios', () => {
    const width = projectedPaneDropWidth([
      { ratio: 0.625 },
      { ratio: 0.375 },
    ], 1600, 560);

    expect(width).toBe(560);
  });

  it('uses full available width for the first dropped pane', () => {
    expect(projectedPaneDropWidth([], 900, 560)).toBe(900);
  });

  it('clamps projection to minPaneWidth when the share is small', () => {
    // four identical 0.25 ratios -> share = 0.25/1.25 -> 200 of 1000.
    // minPaneWidth (560) wins.
    const width = projectedPaneDropWidth(
      Array.from({ length: 4 }, () => ({ ratio: 0.25 })),
      1000,
      560,
    );
    expect(width).toBe(560);
  });

  it('falls back to minPaneWidth when ratios sum to zero', () => {
    expect(projectedPaneDropWidth([{ ratio: 0 }, { ratio: 0 }], 1000, 320)).toBe(320);
  });

  it('falls back to minPaneWidth when ratios are non-finite', () => {
    expect(projectedPaneDropWidth([{ ratio: Number.NaN }], 1000, 240)).toBe(240);
    expect(projectedPaneDropWidth([{ ratio: Number.POSITIVE_INFINITY }], 1000, 240)).toBe(240);
  });
});
