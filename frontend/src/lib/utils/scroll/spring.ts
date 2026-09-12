// Chase lifecycle, geometry arbitration and recovery. Continuous motion lives in motion.ts.
import { ARRIVAL_DISTANCE_PX, springGateIsOpen, withinArrivalBand } from './resolver';
import { installDocumentResumeTracking, msSinceDocumentResume } from './documentResume';
import { nowMs } from './time';
import { trace } from './trace';
import { SpringMotion, SPRING_MAX_CATCHUP_STEPS } from './motion';
import { isUiRenderTraceEnabled } from '../uiRenderTrace';
import {
  __resetAnimationFrameCoordinatorForTest,
  createAnimationFrameBatcher,
} from '../animationFrameBatcher';
import type { ScrollWriteCaller } from './types';
import type { SpringChase, SpringChaseDeps } from './springTypes';
import { createFrameCadence } from './cadence';
import { scrollReadbackTolerance } from './position';

const SIXTY_FPS_INTERVAL_MS = 1000 / 60;
const SPRING_STALL_TICK_FRAMES = 3;
export const RETAIN_ANIMATION_DURATION_MS = 350;
const ARRIVAL_VELOCITY_THRESHOLD = 0.5;
const SPRING_MAX_CHASE_DISTANCE_VIEWPORTS = 1;
const STALL_RESUME_GAP_MS = 1000;
const RESUME_CLAMP_WINDOW_MS = 2000;

export const SPRING_WRITE_REFUSAL_LATCH_TICKS = 5;
export const SPRING_WRITE_REFUSAL_RETRY_INTERVAL_MS = 250;

const STRUCTURAL_APPEND_SPRING_WINDOW_MS = 250;

const CHASE_GAP_BUCKET_BOUNDS_MS = [9, 13, 18, 26, 42] as const;

interface ChaseTelemetry {
  startedAt: number;
  ticks: number;
  writeTicks: number;
  zeroStepTicks: number;
  sentinelTicks: number;
  maxGapMs: number;
  gapBuckets: number[];
  catchupClamps: number;
  distanceJumps: number;
  targetChanges: number;
  sentinelEntries: number;
  longTasks: number;
  longTaskMs: number;
  refusedWrites: number;
  selectionPausedTicks: number;
}

let springFrameBatcher = createAnimationFrameBatcher(
  'scroll-spring',
  'before-dom-update',
);

export function __resetSpringFrameBatcherForTest(): void {
  __resetAnimationFrameCoordinatorForTest();
  springFrameBatcher = createAnimationFrameBatcher(
    'scroll-spring',
    'before-dom-update',
  );
}

function requestFrame(callback: FrameRequestCallback): number {
  return typeof requestAnimationFrame === 'function' && typeof cancelAnimationFrame === 'function'
    ? springFrameBatcher.request(callback)
    : window.setTimeout(() => callback(nowMs()), 0);
}

function cancelFrame(handle: number): void {
  if (typeof requestAnimationFrame === 'function' && typeof cancelAnimationFrame === 'function') {
    springFrameBatcher.cancel(handle);
  } else {
    window.clearTimeout(handle);
  }
}

export function createSpringChase(deps: SpringChaseDeps): SpringChase {
  installDocumentResumeTracking();
  const motion = new SpringMotion();
  let lastTickAt: number | null = null;
  let selectionPauseTraced = false;
  const sampleFrameCadence = createFrameCadence();
  let frameIntervalEmaMs: number | null = null;
  let springToken = 0;
  let springGen = 0;
  let springFrameHandle: number | null = null;
  let lastTargetChangedAt = 0;
  let springStopRequested = false;
  let structuralAppendSpringUntil = 0;
  let springStartedFromStructuralAppend = false;
  let sentinelEntryTarget = -1;
  let sentinelEntryClientHeight = 0;
  let sentinelClampWitnessed = false;
  let consecutiveRefusedWrites = 0;
  let writeRefusalLatched = false;
  let refusalLatchedAt = 0;
  let lastRefusalRetryAt = 0;

  function witnessClampIfUnexplained(): boolean {
    if (sentinelEntryTarget < 0) return false;
    if (!sentinelClampWitnessed && deps.scrollTopUnexplained()) {
      sentinelClampWitnessed = true;
    }
    return sentinelClampWitnessed;
  }

  function rebasedSentinelEntryTarget(clientHeightNow: number): number {
    if (sentinelEntryTarget < 0) return -1;
    return sentinelEntryTarget + (sentinelEntryClientHeight - clientHeightNow);
  }
  let lastChaseTarget = -1;
  let chaseTelemetry: ChaseTelemetry | null = null;
  let longTaskObserver: PerformanceObserver | null = null;

  function beginChaseTelemetry(): void {
    if (!isUiRenderTraceEnabled()) return;
    chaseTelemetry = {
      startedAt: nowMs(),
      ticks: 0,
      writeTicks: 0,
      zeroStepTicks: 0,
      sentinelTicks: 0,
      maxGapMs: 0,
      gapBuckets: new Array<number>(CHASE_GAP_BUCKET_BOUNDS_MS.length + 1).fill(0),
      catchupClamps: 0,
      distanceJumps: 0,
      targetChanges: 0,
      sentinelEntries: 0,
      longTasks: 0,
      longTaskMs: 0,
      refusedWrites: 0,
      selectionPausedTicks: 0,
    };
    if (
      typeof PerformanceObserver !== 'undefined'
      && (PerformanceObserver.supportedEntryTypes ?? []).includes('longtask')
    ) {
      try {
        const observer = new PerformanceObserver((list) => {
          const stats = chaseTelemetry;
          if (!stats) return;
          for (const entry of list.getEntries()) {
            stats.longTasks += 1;
            stats.longTaskMs += entry.duration;
          }
        });
        observer.observe({ type: 'longtask' });
        longTaskObserver = observer;
      } catch (error) {
        console.error('Could not observe scroll chase long tasks', error);
        longTaskObserver = null;
      }
    }
  }

  function recordChaseFrame(now: number, previousTickAt: number | null, dtFrames: number): void {
    const stats = chaseTelemetry;
    if (!stats) return;
    stats.ticks += 1;
    if (dtFrames > SPRING_STALL_TICK_FRAMES) stats.catchupClamps += 1;
    if (previousTickAt === null) return;
    const gapMs = now - previousTickAt;
    if (gapMs > stats.maxGapMs) stats.maxGapMs = gapMs;
    let bucket: number = CHASE_GAP_BUCKET_BOUNDS_MS.length;
    for (let i = 0; i < CHASE_GAP_BUCKET_BOUNDS_MS.length; i++) {
      if (gapMs < CHASE_GAP_BUCKET_BOUNDS_MS[i]) {
        bucket = i;
        break;
      }
    }
    stats.gapBuckets[bucket] += 1;
  }

  function endChaseTelemetry(): void {
    const stats = chaseTelemetry;
    chaseTelemetry = null;
    if (longTaskObserver) {
      longTaskObserver.disconnect();
      longTaskObserver = null;
    }
    if (!stats || !isUiRenderTraceEnabled()) return;
    if (stats.ticks < 3 && stats.selectionPausedTicks === 0) return;
    trace('scroll.spring.chase', () => ({
      durationMs: Math.round(nowMs() - stats.startedAt),
      ticks: stats.ticks,
      writeTicks: stats.writeTicks,
      zeroStepTicks: stats.zeroStepTicks,
      sentinelTicks: stats.sentinelTicks,
      maxGapMs: Math.round(stats.maxGapMs * 10) / 10,
      gapBuckets: stats.gapBuckets,
      cadenceEmaMs:
        frameIntervalEmaMs === null ? null : Math.round(frameIntervalEmaMs * 100) / 100,
      catchupClamps: stats.catchupClamps,
      distanceJumps: stats.distanceJumps,
      targetChanges: stats.targetChanges,
      sentinelEntries: stats.sentinelEntries,
      longTasks: stats.longTasks,
      longTaskMs: Math.round(stats.longTaskMs),
      refusedWrites: stats.refusedWrites,
      selectionPausedTicks: stats.selectionPausedTicks,
    }));
  }

  function cancel(): void {
    if (springFrameHandle !== null) {
      cancelFrame(springFrameHandle);
      springFrameHandle = null;
    }
    springToken = 0;
    motion.reset();
    lastTickAt = null;
    selectionPauseTraced = false;
    deps.arrival.clear();
    springStartedFromStructuralAppend = false;
    lastTargetChangedAt = 0;
    sentinelEntryTarget = -1;
    sentinelClampWitnessed = false;
    if (writeRefusalLatched) {
      const el = deps.getScrollEl();
      deps.reportWriteRefusal({
        phase: 'abandoned',
        consecutiveRefusals: consecutiveRefusedWrites,
        requested: -1,
        scrollTop: el ? el.scrollTop : -1,
        target: el ? deps.targetScrollTop() : -1,
        wedgeMs: Math.round(nowMs() - refusalLatchedAt),
      });
    }
    consecutiveRefusedWrites = 0;
    writeRefusalLatched = false;
    refusalLatchedAt = 0;
    lastRefusalRetryAt = 0;
    lastChaseTarget = -1;
    endChaseTelemetry();
  }

  function snapOscillationToBottom(caller: ScrollWriteCaller, top: number): void {
    deps.writeScrollTop(caller, top, top);
    motion.reset();
    sentinelEntryTarget = -1;
    sentinelClampWitnessed = false;
  }

  function gateOpen(): boolean {
    return springGateIsOpen({
      springStopRequested,
      paused: deps.isPaused(),
      isAtBottom: deps.isAtBottom(),
      escaped: deps.isEscaped(),
      prefersReducedMotion: deps.prefersReducedMotion(),
    });
  }

  function writeMotion(current: number, position: number, request: number, target: number, overshot: boolean): number {
    const now = nowMs();
    if (chaseTelemetry) chaseTelemetry.zeroStepTicks -= 1;
    const postWriteTop = deps.writeScrollTop(
      overshot ? 'spring.overshoot' : 'spring.tick', request, target,
    ) ?? current;
    if (chaseTelemetry) chaseTelemetry.writeTicks += 1;
    if (position === target) deps.arrival.record(target);
    if (postWriteTop !== current) {
      if (writeRefusalLatched) {
        deps.reportWriteRefusal({
          phase: 'healed',
          consecutiveRefusals: consecutiveRefusedWrites,
          requested: request,
          scrollTop: postWriteTop,
          target,
          wedgeMs: Math.round(now - refusalLatchedAt),
        });
        writeRefusalLatched = false;
        refusalLatchedAt = 0;
        lastRefusalRetryAt = 0;
      }
      consecutiveRefusedWrites = 0;
    } else if (Math.abs(position - current) >= deps.scrollGrid().quantum * (1 - 1e-6) && Math.abs(target - current) > ARRIVAL_DISTANCE_PX) {
      consecutiveRefusedWrites += 1;
      if (chaseTelemetry) chaseTelemetry.refusedWrites += 1;
      if (
        !writeRefusalLatched
        && consecutiveRefusedWrites >= SPRING_WRITE_REFUSAL_LATCH_TICKS
      ) {
        writeRefusalLatched = true;
        refusalLatchedAt = now;
        lastRefusalRetryAt = now;
        deps.reportWriteRefusal({
          phase: 'latched',
          consecutiveRefusals: consecutiveRefusedWrites,
          requested: request,
          scrollTop: postWriteTop,
          target,
          wedgeMs: 0,
        });
      }
    }
    return postWriteTop;
  }

  function start(): void {
    if (springToken !== 0) return;
    if (!gateOpen()) return;
    springStartedFromStructuralAppend =
      !deps.liveContentActive()
      && structuralAppendSpringUntil > nowMs();
    const myToken = ++springGen;
    springToken = myToken;
    lastTickAt = null;
    deps.forceNextSpringTickTrace();
    beginChaseTelemetry();

    const tick = (): void => {
      if (springToken !== myToken) return;
      // A browser can deliver frames on this display while its animation
      // timestamps follow another display. Integrate actual elapsed time.
      const now = nowMs();
      springFrameHandle = null;
      const el = deps.getScrollEl();
      if (!el) {
        cancel();
        return;
      }
      if (springStopRequested || deps.isPaused() || !deps.isAtBottom() || deps.isEscaped()) {
        cancel();
        return;
      }
      if (deps.selectionActive()) {
        lastTickAt = now;
        if (chaseTelemetry) chaseTelemetry.selectionPausedTicks += 1;
        if (!selectionPauseTraced) {
          selectionPauseTraced = true;
          if (isUiRenderTraceEnabled()) trace('scroll.spring.selectionPause', () => ({
            scrollTop: Math.round(el.scrollTop),
            target: Math.round(deps.targetScrollTop()),
          }));
        }
        springFrameHandle = requestFrame(tick);
        return;
      }
      selectionPauseTraced = false;

      const previousTickAt = lastTickAt;
      const rawDtFrames =
        previousTickAt === null ? 1 : (now - previousTickAt) / SIXTY_FPS_INTERVAL_MS;
      const dtFrames = Number.isFinite(rawDtFrames) ? Math.max(rawDtFrames, 0) : 1;
      lastTickAt = now;
      if (previousTickAt !== null) frameIntervalEmaMs = sampleFrameCadence(now - previousTickAt);
      if (chaseTelemetry) recordChaseFrame(now, previousTickAt, dtFrames);
      const integrationFrames = Math.min(dtFrames, SPRING_MAX_CATCHUP_STEPS);

      if (writeRefusalLatched) {
        if (now - lastRefusalRetryAt < SPRING_WRITE_REFUSAL_RETRY_INTERVAL_MS) {
          motion.decay(dtFrames);
          springFrameHandle = requestFrame(tick);
          return;
        }
        lastRefusalRetryAt = now;
      }

      const grid = deps.scrollGrid();
      const forceGeometryRead = sentinelEntryTarget >= 0 || writeRefusalLatched;
      const target = deps.targetScrollTop(forceGeometryRead);
      let current = deps.currentScrollTop(forceGeometryRead);
      deps.arrival.invalidateStale(target);
      witnessClampIfUnexplained();

      if (deps.prefersReducedMotion()) {
        if (deps.arrival.shouldWriteExact(target)) {
          deps.arrival.writeExact('spring.arrive', target);
        }
        cancel();
        return;
      }

      if (chaseTelemetry) {
        if (lastChaseTarget >= 0 && target !== lastChaseTarget) {
          chaseTelemetry.targetChanges += 1;
        }
        lastChaseTarget = target;
      }

      const wantsStreamingSpringNow = deps.liveContentActive();
      const wantsSpringNow = wantsStreamingSpringNow || springStartedFromStructuralAppend;
      const withinTargetChangeRetainWindow =
        wantsSpringNow && now - lastTargetChangedAt < RETAIN_ANIMATION_DURATION_MS;

      if (current !== target && !deps.arrival.matches(target)) {
        if (
          sentinelEntryTarget >= 0
          && withinArrivalBand(target, rebasedSentinelEntryTarget(el.clientHeight))
          && witnessClampIfUnexplained()
        ) {
          snapOscillationToBottom('spring.oscillationSnap', target);
        } else {
          deps.arrival.clear();
          sentinelEntryTarget = -1;
          sentinelClampWitnessed = false;
          const tickGapMs = previousTickAt === null ? 0 : now - previousTickAt;
          const resumedFromDiscontinuity =
            tickGapMs >= STALL_RESUME_GAP_MS
            || msSinceDocumentResume() <= RESUME_CLAMP_WINDOW_MS;
          if (resumedFromDiscontinuity) {
            const chaseLimitPx = el.clientHeight * SPRING_MAX_CHASE_DISTANCE_VIEWPORTS;
            if (chaseLimitPx > 0 && Math.abs(target - current) > chaseLimitPx) {
              const catchupReadback = deps.writeScrollTop('spring.catchupSnap', target, target);
              if (catchupReadback !== undefined && catchupReadback !== current) {
                current = catchupReadback;
                motion.reset();
                if (chaseTelemetry) chaseTelemetry.distanceJumps += 1;
              }
            }
          }
          if (integrationFrames > 0) {
            if (chaseTelemetry) chaseTelemetry.zeroStepTicks += 1;
            current = motion.step(current, target, integrationFrames, grid, writeMotion);
          }
        }
      } else {
        motion.park(dtFrames, withinTargetChangeRetainWindow);
      }

      const arrived =
        (Math.abs(current - target) <= scrollReadbackTolerance(grid, target)
          || deps.arrival.matches(target))
        && Math.abs(motion.velocity) < ARRIVAL_VELOCITY_THRESHOLD;
      if (arrived && !withinTargetChangeRetainWindow) {
        if (wantsStreamingSpringNow) {
          if (deps.arrival.shouldWriteExact(target)) {
            deps.arrival.writeExact('spring.arrive', target);
          }
          motion.reset();
          if (chaseTelemetry) chaseTelemetry.sentinelTicks += 1;
          if (sentinelEntryTarget < 0) {
            sentinelEntryTarget = target;
            sentinelEntryClientHeight = el.clientHeight;
            sentinelClampWitnessed = false;
            if (chaseTelemetry) chaseTelemetry.sentinelEntries += 1;
          }
          springFrameHandle = requestFrame(tick);
          return;
        }
        if (deps.arrival.shouldWriteExact(target)) {
          deps.arrival.writeExact('spring.arrive', target);
        }
        cancel();
        return;
      }
      springFrameHandle = requestFrame(tick);
    };
    springFrameHandle = requestFrame(tick);
  }

  return {
    start,
    cancel,
    snapOscillationToBottom,
    gateOpen,
    isActive: () => springToken !== 0,
    token: () => springToken,
    velocityForTest: () => motion.velocity,
    requestStop: () => {
      springStopRequested = true;
    },
    clearStopRequest: () => {
      springStopRequested = false;
    },
    stopRequested: () => springStopRequested,
    markTargetChanged: () => {
      lastTargetChangedAt = nowMs();
    },
    markStructuralAppend: () => {
      structuralAppendSpringUntil = nowMs() + STRUCTURAL_APPEND_SPRING_WINDOW_MS;
    },
    structuralAppendPending: () => structuralAppendSpringUntil > nowMs(),
    clearStructuralAppend: () => {
      structuralAppendSpringUntil = 0;
      springStartedFromStructuralAppend = false;
    },
    sentinelTarget: () => {
      if (sentinelEntryTarget < 0) return -1;
      const el = deps.getScrollEl();
      if (!el) return sentinelEntryTarget;
      return rebasedSentinelEntryTarget(el.clientHeight);
    },
    sentinelClampWitnessed: () => witnessClampIfUnexplained(),
    refusalLatched: () => writeRefusalLatched,
  };
}
