// Even motion-floor cadences in measured grid pixels per displayed frame.
// A value >= 1 is that many pixels every frame; a fraction is one pixel
// every reciprocal frames. Returning a scalar keeps spring ticks allocation-free.
export const SPRING_QUANTIZED_MOTION_FLOOR_PX_PER_FRAME = 1;
const SPRING_FLOOR_MIN_CHANGES_PER_SECOND = 45;
const SPRING_FLOOR_CADENCE_SLACK = 0.05;

/** Track display cadence across monitor changes without pricing one missed
 * frame as a slower display. State persists across individual chases. */
export function createFrameCadence(): (gapMs: number) => number | null {
  let average: number | null = null;
  let slowerGap = 0;
  let slowerSamples = 0;
  return (gapMs) => {
    // 20–1000Hz includes low-refresh panels and high-refresh desktop modes.
    // Larger gaps are suspension/stalls, not a useful display-clock sample.
    if (!Number.isFinite(gapMs) || gapMs < 1 || gapMs > 50) {
      slowerSamples = 0;
      return average;
    }
    if (average !== null && gapMs > average * 1.5) {
      slowerSamples = Math.abs(gapMs - slowerGap) <= gapMs * 0.1 ? slowerSamples + 1 : 1;
      slowerGap = gapMs;
      if (slowerSamples < 3) return average;
      average = gapMs;
    } else {
      average = average === null ? gapMs : average + (gapMs - average) * 0.15;
    }
    slowerSamples = 0;
    return average;
  };
}

export function quantizedFloorStep(
  gridPixelsPerCssPixel: number,
  frameFraction: number,
): number {
  const reference = gridPixelsPerCssPixel * frameFraction * SPRING_QUANTIZED_MOTION_FLOOR_PX_PER_FRAME;
  if (reference >= 1) {
    const low = Math.floor(reference);
    const pxPerEvent = reference / low <= (low + 1) / reference ? low : low + 1;
    return pxPerEvent;
  }
  // One pixel every k frames: k = floor(1/reference) runs at or above the
  // reference, k + 1 below it, and neither slower than the cadence bound
  // (k displayed frames per change ≤ 60Hz frames per change at the bound).
  const maxFrames = Math.max(
    1,
    Math.floor(
      ((60 / SPRING_FLOOR_MIN_CHANGES_PER_SECOND) * (1 + SPRING_FLOOR_CADENCE_SLACK))
        / frameFraction,
    ),
  );
  const fast = Math.min(maxFrames, Math.max(1, Math.floor(1 / reference)));
  const slow = fast + 1;
  const framesPerEvent =
    slow <= maxFrames && reference * slow < 1 / (fast * reference) ? slow : fast;
  return 1 / framesPerEvent;
}
