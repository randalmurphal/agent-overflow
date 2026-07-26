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

  it('puts the thumb at the end of its travel when scrolled to the bottom', () => {
    const thumb = thumbMetrics(metrics(300, 100, 400), 200);

    expect(thumb.topPx).toBe(150); // track 200 - thumb 50
  });

  it('maps a mid-scroll position proportionally', () => {
    const thumb = thumbMetrics(metrics(150, 100, 400), 200);

    expect(thumb.topPx).toBe(75); // half of 150px of travel
  });

  it('hides itself when there is nothing to scroll', () => {
    expect(thumbMetrics(metrics(0, 400, 400), 200).visible).toBe(false);
  });

  it('treats sub-pixel overflow as nothing to scroll', () => {
    // Rounding noise from a fractional row height must not raise a bar.
    expect(thumbMetrics(metrics(0, 400, 400.6), 200).visible).toBe(false);
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

  it('reports nothing for a track with no height yet', () => {
    expect(thumbMetrics(metrics(0, 100, 400), 0).visible).toBe(false);
  });
});

describe('scrollTopForDrag', () => {
  const origin = { scrollTop: 0, pointerY: 500 };

  it('maps pointer travel to content travel', () => {
    // 150px of thumb travel spans 300px of scroll, so 1px of pointer is 2px
    // of content.
    const next = scrollTopForDrag(origin, 520, metrics(0, 100, 400), 200);

    expect(next).toBe(40);
  });

  it('lands exactly at the bottom when the thumb reaches the end of the track', () => {
    const next = scrollTopForDrag(origin, 500 + 150, metrics(0, 100, 400), 200);

    expect(next).toBe(300);
  });

  it('lands exactly at the bottom even with a min-clamped thumb', () => {
    // The naive scrollHeight/clientHeight scale would overshoot badly here;
    // only the inverse of the thumb mapping lines up.
    const long = metrics(0, 100, 100_000);
    const travel = 200 - MIN_THUMB_PX;

    expect(scrollTopForDrag(origin, 500 + travel, long, 200)).toBe(99_900);
  });

  it('clamps at both ends', () => {
    expect(scrollTopForDrag(origin, 100, metrics(0, 100, 400), 200)).toBe(0);
    expect(scrollTopForDrag(origin, 9_000, metrics(0, 100, 400), 200)).toBe(300);
  });

  it('drags relative to where the grab started, not the top', () => {
    const grabbed = { scrollTop: 150, pointerY: 500 };

    expect(scrollTopForDrag(grabbed, 510, metrics(150, 100, 400), 200)).toBe(170);
  });

  it('is inert when there is nothing to scroll', () => {
    const grabbed = { scrollTop: 0, pointerY: 500 };

    expect(scrollTopForDrag(grabbed, 600, metrics(0, 400, 400), 200)).toBe(0);
  });
});

describe('scrollTopForTrackClick', () => {
  // Thumb occupies 0..50 of a 200px track at scrollTop 0.
  const surface = metrics(0, 100, 400);

  it('pages down when the click is below the thumb', () => {
    expect(scrollTopForTrackClick(180, surface, 200)).toBe(100);
  });

  it('pages up when the click is above the thumb', () => {
    expect(scrollTopForTrackClick(10, metrics(200, 100, 400), 200)).toBe(100);
  });

  it('does nothing when the click lands on the thumb — that gesture is a drag', () => {
    expect(scrollTopForTrackClick(25, surface, 200)).toBe(0);
  });

  it('clamps a page against the end of the content', () => {
    expect(scrollTopForTrackClick(199, metrics(250, 100, 400), 200)).toBe(300);
  });

  it('is inert when there is nothing to scroll', () => {
    expect(scrollTopForTrackClick(180, metrics(0, 400, 400), 200)).toBe(0);
  });
});

describe('scrollTopForWheel', () => {
  const surface = metrics(100, 100, 400);

  it('applies a pixel notch as it arrives', () => {
    expect(scrollTopForWheel(surface, 60, 0)).toBe(160);
    expect(scrollTopForWheel(surface, -60, 0)).toBe(40);
  });

  it('reads a line notch as lines', () => {
    // A raw 3 would move the surface by three pixels — a wheel that does
    // nothing. Devices reporting lines are rare but real.
    expect(scrollTopForWheel(surface, 3, 1)).toBe(148);
  });

  it('reads a page notch as a viewport', () => {
    expect(scrollTopForWheel(surface, 1, 2)).toBe(200);
  });

  it('clamps both ends, because nothing else will', () => {
    // The browser clamps the surfaces it scrolls itself; this one it never
    // touches, so an overshoot would land an out-of-range scrollTop and the
    // at-bottom report that follows it would be reasoning about that value.
    expect(scrollTopForWheel(surface, 9_000, 0)).toBe(300);
    expect(scrollTopForWheel(surface, -9_000, 0)).toBe(0);
  });
});
