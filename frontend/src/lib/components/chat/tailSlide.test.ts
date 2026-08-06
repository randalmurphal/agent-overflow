import { describe, it, expect } from 'vitest';
import { slideDecision, transformTranslateY, type SlideObservation } from './tailSlide';

// The line-slide guard matrix. Geometry mirrors the real clamp: a 19.5px
// line height (text-[0.75rem] + leading-relaxed) and a 58.5px 3-line box.
const LH = 19.5;
const BOX = LH * 3;

const obs = (lines: number, o: Partial<SlideObservation> = {}): SlideObservation => ({
  innerH: lines * LH,
  innerW: 400,
  outerH: BOX,
  ...o,
});

describe('slideDecision', () => {
  it('calibrates on the first observation without animating', () => {
    const next = obs(5);
    expect(slideDecision(null, next, 0)).toEqual({ kind: 'none', memory: next });
  });

  it('slides an append-driven line crossing by exactly the clip delta', () => {
    const d = slideDecision(obs(5), obs(6), 0);
    expect(d.kind).toBe('slide');
    if (d.kind === 'slide') expect(d.startPx).toBeCloseTo(LH);
  });

  it('compounds onto an in-flight slide from the live translateY', () => {
    const d = slideDecision(obs(5), obs(6), 10);
    expect(d.kind).toBe('slide');
    if (d.kind === 'slide') expect(d.startPx).toBeCloseTo(10 + LH);
  });

  it('caps a compounded start at one full window', () => {
    const d = slideDecision(obs(5), obs(7), BOX - 5);
    expect(d.kind).toBe('slide');
    // true continuity would be (BOX - 5) + 2·LH — past the window, where the
    // inversion would start on content that was never visible.
    if (d.kind === 'slide') expect(d.startPx).toBeCloseTo(BOX);
  });

  it('clears on a width change (re-wrap: a translate cannot represent it)', () => {
    const d = slideDecision(obs(5), obs(6, { innerW: 380 }), 0);
    expect(d).toEqual({ kind: 'clear', memory: obs(6, { innerW: 380 }) });
  });

  it('a sub-pixel width wobble does not mask a real line crossing', () => {
    const d = slideDecision(obs(5), obs(6, { innerW: 400.3 }), 0);
    expect(d.kind).toBe('slide');
  });

  it('a sub-pixel-but-real width change (>0.5px) is a re-wrap, not an append', () => {
    // The a5a5d032 spring strand oscillates widths fractionally; integer
    // rounding used to classify these as appends and animate the re-wrap.
    const d = slideDecision(obs(5), obs(6, { innerW: 399 }), 0);
    expect(d.kind).toBe('clear');
  });

  it('clears on an outer-box height change (expanded flip / clamp engaging)', () => {
    const d = slideDecision(obs(5), obs(5, { outerH: 5 * LH }), 0);
    expect(d.kind).toBe('clear');
  });

  it('clears when the box grows AND overflows in one frame (regime crossing)', () => {
    // A 2-under-clamp tail takes a 3-line burst: the box grows to max-h and
    // the clip engages together. The growth is the scroll spring's motion —
    // a slide on top would double-ease the same pixels.
    const prev = obs(2, { outerH: 2 * LH });
    const d = slideDecision(prev, obs(5), 0);
    expect(d.kind).toBe('clear');
  });

  it('leaves an in-flight slide alone when the clip shrinks (window cut)', () => {
    // A wrap-stable cut removes invisible content above: the visible lines
    // stay pixel-identical, so a running release must not be snapped.
    const d = slideDecision(obs(8), obs(6), 12);
    expect(d).toEqual({ kind: 'none', memory: obs(6) });
  });

  it('does nothing when the geometry is unchanged', () => {
    expect(slideDecision(obs(5), obs(5), 0)).toEqual({ kind: 'none', memory: obs(5) });
  });

  it('clears on a whole-window discontinuity (no visible line survives)', () => {
    const d = slideDecision(obs(5), obs(8), 0);
    expect(d.kind).toBe('clear');
  });

  it('animates while the clip is under one window', () => {
    // 2 lines of advance: continuity exists (one visible line survives).
    const d = slideDecision(obs(5), obs(7), 0);
    expect(d.kind).toBe('slide');
    if (d.kind === 'slide') expect(d.startPx).toBeCloseTo(2 * LH);
  });

  it('recalibrates (memory null) on unusable geometry, so re-showing cannot read as an append', () => {
    const d = slideDecision(obs(5), obs(0, { outerH: 0 }), 0);
    expect(d).toEqual({ kind: 'clear', memory: null });
    // The re-show is then a calibration frame, not a slide.
    const next = obs(5);
    expect(slideDecision(null, next, 0)).toEqual({ kind: 'none', memory: next });
  });

  it('growth regime (box under max-h) never slides — clip stays zero', () => {
    const d = slideDecision(obs(1, { outerH: LH }), obs(2, { outerH: 2 * LH }), 0);
    expect(d.kind).toBe('clear'); // outer changed — and clip is 0 on both sides anyway
  });
});

describe('transformTranslateY', () => {
  it('reads ty from a computed matrix()', () => {
    expect(transformTranslateY('matrix(1, 0, 0, 1, 0, 19.5)')).toBeCloseTo(19.5);
  });

  it('reads ty from a computed matrix3d()', () => {
    expect(
      transformTranslateY('matrix3d(1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 42.25, 0, 1)'),
    ).toBeCloseTo(42.25);
  });

  it('treats none/empty/unparseable as zero', () => {
    expect(transformTranslateY('none')).toBe(0);
    expect(transformTranslateY('')).toBe(0);
    expect(transformTranslateY('translateY(10px)')).toBe(0);
    expect(transformTranslateY('matrix(garbage')).toBe(0);
  });
});
