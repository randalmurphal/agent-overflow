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
