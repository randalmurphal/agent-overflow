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

// A browser can present on one display while its animation timestamps follow
// another. Evidence of a foreign clock (a repeated timestamp, or a timestamp
// delta whose frame count disagrees with callback time) holds whole-frame
// stepping past the longest beat between such clocks (144Hz timestamps on a
// 165Hz display repeat one every 8 frames); an isolated glitch releases fast.
const CLOCK_MISMATCH_EVIDENCE_TICKS = 8;
const CLOCK_MISMATCH_HOLD_TICKS = 64;
// Recent-window medians: the timestamp unit follows a refresh-rate change
// within the window, and the delivered period ignores symmetric callback
// jitter and occasional dropped frames where an average drifts.
const PERIOD_WINDOW = 32;
const PERIOD_REFRESH_TICKS = 8;
const PERIOD_WARMUP_SAMPLES = 8;

class MedianWindow {
  private readonly values = new Float64Array(PERIOD_WINDOW);
  private readonly sorted = new Float64Array(PERIOD_WINDOW);
  private samples = 0;
  median = 0;

  push(value: number): void {
    this.values[this.samples % PERIOD_WINDOW] = value;
    this.samples += 1;
    if (this.samples < PERIOD_WARMUP_SAMPLES) return;
    if (this.median !== 0 && this.samples % PERIOD_REFRESH_TICKS !== 0) return;
    const count = Math.min(this.samples, PERIOD_WINDOW);
    this.sorted.set(this.values.subarray(0, count));
    this.sorted.subarray(0, count).sort();
    this.median = this.sorted[count >> 1];
  }
}

// Callback time may land most of a frame early or late; only a timestamp
// that disagrees with it by more than this many frames is foreign evidence.
const FRAME_DISAGREEMENT = 0.75;

/** Elapsed animation time for one tick. Frame timestamps are vsync-aligned
 * while callback time jitters with main-thread scheduling, so the timestamp
 * delta is the step whenever the timestamp clock tracks delivery. Under a
 * foreign clock the step is whole delivered frames, counted from the
 * timestamp when it advanced and from callback time when it repeated. */
export function createFrameStep(): (elapsedMs: number, presentedMs: number) => number {
  const unit = new MedianWindow();
  const delivered = new MedianWindow();
  let mismatchTicks = 0;
  return (elapsedMs, presentedMs) => {
    if (!Number.isFinite(elapsedMs) || !Number.isFinite(presentedMs)) throw new RangeError('Frame step requires finite times');
    // Gaps beyond 50ms are stalls or suspension, not display cadence.
    if (presentedMs >= 1 && presentedMs <= 50) unit.push(presentedMs);
    if (elapsedMs >= 1 && elapsedMs <= 50) delivered.push(elapsedMs);
    if (unit.median === 0 || delivered.median === 0) return presentedMs > 0 ? presentedMs : Math.max(elapsedMs, 0);
    const presentedFrames = Math.round(presentedMs / unit.median);
    // A timestamp under one unit can only be a faster refresh rate; follow it.
    if (presentedMs > 0 && presentedFrames === 0) return presentedMs;
    const elapsedFrames = elapsedMs / unit.median;
    const evidence = presentedMs <= 0
      || (Math.min(presentedFrames, elapsedFrames) <= 2.5
        && Math.abs(presentedFrames - elapsedFrames) > FRAME_DISAGREEMENT);
    if (evidence) mismatchTicks = Math.min(CLOCK_MISMATCH_HOLD_TICKS, mismatchTicks + CLOCK_MISMATCH_EVIDENCE_TICKS);
    else if (mismatchTicks > 0) mismatchTicks -= 1;
    if (mismatchTicks === 0) return presentedMs;
    const deliveredFrames = Math.max(1, Math.round(elapsedFrames));
    const frames = presentedMs <= 0 ? deliveredFrames : Math.min(presentedFrames, deliveredFrames);
    return Math.max(1, frames) * delivered.median;
  };
}
