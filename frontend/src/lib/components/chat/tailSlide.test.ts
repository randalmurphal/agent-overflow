import { describe, it, expect } from 'vitest';
import {
  MAX_ADAPTIVE_CHARS_PER_SEC,
  MAX_REVEAL_TICK_DT_MS,
  PerItemSmoother,
  __resetRevealFrameGateForTest,
  type SmoothingClock,
} from '../../markdown/smoothing/PerItemSmoother';
import {
  SLIDE_DRAIN_PER_FRAME,
  SLIDE_MAX_CATCHUP_FRAMES,
  SLIDE_MAX_WINDOWS,
  SLIDE_MIN_STEP_PX,
  slideDecision,
  stepSlide,
  transformTranslateY,
  type SlideObservation,
} from './tailSlide';

// The reveal smoother's clock, stepped a paint at a time: callbacks
// scheduled during a frame run on the next one, as rAF does.
class PaintClock implements SmoothingClock {
  private current = 0;
  private nextHandle = 1;
  private pending = new Map<number, (frameTimestamp?: number) => void>();
  now(): number { return this.current; }
  schedule(cb: (frameTimestamp?: number) => void): number {
    const handle = this.nextHandle++;
    this.pending.set(handle, cb);
    return handle;
  }
  cancel(handle: number): void { this.pending.delete(handle); }
  paint(ms: number): void {
    this.current += ms;
    const due = [...this.pending.values()];
    this.pending.clear();
    for (const cb of due) cb(this.current);
  }
}

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
  it.each<[string, SlideObservation | null, SlideObservation, number]>([
    ['calibrates on the first observation without animating', null, obs(5), 0],
    // A wrap-stable cut removes invisible content above: the visible lines
    // stay pixel-identical, so a running release must not be snapped.
    ['leaves an in-flight slide alone when the clip shrinks (window cut)', obs(8), obs(6), 12],
    ['does nothing when the geometry is unchanged', obs(5), obs(5), 0],
  ])('%s', (_name, prev, next, offset) => {
    expect(slideDecision(prev, next, offset)).toEqual({ kind: 'none', memory: next });
  });

  it.each<[string, SlideObservation, SlideObservation, number, number]>([
    ['slides an append-driven line crossing by exactly the clip delta', obs(5), obs(6), 0, LH],
    ['compounds onto an in-flight slide from the live translateY', obs(5), obs(6), 10, 10 + LH],
    // Fifty lines in one frame (a run of blank lines at the ceiling, or a
    // unit landing whole) onto a slide already five windows deep: past the
    // cap the ticker would be a blur, so the excess snaps.
    ['caps a compounded start at SLIDE_MAX_WINDOWS windows', obs(60), obs(110), 5 * BOX, SLIDE_MAX_WINDOWS * BOX],
    // Four lines of content hold one line above the box: an offset past
    // that would show blank space, whatever is pending.
    ['never translates past the content above the box', obs(3), obs(4), 2 * BOX, LH],
    // Two lines landing in one frame onto a slide already two lines deep:
    // one window would pin here and teleport the excess.
    ['lets a second window of catch-up compound past one window', obs(5), obs(7), 2 * LH, 4 * LH],
    // No visible line survives a 3-line advance, but a slide through it
    // reads as a fast ticker where a snap reads as a glitch.
    ['tickers through a whole-window discontinuity', obs(5), obs(8), 0, BOX],
    // 2 lines of advance: continuity exists (one visible line survives).
    ['animates while the clip is under one window', obs(5), obs(7), 0, 2 * LH],
    ['a sub-pixel width wobble does not mask a real line crossing', obs(5), obs(6, { innerW: 400.3 }), 0, LH],
    // A 2-line tail takes a 3-line burst (a paragraph break plus the next
    // word in one reveal tick): the box grows to max-h and the clip engages
    // together. The growth is the scroll spring's motion, but the two lines
    // that overflowed re-packed upward instantly — that part slides.
    ['slides by the overflow when the box grows AND overflows in one frame (regime crossing)', obs(2, { outerH: 2 * LH }), obs(5), 0, 2 * LH],
  ])('%s', (_name, prev, next, offset, startPx) => {
    const d = slideDecision(prev, next, offset);
    expect(d.kind).toBe('slide');
    if (d.kind === 'slide') expect(d.startPx).toBeCloseTo(startPx);
  });

  it.each<[string, SlideObservation, SlideObservation]>([
    ['clears on a width change (re-wrap: a translate cannot represent it)', obs(5), obs(6, { innerW: 380 })],
    // The a5a5d032 spring strand oscillates widths fractionally; integer
    // rounding used to classify these as appends and animate the re-wrap.
    ['a sub-pixel-but-real width change (>0.5px) is a re-wrap, not an append', obs(5), obs(6, { innerW: 399 })],
    ['clears on an outer-box height change (expanded flip / clamp engaging)', obs(5), obs(5, { outerH: 5 * LH })],
    // The expanded flip collapsing back, or the clamp re-engaging on a
    // replacement: the lines re-derive, nothing to invert.
    ['clears when the box shrinks, whatever the clip did', obs(5, { outerH: 5 * LH }), obs(6)],
    // Outer changed, and the clip is 0 on both sides.
    ['growth regime (box under max-h) never slides — clip stays zero', obs(1, { outerH: LH }), obs(2, { outerH: 2 * LH })],
  ])('%s', (_name, prev, next) => {
    expect(slideDecision(prev, next, 0)).toEqual({ kind: 'clear', memory: next });
  });

  it('brings a pending offset down to what a cut left above the box', () => {
    // The window cut shortened the content below the pending offset (a cut
    // shorter than the retention contract): the offset follows the content
    // that remains instead of translating into blank space.
    const d = slideDecision(obs(55), obs(10), 20 * LH);
    expect(d.kind).toBe('slide');
    if (d.kind === 'slide') expect(d.startPx).toBeCloseTo(7 * LH);
    // A cut that keeps the retention contract leaves the offset alone.
    expect(slideDecision(obs(55), obs(21), 18 * LH)).toEqual({ kind: 'none', memory: obs(21) });
  });

  it('never pins under a sustained intake faster than the drain holds in a window', () => {
    // Four short lines a frame (preserved blank lines at the ceiling) for
    // 300 frames: the offset rises to where the drain takes four lines a
    // frame and stays there; every line that landed is in the offset.
    let lines = 5;
    let prev = obs(lines);
    let offset = 0;
    for (let frame = 0; frame < 300; frame += 1) {
      lines += 4;
      const next = obs(lines);
      const d = slideDecision(prev, next, offset);
      expect(d.kind).toBe('slide');
      if (d.kind !== 'slide') return;
      expect(d.startPx).toBeCloseTo(offset + 4 * LH);
      offset = stepSlide(d.startPx, 1000 / 60);
      prev = next;
    }
    // Four lines a frame settle the pre-drain offset at 4·LH / 0.35
    // (223px) and leave 65% of it after the drain: between two and three
    // windows, and the same every frame.
    expect(offset).toBeGreaterThan(2 * BOX);
    expect(offset).toBeLessThan(3 * BOX);
  });

  it('keeps pace with the reveal on one-character lines under every pattern of paint gaps', () => {
    // A code block of one-character lines at the ceiling: the reveal
    // lands up to MAX_REVEAL_TICK_DT_MS of budget a paint (16 chars, eight
    // lines at 20Hz), and the drain takes the same elapsed time, so the
    // offset settles well under the cap and no line snaps, whatever the
    // gaps: steady 60, 30 and 20Hz paints, a 165Hz panel whose main thread
    // alternates one frame and eight (6ms, 48ms), and a 60Hz one that
    // alternates a frame and a 50ms paint. With a drain of one frame a
    // paint, 20Hz pinned at the cap within a second; with a drain of the
    // last paint period, the alternating 165Hz panel reached 19 lines.
    const patterns: Array<[string, number[]]> = [
      ['60Hz', [1000 / 60]],
      ['30Hz', [1000 / 30]],
      ['20Hz', [1000 / 20]],
      ['165Hz 1:8', [1000 / 165, 8000 / 165]],
      ['60Hz 1:3', [1000 / 60, 50]],
    ];
    for (const [name, gaps] of patterns) {
      __resetRevealFrameGateForTest();
      const clock = new PaintClock();
      let revealed = '';
      const smoother = new PerItemSmoother({ onReveal: (delta) => { revealed += delta; }, clock });
      smoother.appendDelta('a\n'.repeat(4000));
      let lines = 5;
      let prev = obs(lines);
      let offset = 0;
      let peak = 0;
      let elapsed = 0;
      for (let paint = 0; elapsed < 3000; paint += 1) {
        const gap = gaps[paint % gaps.length]!;
        const before = revealed.length;
        clock.paint(gap);
        elapsed += gap;
        const landed = (revealed.length - before) / 2;
        if (landed === 0) {
          offset = stepSlide(offset, gap);
          continue;
        }
        lines += landed;
        const next = obs(lines);
        const d = slideDecision(prev, next, offset);
        expect(d.kind, name).toBe('slide');
        if (d.kind !== 'slide') return;
        // Every line that landed is in the offset: nothing snapped.
        expect(d.startPx, `${name} paint ${paint}`).toBeCloseTo(offset + landed * LH);
        peak = Math.max(peak, d.startPx);
        offset = stepSlide(d.startPx, gap);
        prev = next;
      }
      expect(revealed.length / (elapsed / 1000), name).toBeGreaterThan(MAX_ADAPTIVE_CHARS_PER_SEC * 0.9);
      expect(peak, name).toBeLessThan(SLIDE_MAX_WINDOWS * BOX * 0.75);
      smoother.dispose();
    }
  });

  it('slides by the lines that landed when the clamp cut lines above the box in the same frame', () => {
    // A cut keeps the clamp's window bounded and lands in the frame of the
    // text that pushed it over: the box shows one new line while the
    // content above it shrank. The slide is the line that landed.
    const one = slideDecision(obs(22), obs(22), 0, LH);
    expect(one.kind).toBe('slide');
    if (one.kind === 'slide') expect(one.startPx).toBeCloseTo(LH);
    const two = slideDecision(obs(22), obs(21), 0, 3 * LH);
    expect(two.kind).toBe('slide');
    if (two.kind === 'slide') expect(two.startPx).toBeCloseTo(2 * LH);
    // A cut with no landing is a shrink: nothing visible moved.
    expect(slideDecision(obs(22), obs(20), 0, 2 * LH)).toEqual({ kind: 'none', memory: obs(20) });
  });

  it('recalibrates (memory null) on unusable geometry, so re-showing cannot read as an append', () => {
    const d = slideDecision(obs(5), obs(0, { outerH: 0 }), 0);
    expect(d).toEqual({ kind: 'clear', memory: null });
  });
});

describe('stepSlide', () => {
  const FRAME = 1000 / 60;

  it('drains a fixed fraction of the pending offset per frame', () => {
    expect(stepSlide(BOX, FRAME)).toBeCloseTo(BOX * (1 - SLIDE_DRAIN_PER_FRAME), 3);
  });

  it('drains a sub-frame dt by its share of a frame', () => {
    expect(stepSlide(BOX, FRAME / 2)).toBeCloseTo(BOX * Math.pow(1 - SLIDE_DRAIN_PER_FRAME, 0.5), 3);
  });

  it('drains a gap up to the reveal\'s stall bound, and resumes after a longer stall instead of catching it up', () => {
    // The reveal's stall bound is the slide's: a 20Hz paint drains three
    // frames' worth, the most any gap drains, so a dropped frame drains
    // its whole gap and a stall drains three frames, as the reveal lands
    // three frames of budget on the paint after one.
    expect(SLIDE_MAX_CATCHUP_FRAMES).toBeCloseTo(MAX_REVEAL_TICK_DT_MS / FRAME, 6);
    const frames = (n: number) => BOX * Math.pow(1 - SLIDE_DRAIN_PER_FRAME, n);
    expect(stepSlide(BOX, 2 * FRAME)).toBeCloseTo(frames(2), 3);
    expect(stepSlide(BOX, MAX_REVEAL_TICK_DT_MS)).toBeCloseTo(frames(SLIDE_MAX_CATCHUP_FRAMES), 3);
    expect(stepSlide(BOX, 120)).toBeCloseTo(frames(SLIDE_MAX_CATCHUP_FRAMES), 3);
    expect(stepSlide(BOX, 5000)).toBeCloseTo(frames(SLIDE_MAX_CATCHUP_FRAMES), 3);
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
  it.each([
    ['matrix(1, 0, 0, 1, 0, 19.5)', 19.5],
    ['matrix3d(1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 42.25, 0, 1)', 42.25],
    // none, empty and unparseable read as zero.
    ['none', 0],
    ['', 0],
    ['translateY(10px)', 0],
    ['matrix(garbage', 0],
  ])('reads %j as translateY %d', (transform, ty) => {
    expect(transformTranslateY(transform)).toBe(ty);
  });
});
