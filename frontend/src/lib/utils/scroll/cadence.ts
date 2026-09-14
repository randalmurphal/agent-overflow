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
// another. The signature is in the clocks themselves: a repeated timestamp,
// or a timestamp unit that runs a different rate from the delivered period
// (144Hz on 165Hz differs by 13%). Callback lateness under load moves single
// gaps, never the medians, so load is not mistaken for a foreign clock.
// Evidence holds whole-frame stepping past the longest beat between such
// clocks; an isolated glitch releases fast.
const CLOCK_MISMATCH_EVIDENCE_TICKS = 8;
const CLOCK_MISMATCH_HOLD_TICKS = 64;
const CLOCK_RATE_TOLERANCE = 0.08;
// Recent-window medians: the timestamp unit follows a refresh-rate change
// within the window, and the delivered period ignores symmetric callback
// jitter and occasional dropped frames where an average drifts.
const PERIOD_WINDOW = 32;
const PERIOD_REFRESH_TICKS = 8;
const PERIOD_WARMUP_SAMPLES = 8;

export class MedianWindow {
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


/** Elapsed animation time for one tick. Frame timestamps are vsync-aligned
 * while callback time jitters with main-thread scheduling, so the timestamp
 * delta is the step whenever the timestamp clock tracks delivery. Under a
 * foreign clock the step is whole delivered frames, counted from the
 * timestamp when it advanced and from callback time when it repeated. */
export interface FrameStep {
  step(elapsedMs: number, presentedMs: number): number;
  /** True while the last step fell back to whole delivered frames. */
  fallbackActive(): boolean;
}

export function createFrameStep(): FrameStep {
  const unit = new MedianWindow();
  const delivered = new MedianWindow();
  let mismatchTicks = 0;
  const step = (elapsedMs: number, presentedMs: number): number => {
    if (!Number.isFinite(elapsedMs) || !Number.isFinite(presentedMs)) throw new RangeError('Frame step requires finite times');
    // Gaps beyond 50ms are stalls or suspension, not display cadence.
    if (presentedMs >= 1 && presentedMs <= 50) unit.push(presentedMs);
    if (elapsedMs >= 1 && elapsedMs <= 50) delivered.push(elapsedMs);
    if (unit.median === 0 || delivered.median === 0) return presentedMs > 0 ? presentedMs : Math.max(elapsedMs, 0);
    const presentedFrames = Math.round(presentedMs / unit.median);
    // A timestamp under one unit can only be a faster refresh rate; follow it.
    if (presentedMs > 0 && presentedFrames === 0) return presentedMs;
    const evidence = presentedMs <= 0
      || Math.abs(unit.median - delivered.median) > delivered.median * CLOCK_RATE_TOLERANCE;
    if (evidence) mismatchTicks = Math.min(CLOCK_MISMATCH_HOLD_TICKS, mismatchTicks + CLOCK_MISMATCH_EVIDENCE_TICKS);
    else if (mismatchTicks > 0) mismatchTicks -= 1;
    if (mismatchTicks === 0) return presentedMs;
    const deliveredFrames = Math.max(1, Math.round(elapsedMs / unit.median));
    const frames = presentedMs <= 0 ? deliveredFrames : Math.min(presentedFrames, deliveredFrames);
    return Math.max(1, frames) * delivered.median;
  };
  return { step, fallbackActive: () => mismatchTicks > 0 };
}
