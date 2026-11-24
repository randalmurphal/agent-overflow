import { describe, expect, it } from 'vitest';
import {
  MIN_THUMB_PX,
  scrollTopForDrag,
  scrollTopForTrackClick,
  scrollTopForWheel,
  thumbMetrics,
  type ScrollMetrics,
} from './overlayScrollbar';

/** A surface showing `clientHeight` of `scrollHeight`, scrolled to `scrollTop`. */
function metrics(scrollTop: number, clientHeight: number, scrollHeight: number): ScrollMetrics {
  return { scrollTop, clientHeight, scrollHeight };
}

describe('thumbMetrics', () => {
  it('sizes the thumb by the visible fraction', () => {
    const thumb = thumbMetrics(metrics(0, 100, 400), 200);

    expect(thumb.visible).toBe(true);
    expect(thumb.heightPx).toBe(50); // 100/400 of a 200px track
    expect(thumb.topPx).toBe(0);
  });

  it.each([
    // track 200 - thumb 50
    ['puts the thumb at the end of its travel when scrolled to the bottom', 300, 150],
    // half of 150px of travel
    ['maps a mid-scroll position proportionally', 150, 75],
  ])('%s', (_name, scrollTop, topPx) => {
    expect(thumbMetrics(metrics(scrollTop, 100, 400), 200).topPx).toBe(topPx);
  });

  it.each([
    ['hides itself when there is nothing to scroll', metrics(0, 400, 400), 200],
    // Rounding noise from a fractional row height must not raise a bar.
    ['treats sub-pixel overflow as nothing to scroll', metrics(0, 400, 400.6), 200],
    ['reports nothing for a track with no height yet', metrics(0, 100, 400), 0],
  ])('%s', (_name, surface, trackPx) => {
    expect(thumbMetrics(surface, trackPx).visible).toBe(false);
  });

  it('holds a grabbable minimum on a very long run', () => {
    const thumb = thumbMetrics(metrics(0, 100, 100_000), 200);

    expect(thumb.heightPx).toBe(MIN_THUMB_PX);
  });

  it('never renders a thumb taller than its track', () => {
    // Track shorter than the minimum: the clamp must not invert.
    const thumb = thumbMetrics(metrics(0, 100, 100_000), 12);

    expect(thumb.heightPx).toBe(12);
    expect(thumb.topPx).toBe(0);
  });
});

describe('scrollTopForDrag', () => {
  const origin = { scrollTop: 0, pointerY: 500 };
  const surface = metrics(0, 100, 400);

  it.each([
    // 150px of thumb travel spans 300px of scroll, so 1px of pointer is 2px
    // of content.
    ['maps pointer travel to content travel', origin, 520, surface, 40],
    ['lands exactly at the bottom when the thumb reaches the end of the track', origin, 500 + 150, surface, 300],
    // The naive scrollHeight/clientHeight scale would overshoot badly here;
    // only the inverse of the thumb mapping lines up.
    ['lands exactly at the bottom even with a min-clamped thumb', origin, 500 + (200 - MIN_THUMB_PX), metrics(0, 100, 100_000), 99_900],
    ['clamps at the top', origin, 100, surface, 0],
    ['clamps at the bottom', origin, 9_000, surface, 300],
    ['drags relative to where the grab started, not the top', { scrollTop: 150, pointerY: 500 }, 510, metrics(150, 100, 400), 170],
    ['is inert when there is nothing to scroll', origin, 600, metrics(0, 400, 400), 0],
  ])('%s', (_name, grab, pointerY, dragged, expected) => {
    expect(scrollTopForDrag(grab, pointerY, dragged, 200)).toBe(expected);
  });
});

describe('scrollTopForTrackClick', () => {
  // Thumb occupies 0..50 of a 200px track at scrollTop 0.
  const surface = metrics(0, 100, 400);

  it.each([
    ['pages down when the click is below the thumb', 180, surface, 100],
    ['pages up when the click is above the thumb', 10, metrics(200, 100, 400), 100],
    ['does nothing when the click lands on the thumb — that gesture is a drag', 25, surface, 0],
    ['clamps a page against the end of the content', 199, metrics(250, 100, 400), 300],
    ['is inert when there is nothing to scroll', 180, metrics(0, 400, 400), 0],
  ])('%s', (_name, clickY, clicked, expected) => {
    expect(scrollTopForTrackClick(clickY, clicked, 200)).toBe(expected);
  });
});

describe('scrollTopForWheel', () => {
  const surface = metrics(100, 100, 400);

  it.each([
    ['applies a downward pixel notch as it arrives', 60, 0, 160],
    ['applies an upward pixel notch as it arrives', -60, 0, 40],
    // A raw 3 would move the surface by three pixels — a wheel that does
    // nothing. Devices reporting lines are rare but real.
    ['reads a line notch as lines', 3, 1, 148],
    ['reads a page notch as a viewport', 1, 2, 200],
    // The browser clamps the surfaces it scrolls itself; this one it never
    // touches, so an overshoot would land an out-of-range scrollTop and the
    // at-bottom report that follows it would be reasoning about that value.
    ['clamps the bottom, because nothing else will', 9_000, 0, 300],
    ['clamps the top, because nothing else will', -9_000, 0, 0],
  ])('%s', (_name, delta, mode, expected) => {
    expect(scrollTopForWheel(surface, delta, mode)).toBe(expected);
  });
});
