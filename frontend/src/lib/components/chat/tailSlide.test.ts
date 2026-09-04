import { describe, it, expect } from 'vitest';
import {
  SLIDE_DRAIN_PER_FRAME,
  SLIDE_MAX_WINDOWS,
  SLIDE_MIN_STEP_PX,
  slideDecision,
  stepSlide,
  transformTranslateY,
  type SlideObservation,
} from './tailSlide';

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

  it('caps a compounded start at SLIDE_MAX_WINDOWS windows', () => {
    const d = slideDecision(obs(5), obs(9), 2 * BOX - 5);
    expect(d.kind).toBe('slide');
    // true continuity would be (2·BOX - 5) + 4·LH — past the cap, where the
    // rate is unreadable regardless and the excess snaps.
    if (d.kind === 'slide') expect(d.startPx).toBeCloseTo(SLIDE_MAX_WINDOWS * BOX);
  });

  it('lets a second window of catch-up compound past one window', () => {
    // Two lines landing in one frame onto a slide already two lines deep:
    // one window would pin here and teleport the excess.
    const d = slideDecision(obs(5), obs(7), 2 * LH);
    expect(d.kind).toBe('slide');
    if (d.kind === 'slide') expect(d.startPx).toBeCloseTo(4 * LH);
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

  it('slides by the overflow when the box grows AND overflows in one frame (regime crossing)', () => {
    // A 2-line tail takes a 3-line burst (a paragraph break plus the next
    // word in one reveal tick): the box grows to max-h and the clip engages
    // together. The growth is the scroll spring's motion, but the two lines
    // that overflowed re-packed upward instantly — that part slides.
    const prev = obs(2, { outerH: 2 * LH });
    const d = slideDecision(prev, obs(5), 0);
    expect(d.kind).toBe('slide');
    if (d.kind === 'slide') expect(d.startPx).toBeCloseTo(2 * LH);
  });

  it('clears when the box shrinks, whatever the clip did', () => {
    // The expanded flip collapsing back, or the clamp re-engaging on a
    // replacement: the lines re-derive, nothing to invert.
    const d = slideDecision(obs(5, { outerH: 5 * LH }), obs(6), 0);
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

  it('tickers through a whole-window discontinuity', () => {
    // No visible line survives a 3-line advance, but a slide through it
    // reads as a fast ticker where a snap reads as a glitch.
    const d = slideDecision(obs(5), obs(8), 0);
    expect(d.kind).toBe('slide');
    if (d.kind === 'slide') expect(d.startPx).toBeCloseTo(BOX);
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

describe('stepSlide', () => {
  const FRAME = 1000 / 60;

  it('drains a fixed fraction of the pending offset per frame', () => {
    expect(stepSlide(BOX, FRAME)).toBeCloseTo(BOX * (1 - SLIDE_DRAIN_PER_FRAME), 3);
  });

  it('is frame-rate independent: one 33ms frame equals two 16.7ms frames', () => {
    const two = stepSlide(stepSlide(BOX, FRAME), FRAME);
    expect(stepSlide(BOX, 2 * FRAME)).toBeCloseTo(two, 3);
  });

  it('never drains slower than the minimum step, and lands on exactly zero', () => {
    expect(stepSlide(2, FRAME)).toBeCloseTo(2 - SLIDE_MIN_STEP_PX, 3);
    let offset = BOX;
    let frames = 0;
    while (offset > 0 && frames < 200) {
      offset = stepSlide(offset, FRAME);
      frames += 1;
    }
    expect(offset).toBe(0);
    // Ease-out with the old 140ms transition's feel: gone well within a third of a second.
    expect(frames).toBeLessThan(20);
  });

  it('treats a non-positive offset or dt as at rest', () => {
    expect(stepSlide(0, FRAME)).toBe(0);
    expect(stepSlide(-4, FRAME)).toBe(0);
    expect(stepSlide(BOX, 0)).toBe(BOX);
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
