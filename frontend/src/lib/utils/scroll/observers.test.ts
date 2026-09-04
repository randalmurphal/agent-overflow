import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createContentObserver, type ContentObserverDeps } from './observers';

function observer() {
  const unused = () => { throw new Error('Unexpected geometry access in resize-clear lifecycle test'); };
  return createContentObserver({
    getScrollEl: unused, getContentEl: unused, hasExternalGeometrySource: () => true,
    liveContentActive: unused, getQuietContextSignal: unused, warm: unused,
    setWarm: () => {}, setWarmReason: () => {}, isAtBottom: unused,
    setIsAtBottom: unused, escaped: unused, pauseDepth: unused, isNearBottom: unused,
    targetScrollTop: unused, refreshIsNearBottom: unused, contentGeometryForSample: unused,
    cacheExternalBottomTarget: unused, writeScrollTop: unused, resolverStateSnapshot: unused,
    prefersReducedMotion: unused, contentGeometryProcessed: unused,
    spring: { structuralAppendPending: unused, snapOscillationToBottom: unused,
      markTargetChanged: unused, start: unused },
  } satisfies ContentObserverDeps);
}

let frames: Map<number, FrameRequestCallback>;
beforeEach(() => {
  vi.useFakeTimers();
  frames = new Map();
  let next = 0;
  vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
    frames.set(++next, callback);
    return next;
  });
  vi.stubGlobal('cancelAnimationFrame', (id: number) => frames.delete(id));
});
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); });
function flushFrames() {
  const callbacks = [...frames.values()];
  frames.clear();
  for (const callback of callbacks) callback(performance.now());
}

describe('resize correlation clear ownership', () => {
  it('clears after the timer and frame, not before either boundary', () => {
    const subject = observer();
    subject.stampSyntheticResizeCorrelation();
    expect(subject.resizeDifferenceNow()).toBe(1);
    vi.runOnlyPendingTimers();
    expect(subject.resizeDifferenceNow()).toBe(1);
    flushFrames();
    expect(subject.resizeDifferenceNow()).toBe(0);
    subject.detach();
  });

  it.each(['timer', 'frame'] as const)('detach cancels the pending %s and can be called twice', (stage) => {
    const subject = observer();
    subject.stampSyntheticResizeCorrelation();
    if (stage === 'frame') vi.runOnlyPendingTimers();
    subject.detach();
    subject.detach();
    expect(vi.getTimerCount()).toBe(0);
    expect(frames.size).toBe(0);
    expect(subject.resizeDifferenceNow()).toBe(0);
  });

  it.each([false, true])('a superseded callback cannot clear a new stamp (reattach=%s)', (reattach) => {
    const subject = observer();
    subject.stampSyntheticResizeCorrelation();
    vi.runOnlyPendingTimers();
    const oldCallback = [...frames.values()][0];
    if (reattach) { subject.detach(); subject.attach(); }
    subject.stampSyntheticResizeCorrelation();
    expect(frames.size).toBe(0);
    // Even a callback already dispatched by the browser belongs to its old generation.
    oldCallback(performance.now());
    expect(subject.resizeDifferenceNow()).toBe(1);
    vi.runOnlyPendingTimers();
    flushFrames();
    expect(subject.resizeDifferenceNow()).toBe(0);
    subject.detach();
  });
});
