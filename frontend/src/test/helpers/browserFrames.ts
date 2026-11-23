// Frame-stepping helpers for the real-Chromium `browser` vitest project.
// happy-dom suites fake time; these tests instead wait on the real engine —
// a frame at a time — for ResizeObserver deliveries, scroll events, and
// layout to settle.
import { flushSync } from 'svelte';
import { onTestFinished } from 'vitest';
import type { SmoothingClock } from '../../lib/markdown/smoothing/PerItemSmoother';
import { __setSmoothingClockForTest } from '../../lib/stores/thread.svelte';

export function raf(): Promise<void> {
  return new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
}

/**
 * How long `waitFor` waits by default: far above any settle on a loaded
 * runner, and below the suite's hang timeout so the label names the wait.
 */
const SETTLE_TIMEOUT_MS = 20_000;

// Poll an observable predicate once a frame until it holds. The bound is
// wall-clock time, not a frame count: work that yields between frames (a
// chunked scan, a measurement pass) takes more frames on a slow runner, and
// a frame budget would fail it there.
export async function waitFor(
  predicate: () => boolean,
  label: string,
  timeoutMs = SETTLE_TIMEOUT_MS,
): Promise<void> {
  const deadline = performance.now() + timeoutMs;
  for (;;) {
    if (predicate()) return;
    if (performance.now() >= deadline) throw new Error(`timed out after ${timeoutMs} ms waiting for: ${label}`);
    await raf();
  }
}

// Assert that a predicate holds within `frames` frames: for behavior whose
// claim is a frame count (an install inside a presentation ceiling), not
// for waiting on a settle.
export async function withinFrames(
  predicate: () => boolean,
  label: string,
  frames: number,
): Promise<void> {
  for (let i = 0; i < frames; i++) {
    if (predicate()) return;
    await raf();
  }
  if (predicate()) return;
  throw new Error(`not within ${frames} frames: ${label}`);
}

export function wait(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// One frame of a 60 Hz display.
const DISPLAY_FRAME_MS = 1000 / 60;

/**
 * Runs this test's streaming reveal as fast as the page can take it, with
 * the pace of a 60 Hz display: each smoother tick runs on the next task
 * instead of the next animation frame, and its clock moves one 60 Hz frame
 * per tick. Each tick reveals what a 60 Hz frame earns, so the reveal
 * takes the same path of appends as on a display, without waiting out
 * the display's time. `onTick` listeners run after each tick has rendered,
 * so a test can check every state the reveal passes through. Call before
 * the pane mounts; the clock is removed when the test finishes.
 */
export function revealWithoutWaitingForFrames(): { onTick(listener: () => void): void } {
  const listeners: (() => void)[] = [];
  let now = performance.now();
  let nextHandle = 1;
  const pending = new Map<number, (frameTimestamp?: number) => void>();
  const channel = new MessageChannel();
  channel.port1.onmessage = (event: MessageEvent<number>) => {
    const callback = pending.get(event.data);
    if (callback === undefined) return;
    pending.delete(event.data);
    now += DISPLAY_FRAME_MS;
    callback(now);
    if (listeners.length === 0) return;
    flushSync();
    for (const listener of listeners) listener();
  };
  const clock: SmoothingClock = {
    now: () => now,
    schedule: (callback) => {
      const handle = nextHandle++;
      pending.set(handle, callback);
      channel.port2.postMessage(handle);
      return handle;
    },
    cancel: (handle) => { pending.delete(handle); },
  };
  __setSmoothingClockForTest(clock);
  onTestFinished(() => {
    __setSmoothingClockForTest(undefined);
    channel.port1.close();
  });
  return { onTick: (listener) => { listeners.push(listener); } };
}
