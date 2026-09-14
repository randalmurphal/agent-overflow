import { MedianWindow } from './cadence';

/** Cadence facts for one spring chase, in the units a reader can act on:
 * delivered frames, not callback time; grid quanta, not CSS pixels. */
export interface ChaseCadenceStats {
  ticks: number;
  writes: number;
  /** Frames the display presented without a tick, summed over holes. */
  droppedFrames: number;
  /** Longest hole, in frames including the tick that ended it. */
  maxHoleFrames: number;
  /** Ticks whose callback ran more than half a frame after its timestamp. */
  lateTicks: number;
  /** Writes whose tick interval since the previous write differs from the interval before it. */
  unevenWrites: number;
  /** Ticks whose grid step differs from the previous tick's by two or more quanta. */
  stepJumps: number;
  /** Ticks stepped by whole delivered frames under a foreign timestamp clock. */
  fallbackTicks: number;
}

export interface GlideTotals extends ChaseCadenceStats {
  chases: number;
}

function zeroTotals(): GlideTotals {
  return {
    chases: 0, ticks: 0, writes: 0, droppedFrames: 0, maxHoleFrames: 0,
    lateTicks: 0, unevenWrites: 0, stepJumps: 0, fallbackTicks: 0,
  };
}

/** One record per tick; the arrays are what a trace reader replays. */
export class ChaseCadence implements ChaseCadenceStats {
  ticks = 0;
  writes = 0;
  droppedFrames = 0;
  maxHoleFrames = 0;
  lateTicks = 0;
  unevenWrites = 0;
  stepJumps = 0;
  fallbackTicks = 0;
  private readonly period = new MedianWindow();
  private lastWriteTick = -1;
  private lastWriteInterval = 0;
  private previousStep = 0;

  /** Delivered frame period so far, 0 until measured. */
  get periodMs(): number { return this.period.median; }

  /** Returns the delivered frames this tick spans (1 when unknown). */
  tick(frameGapMs: number | null, lateMs: number, stepQuanta: number, wrote: boolean, fallback: boolean): number {
    this.ticks += 1;
    let frames = 1;
    if (frameGapMs !== null && frameGapMs >= 1 && frameGapMs <= 50) this.period.push(frameGapMs);
    const period = this.period.median;
    if (frameGapMs !== null && frameGapMs > 0 && period > 0) {
      frames = Math.max(1, Math.round(frameGapMs / period));
      if (frames >= 2) {
        this.droppedFrames += frames - 1;
        if (frames > this.maxHoleFrames) this.maxHoleFrames = frames;
      }
    }
    if (period > 0 && lateMs > period / 2) this.lateTicks += 1;
    if (wrote) {
      this.writes += 1;
      const interval = this.ticks - this.lastWriteTick;
      if (this.lastWriteTick >= 0 && this.lastWriteInterval > 0 && interval !== this.lastWriteInterval) {
        this.unevenWrites += 1;
      }
      if (this.lastWriteTick >= 0) this.lastWriteInterval = interval;
      this.lastWriteTick = this.ticks;
    }
    if (Math.abs(stepQuanta - this.previousStep) >= 2) this.stepJumps += 1;
    this.previousStep = stepQuanta;
    if (fallback) this.fallbackTicks += 1;
    return frames;
  }
}

// Page-wide totals the harness perf meter reads; a chase folds in at its end.
let totals = zeroTotals();
let runMaxHoleFrames = 0;

export function foldGlideChase(chase: ChaseCadenceStats): void {
  if (chase.ticks === 0) return;
  totals.chases += 1;
  totals.ticks += chase.ticks;
  totals.writes += chase.writes;
  totals.droppedFrames += chase.droppedFrames;
  totals.lateTicks += chase.lateTicks;
  totals.unevenWrites += chase.unevenWrites;
  totals.stepJumps += chase.stepJumps;
  totals.fallbackTicks += chase.fallbackTicks;
  if (chase.maxHoleFrames > totals.maxHoleFrames) totals.maxHoleFrames = chase.maxHoleFrames;
  if (chase.maxHoleFrames > runMaxHoleFrames) runMaxHoleFrames = chase.maxHoleFrames;
}

/** Snapshot at the start of a perf run; `readGlideRun` reports against it. */
export function beginGlideRun(): GlideTotals {
  runMaxHoleFrames = 0;
  return { ...totals };
}

export function readGlideRun(start: GlideTotals): GlideTotals {
  return {
    chases: totals.chases - start.chases,
    ticks: totals.ticks - start.ticks,
    writes: totals.writes - start.writes,
    droppedFrames: totals.droppedFrames - start.droppedFrames,
    maxHoleFrames: runMaxHoleFrames,
    lateTicks: totals.lateTicks - start.lateTicks,
    unevenWrites: totals.unevenWrites - start.unevenWrites,
    stepJumps: totals.stepJumps - start.stepJumps,
    fallbackTicks: totals.fallbackTicks - start.fallbackTicks,
  };
}

export function __resetGlideTotalsForTest(): void {
  totals = zeroTotals();
  runMaxHoleFrames = 0;
}
